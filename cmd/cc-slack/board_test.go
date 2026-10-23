package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/slack-go/slack"
	"github.com/yasyf/cc-interact/agent"
)

func fakePresent(t *testing.T) string {
	t.Helper()
	bin := t.TempDir()
	calls := filepath.Join(bin, "calls")
	body := "#!/bin/sh\n{ echo \"$CLAUDE_CODE_SESSION_ID $*\"; cat; echo; } >> '" + calls + "'\n"
	if err := os.WriteFile(filepath.Join(bin, presentBinary), []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return calls
}

func presentCalls(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func mergeCard() statusCard {
	return statusCard{Status: "needs_you", Title: "Merge #41 now?", Body: "CI is green.", Options: []string{"Merge", "Hold"}, Recommended: "Hold", Waits: "step-3:merge"}
}

type postedCard struct {
	id     identity
	thread string
	ts     string
}

func postWaiting(t *testing.T, l *liveDaemon, card statusCard) postedCard {
	t.Helper()
	var res map[string]string
	if err := json.Unmarshal(l.do(opDMStatus, map[string]any{"card": card}).Body, &res); err != nil {
		t.Fatal(err)
	}
	id, err := loadIdentity(l.ctx, l.srv.DB())
	if err != nil {
		t.Fatal(err)
	}
	if res["ts"] == "" || res["permalink"] != id.permalink(id.DMChannelID, res["ts"], res["thread_ts"]) {
		t.Fatalf("dm-status result %v, want the card's ts and permalink", res)
	}
	return postedCard{id: id, thread: res["thread_ts"], ts: res["ts"]}
}

func (c postedCard) url() string { return c.id.permalink(c.id.DMChannelID, c.ts, c.thread) }

func answerOnBoard(t *testing.T, l *liveDaemon, c postedCard, label string) cardAnswerResult {
	t.Helper()
	var res cardAnswerResult
	body := map[string]any{"waits": "step-3:merge", "url": c.url(), "label": label, "via": "http://127.0.0.1:7000/p/asks--ab12"}
	if err := json.Unmarshal(l.do(opCardAnswer, body).Body, &res); err != nil {
		t.Fatal(err)
	}
	return res
}

func TestBoardAnswerFiresTheOneCaptureAndLaterAnswersLose(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	c := postWaiting(t, l, mergeCard())

	if _, err := invoke(l.ctx, l.deps, testSession, "/any", opCardAnswer, mustJSON(map[string]any{
		"waits": "step-3:merge", "url": c.url(), "label": "Ship", "via": "http://127.0.0.1:7000/p/asks--ab12",
	})); err == nil || !strings.Contains(err.Error(), "not one of waiting card") {
		t.Fatalf("a board answer outside the options: %v", err)
	}
	if res := answerOnBoard(t, l, c, "Hold"); !res.Answered || res.Label != "Hold" {
		t.Fatalf("first board answer = %+v, want it to win", res)
	}
	got := l.inject(agent.TopLevel)
	if !strings.Contains(got, "Board answer from") || !strings.Contains(got, `answered the waiting ask "step-3:merge" with "Hold"`) ||
		!strings.Contains(got, "on the cc-present board http://127.0.0.1:7000/p/asks--ab12, which answers Slack card ") {
		t.Fatalf("directive = %q, want the board answer and its capture", got)
	}
	payloads := strings.Join(l.channelPayloads(), "\n")
	for _, part := range []string{`"waits":true`, `"label":"Hold"`, `"via":"http://127.0.0.1:7000/p/asks--ab12"`, `"capture":`} {
		if !strings.Contains(payloads, part) {
			t.Fatalf("channel payloads %s lack %s", payloads, part)
		}
	}
	if edits := editsOf(l.slack.edited(), c.ts); len(edits) == 0 || !strings.Contains(edits[len(edits)-1], "✓ *Hold* · answered on ") ||
		!strings.Contains(edits[len(edits)-1], "http://127.0.0.1:7000/p/asks--ab12|the board") || strings.Contains(edits[len(edits)-1], cardWaitAction) {
		t.Fatalf("card edits %v, want the board's answer linked in place of the buttons", edits)
	}

	push := cardActionPush{User: testUser, Channel: c.id.DMChannelID, MessageTS: c.ts, ThreadTS: c.thread,
		ActionID: cardWaitAction + "1:step-3:merge", Value: string(mustJSON(askTap{ID: "step-3:merge", Label: "Merge", Question: "Merge #41 now?"})),
		ActionTS: fmt.Sprintf("%d.000100", time.Now().Unix()), Seq: "001800000100000-abcd0200"}
	l.tap(c.id, push)
	if got := l.inject(agent.TopLevel); got != "" {
		t.Fatalf("a Slack tap after the board answered steered %q", got)
	}
	if res := answerOnBoard(t, l, c, "Merge"); res.Answered || res.Label != "Hold" {
		t.Fatalf("a second board answer = %+v, want a refusal naming the winner", res)
	}
	if got := l.inject(agent.TopLevel); got != "" {
		t.Fatalf("a losing board answer steered %q", got)
	}
}

func TestWrittenInAnswerFromTheOtherModalAnswersTheCardOnce(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	c := postWaiting(t, l, mergeCard())
	push := func(actionID string, k askTap, seq string) cardActionPush {
		return cardActionPush{User: testUser, Channel: c.id.DMChannelID, MessageTS: c.ts, ThreadTS: c.thread,
			ActionID: actionID, Value: string(mustJSON(k)), ActionTS: fmt.Sprintf("%d.000%s", time.Now().Unix(), seq[len(seq)-3:]), Seq: seq}
	}

	l.tap(c.id, push(cardOtherAction+"step-3:merge", askTap{ID: "step-3:merge", Label: cardOtherLabel, Question: "Merge #41 now?"}, "001800000100000-abcd0301"))
	if got := l.inject(agent.TopLevel); got != "" {
		t.Fatalf("a bare Other tap steered %q; only the modal's written answer answers", got)
	}
	l.tap(c.id, push(cardWroteAction+"step-3:merge", askTap{ID: "step-3:merge", Label: "  ", Question: "Merge #41 now?"}, "001800000100000-abcd0302"))
	if got := l.inject(agent.TopLevel); got != "" {
		t.Fatalf("an empty written answer steered %q", got)
	}
	written := askTap{ID: "step-3:merge", Label: "Merge after the 5pm freeze", Question: "Merge #41 now?", Options: []string{"Hold", "Merge"}}
	l.tap(c.id, push(cardWroteAction+"step-3:merge", written, "001800000100000-abcd0303"))
	got := l.inject(agent.TopLevel)
	if !strings.Contains(got, "Slack answer written in from") || !strings.Contains(got, `with "Merge after the 5pm freeze"`) || !strings.Contains(got, "record the capture") {
		t.Fatalf("directive = %q, want the written answer handed to the lane with its capture", got)
	}
	if !strings.Contains(strings.Join(l.channelPayloads(), "\n"), `"other":true`) {
		t.Fatal("the written answer's slack.action does not say it is an Other")
	}
	if edits := editsOf(l.slack.edited(), c.ts); len(edits) == 0 || !strings.Contains(edits[len(edits)-1], "✓ *Other:* Merge after the 5pm freeze") {
		t.Fatalf("card edits %v, want the written answer on the card", edits)
	}
	l.tap(c.id, push(cardWaitAction+"0:step-3:merge", askTap{ID: "step-3:merge", Label: "Hold", Question: "Merge #41 now?"}, "001800000100000-abcd0304"))
	if got := l.inject(agent.TopLevel); got != "" {
		t.Fatalf("a tap after the written answer steered %q", got)
	}
}

func TestReusedOpenWaitsIDEditsTheCardInPlace(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	c := postWaiting(t, l, mergeCard())
	posts := len(l.slack.sent())

	redrawn := mergeCard()
	redrawn.Title = "Merge #41 before the freeze?"
	c2 := postWaiting(t, l, redrawn)
	if c2.ts != c.ts || len(l.slack.sent()) != posts {
		t.Fatalf("redraw posted %d new messages at %s, want the card at %s edited in place", len(l.slack.sent())-posts, c2.ts, c.ts)
	}
	if edits := editsOf(l.slack.edited(), c.ts); len(edits) == 0 || !strings.Contains(edits[len(edits)-1], "Merge #41 before the freeze?") {
		t.Fatalf("card edits %v, want the new question drawn over the old card", edits)
	}
	answerOnBoard(t, l, c2, "Merge")
	raw := mustJSON(map[string]any{"card": redrawn})
	if _, err := invoke(l.ctx, l.deps, testSession, "/any", opDMStatus, raw); err == nil || !strings.Contains(err.Error(), `was answered with "Merge"`) {
		t.Fatalf("re-asking an answered card: %v", err)
	}
}

func TestWaitingCardsTellTheBoardOnPostAndOnASlackAnswer(t *testing.T) {
	calls := fakePresent(t)
	l := startDaemon(t)
	loggedIn(t, l)
	l.do(opDMStatus, map[string]any{"card": mergeCard(), "mirrored": true})
	if got := presentCalls(t, calls); got != "" {
		t.Fatalf("a card already on the board told it again: %q", got)
	}
	l.do(opDMStatus, map[string]any{"card": statusCard{Status: "info", Title: "Landed"}})
	if got := presentCalls(t, calls); got != "" {
		t.Fatalf("a card that waits on nothing told the board: %q", got)
	}
	card := mergeCard()
	card.Waits = "step-4:deploy"
	card.Refs = []cardLink{{URL: "https://github.com/acme/app/pull/41"}, {URL: "https://buildkite.com/acme/app/builds/7"}}
	c := postWaiting(t, l, card)
	got := presentCalls(t, calls)
	for _, part := range []string{
		testSession + " slack-sync posted --session " + testSession, `"waits":"step-4:deploy"`, `"options":["Hold","Merge"]`,
		`"prs":[{"repo":"acme/app","number":41}]`,
		`"recommended":"Hold"`, `"permalink":` + string(mustJSON(c.id.permalink(c.id.DMChannelID, c.ts, c.thread))),
	} {
		if !strings.Contains(got, part) {
			t.Fatalf("cc-present calls %q lack %s", got, part)
		}
	}

	push := cardActionPush{User: testUser, Channel: c.id.DMChannelID, MessageTS: c.ts, ThreadTS: c.thread,
		ActionID: cardWaitAction + "0:step-4:deploy", Value: string(mustJSON(askTap{ID: "step-4:deploy", Label: "Hold", Question: card.Title})),
		ActionTS: fmt.Sprintf("%d.000100", time.Now().Unix()), Seq: "001800000100000-abcd0400"}
	l.tap(c.id, push)
	got = strings.TrimPrefix(presentCalls(t, calls), got)
	if !strings.Contains(got, testSession+" slack-sync answered --session "+testSession) || !strings.Contains(got, `"waits":"step-4:deploy"`) || !strings.Contains(got, `"label":"Hold"`) {
		t.Fatalf("cc-present calls after the tap %q, want the answer sent to the board", got)
	}
	card.Waits, card.Refs = "step-5:release", nil
	c5 := postWaiting(t, l, card)
	before := presentCalls(t, calls)
	l.do(opCardAnswer, map[string]any{"waits": "step-5:release", "url": c5.url(), "label": "Merge", "via": "http://127.0.0.1:7000/p/asks--ab12"})
	if after := presentCalls(t, calls); after != before {
		t.Fatalf("a board answer echoed back to the board: %q", strings.TrimPrefix(after, before))
	}
}

func TestAReplyAboutAWaitingCardOpensWithALinkBackToIt(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	c := postWaiting(t, l, mergeCard())
	l.do(opDMStatus, map[string]any{"text": "Merging lands #41 before the freeze; holding waits for Monday.", "about": "step-3:merge"})
	posts := l.slack.sent()
	want := "<" + c.id.permalink(c.id.DMChannelID, c.ts, c.thread) + "|this card> Merging lands #41"
	if got := posts[len(posts)-1].text; !strings.HasPrefix(got, want) {
		t.Fatalf("reply = %q, want it to open with %q", got, want)
	}
	explain := slack.NewButtonBlockElement(cardExplainAction+"merge:41:abc1234", "{}", slack.NewTextBlockObject(slack.PlainTextType, cardExplainLabel, false, false))
	prCard := slack.Message{Msg: slack.Msg{Timestamp: "1800000003.000100", ThreadTimestamp: c.thread, Text: "Merge PR 41?",
		Blocks: slack.Blocks{BlockSet: []slack.Block{slack.NewActionBlock("card-actions", explain)}}}}
	l.slack.add(c.id.DMChannelID, prCard)
	l.do(opDMStatus, map[string]any{"text": "The PR card asks whether to land PR 41.", "about": "merge:41:abc1234"})
	posts = l.slack.sent()
	want = "<" + c.id.permalink(c.id.DMChannelID, prCard.Timestamp, c.thread) + "|this card> The PR card"
	if got := posts[len(posts)-1].text; !strings.HasPrefix(got, want) {
		t.Fatalf("reply about a card another app posted = %q, want it to open with %q", got, want)
	}
	raw := mustJSON(map[string]any{"text": "and one more thing", "about": "step-9:nothing"})
	if _, err := invoke(l.ctx, l.deps, testSession, "/any", opDMStatus, raw); err == nil || !strings.Contains(err.Error(), `no waiting card "step-9:nothing"`) {
		t.Fatalf("a reply about an unknown card: %v", err)
	}
}

func TestAnAnswerDrawnForAnEarlierQuestionDoesNotAnswerTheRedrawnCard(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	c := postWaiting(t, l, mergeCard())
	redrawn := mergeCard()
	redrawn.Title = "Merge #41 before the freeze?"
	postWaiting(t, l, redrawn)
	stale := askTap{ID: "step-3:merge", Label: "Merge after lunch", Question: "Merge #41 now?"}
	l.tap(c.id, cardActionPush{User: testUser, Channel: c.id.DMChannelID, MessageTS: c.ts, ThreadTS: c.thread,
		ActionID: cardWroteAction + "step-3:merge", Value: string(mustJSON(stale)), ActionTS: fmt.Sprintf("%d.000500", time.Now().Unix()), Seq: "001800000100000-abcd0500"})
	if got := l.inject(agent.TopLevel); got != "" {
		t.Fatalf("an answer to the old question steered %q", got)
	}
	if res := answerOnBoard(t, l, c, "Hold"); !res.Answered {
		t.Fatalf("the redrawn card's board answer = %+v, want it to win", res)
	}
}

func TestALosingBoardAnswerLearnsTheWinnerWasWrittenIn(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	c := postWaiting(t, l, mergeCard())
	written := askTap{ID: "step-3:merge", Label: "Hold", Question: "Merge #41 now?"}
	l.tap(c.id, cardActionPush{User: testUser, Channel: c.id.DMChannelID, MessageTS: c.ts, ThreadTS: c.thread,
		ActionID: cardWroteAction + "step-3:merge", Value: string(mustJSON(written)), ActionTS: fmt.Sprintf("%d.000600", time.Now().Unix()), Seq: "001800000100000-abcd0600"})
	if res := answerOnBoard(t, l, c, "Merge"); res.Answered || res.Label != "Hold" || !res.Other {
		t.Fatalf("losing board answer = %+v, want the written-in Hold that won", res)
	}
}

func TestOneDMStatusCallPostsACardGivenTextBesideIt(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	before := len(l.slack.sent())
	l.do(opDMStatus, map[string]any{"text": "Merge #41 now?", "card": map[string]any{"waits": "step-3:merge", "options": []string{"Merge (Recommended)", "Hold"}}})
	posts := l.slack.sent()[before:]
	if len(posts) == 0 || !strings.Contains(posts[len(posts)-1].text, "Merge #41 now?") {
		t.Fatalf("posts = %+v, want the card titled by its text", posts)
	}
}

func TestAHandPostedQuestionCardIsPostedWithAWarningNamingCcPresentSend(t *testing.T) {
	fakePresent(t)
	l := startDaemon(t)
	loggedIn(t, l)
	post := func(body map[string]any) map[string]string {
		t.Helper()
		var res map[string]string
		if err := json.Unmarshal(l.do(opDMStatus, body).Body, &res); err != nil {
			t.Fatal(err)
		}
		return res
	}
	res := post(map[string]any{"card": mergeCard()})
	if res["ts"] == "" || !strings.Contains(res["warning"], "cc-present send") {
		t.Fatalf("a hand-posted question card got %v, want it posted with a warning naming cc-present send", res)
	}
	mirrored := mergeCard()
	mirrored.Waits = "step-4:deploy"
	if res := post(map[string]any{"card": mirrored, "mirrored": true}); res["warning"] != "" {
		t.Fatalf("a card cc-present sent got the warning %q", res["warning"])
	}
	if res := post(map[string]any{"card": statusCard{Status: "info", Title: "Landed"}}); res["warning"] != "" {
		t.Fatalf("a status card that asks nothing got the warning %q", res["warning"])
	}
}
