package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

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

func TestACardThatSupersedesAPickClosesTheOldPostWithItsOutcome(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	var text map[string]string
	if err := json.Unmarshal(l.do(opDMStatus, map[string]any{"text": "Merge #41 now? Reply A to merge or B to hold."}).Body, &text); err != nil {
		t.Fatal(err)
	}
	id, err := loadIdentity(l.ctx, l.srv.DB())
	if err != nil {
		t.Fatal(err)
	}
	pick := id.permalink(id.DMChannelID, text["ts"], "")
	elsewhere := mergeCard()
	elsewhere.Supersedes = []string{"https://example.slack.com/archives/C0OTHER123/p1800000000000100"}
	if _, err := invoke(l.ctx, l.deps, testSession, "/any", opDMStatus, mustJSON(map[string]any{"card": elsewhere})); err == nil || !strings.Contains(err.Error(), "only the bot's earlier posts in your DM") {
		t.Fatalf("a card superseding a post outside the DM: %v", err)
	}
	card := mergeCard()
	card.Supersedes = []string{pick}
	c := postWaiting(t, l, card)
	pickEdits := func() []string {
		var out []string
		for _, e := range l.slack.edited() {
			if e.ts == text["ts"] {
				out = append(out, e.text)
			}
		}
		return out
	}
	if got := pickEdits(); len(got) != 1 || !strings.Contains(got[0], "is asked again as a card with buttons") || !strings.Contains(got[0], c.url()) {
		t.Fatalf("pick edits after the card posted = %q, want one pointing to the card", got)
	}

	reply := id.permalink(id.DMChannelID, "1800000009.000100", "")
	var res cardAnswerResult
	if err := json.Unmarshal(l.do(opCardAnswer, map[string]any{"waits": "step-3:merge", "url": c.url(), "label": "Hold", "via": reply}).Body, &res); err != nil || !res.Answered {
		t.Fatalf("relayed answer = %+v, %v; want it answered", res, err)
	}
	if got := l.inject(agent.TopLevel); !strings.Contains(got, "Answer relayed from") || !strings.Contains(got, "on Slack message "+reply) {
		t.Fatalf("directive = %q, want the relayed answer named with its Slack message", got)
	}
	if edits := editsOf(l.slack.edited(), c.ts); len(edits) == 0 || !strings.Contains(edits[len(edits)-1], reply+"|Slack") {
		t.Fatalf("card edits %v, want the answer linked to the Slack message it came from", edits)
	}
	if got := pickEdits(); len(got) != 2 || !strings.Contains(got[1], "answered *Hold*") || !strings.Contains(got[1], c.url()) {
		t.Fatalf("pick edits after the answer = %q, want the outcome linked to the card", got)
	}
}

func TestAWithdrawnCardShowsItsReasonInPlaceOfTheButtonsOnce(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	c := postWaiting(t, l, mergeCard())
	withdraw := map[string]any{"waits": "step-3:merge", "url": c.url(), "label": "superseded by step 4", "withdrawn": true, "via": "http://127.0.0.1:7000/p/asks--ab12"}

	if _, err := invoke(l.ctx, l.deps, testSession, "/any", opCardAnswer, mustJSON(map[string]any{
		"waits": "step-3:merge", "url": c.url(), "label": "superseded", "withdrawn": true, "settled": true, "via": "http://127.0.0.1:7000/p/asks--ab12",
	})); err == nil || !strings.Contains(err.Error(), "withdrawn takes the reason as its label") {
		t.Fatalf("a withdrawal that also settles: %v", err)
	}
	var res cardAnswerResult
	if err := json.Unmarshal(l.do(opCardAnswer, withdraw).Body, &res); err != nil || !res.Answered {
		t.Fatalf("withdraw = %+v, %v; want it answered", res, err)
	}
	if got := l.inject(agent.TopLevel); got != "" {
		t.Fatalf("a withdrawn card steered the session: %q", got)
	}
	edits := editsOf(l.slack.edited(), c.ts)
	if len(edits) == 0 || !strings.Contains(edits[len(edits)-1], "*Withdrawn:* superseded by step 4") ||
		!strings.Contains(edits[len(edits)-1], "Merge #41 now?") || strings.Contains(edits[len(edits)-1], cardWaitAction) {
		t.Fatalf("card edits %v, want the title kept and the reason in place of the buttons", edits)
	}
	if got := l.slack.reactedTo(c.ts); !slices.Equal(got, []string{cardWithdrawnReaction}) {
		t.Fatalf("reactions on the card = %q, want the withdrawn one", got)
	}
	if res := answerOnBoard(t, l, c, "Merge"); res.Answered {
		t.Fatalf("an answer after the card was withdrawn = %+v, want it refused", res)
	}
	if err := json.Unmarshal(l.do(opCardAnswer, withdraw).Body, &res); err != nil || res.Answered {
		t.Fatalf("a second withdrawal = %+v, %v; want it refused", res, err)
	}
	if got, n := l.slack.reactedTo(c.ts), len(editsOf(l.slack.edited(), c.ts)); len(got) != 1 || n != len(edits) {
		t.Fatalf("after a repeat, reactions %q and %d edits, want the card left as it was", got, n)
	}
}

func TestAnAnsweredCardGetsTheAnsweredReaction(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	c := postWaiting(t, l, mergeCard())
	if res := answerOnBoard(t, l, c, "Merge"); !res.Answered {
		t.Fatalf("board answer = %+v, want it answered", res)
	}
	if got := l.slack.reactedTo(c.ts); !slices.Equal(got, []string{cardAnsweredReaction}) {
		t.Fatalf("reactions on the card = %q, want the answered one", got)
	}
}

func TestASettledCardShowsItsOutcomeAndTellsNoSession(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	c := postWaiting(t, l, mergeCard())
	settle := map[string]any{"waits": "step-3:merge", "url": c.url(), "label": "Landed on dev as 5480445", "other": true, "settled": true, "via": "https://github.com/acme/app/pull/41"}

	if _, err := invoke(l.ctx, l.deps, testSession, "/any", opCardAnswer, mustJSON(map[string]any{
		"waits": "step-3:merge", "url": c.url(), "label": "Merge", "settled": true, "via": "https://github.com/acme/app/pull/41",
	})); err == nil || !strings.Contains(err.Error(), "settled takes a written-in outcome") {
		t.Fatalf("a settled answer naming an option: %v", err)
	}
	var res cardAnswerResult
	if err := json.Unmarshal(l.do(opCardAnswer, settle).Body, &res); err != nil || !res.Answered {
		t.Fatalf("settle = %+v, %v; want it answered", res, err)
	}
	if got := l.inject(agent.TopLevel); got != "" {
		t.Fatalf("a settled card steered the session: %q", got)
	}
	if payloads := strings.Join(l.channelPayloads(), "\n"); strings.Contains(payloads, "Landed on dev") {
		t.Fatalf("a settled card reached the channel: %s", payloads)
	}
	if edits := editsOf(l.slack.edited(), c.ts); len(edits) == 0 || !strings.Contains(edits[len(edits)-1], "https://github.com/acme/app/pull/41|Landed on dev as 5480445") ||
		strings.Contains(edits[len(edits)-1], cardWaitAction) {
		t.Fatalf("card edits %v, want the outcome linked in place of the buttons", edits)
	}
	if res := answerOnBoard(t, l, c, "Merge"); res.Answered {
		t.Fatalf("an answer after the card settled = %+v, want it refused", res)
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

func mirroredCard(t *testing.T, l *liveDaemon, body map[string]any) postedCard {
	t.Helper()
	body["mirrored"] = true
	var res map[string]string
	if err := json.Unmarshal(l.do(opDMStatus, body).Body, &res); err != nil {
		t.Fatal(err)
	}
	id, err := loadIdentity(l.ctx, l.srv.DB())
	if err != nil {
		t.Fatal(err)
	}
	return postedCard{id: id, thread: res["thread_ts"], ts: res["ts"]}
}

func (l *liveDaemon) postedAt(t *testing.T, ts string) fakePost {
	t.Helper()
	sent := l.slack.sent()
	i := slices.IndexFunc(sent, func(p fakePost) bool { return p.ts == ts })
	if i < 0 {
		t.Fatalf("no post at %s in %+v", ts, sent)
	}
	return sent[i]
}

func TestABoardCardReachesSlackWithItsImageAndButtonsAndATapAnswersTheBoard(t *testing.T) {
	calls := fakePresent(t)
	l := startDaemon(t)
	loggedIn(t, l)
	card := mergeCard()
	c := mirroredCard(t, l, map[string]any{"card": card, "files": []string{writeFile(t, "Feed first.png", pngBytes)}})

	post := l.postedAt(t, c.ts)
	if ids := imageIDs(t, post.blocks); len(ids) != 1 {
		t.Fatalf("image blocks %v in %s, want the card's one image", ids, post.blocks)
	}
	if !strings.Contains(post.blocks, "Feed first.png") {
		t.Fatalf("blocks %s lack the image's title", post.blocks)
	}
	for _, button := range []string{"Hold (Recommended)", "Merge", cardOtherLabel, cardExplainLabel} {
		if !strings.Contains(post.blocks, button) {
			t.Fatalf("blocks %s lack the %s button", post.blocks, button)
		}
	}
	if got := presentCalls(t, calls); got != "" {
		t.Fatalf("a card from the board told the board it was posted: %q", got)
	}

	l.tap(c.id, cardActionPush{User: testUser, Channel: c.id.DMChannelID, MessageTS: c.ts, ThreadTS: c.thread,
		ActionID: cardWaitAction + "0:" + card.Waits, Value: string(mustJSON(askTap{ID: card.Waits, Label: "Hold", Question: card.Title})),
		ActionTS: fmt.Sprintf("%d.000100", time.Now().Unix()), Seq: "001800000100000-abcd0500"})
	got := presentCalls(t, calls)
	if !strings.Contains(got, "slack-sync answered") || !strings.Contains(got, `"label":"Hold"`) || !strings.Contains(got, `"waits":"`+card.Waits+`"`) {
		t.Fatalf("cc-present calls after the tap %q, want the answer sent to the board", got)
	}
	edits := slices.DeleteFunc(l.slack.edited(), func(e fakeEdit) bool { return e.ts != c.ts })
	if len(edits) == 0 || len(imageIDs(t, edits[len(edits)-1].blocks)) != 0 {
		t.Fatalf("edits %+v, want the tapped card to collapse past its image", edits)
	}
}

func TestOnlyACardFromTheBoardCarriesImages(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	png := writeFile(t, "a.png", pngBytes)
	card := mergeCard()
	for name, body := range map[string]map[string]any{
		"a hand-posted question":  {"card": card, "files": []string{png}},
		"a status card":           {"card": statusCard{Status: "info", Title: "Landed"}, "mirrored": true, "files": []string{png}},
		"a file that is no image": {"card": card, "mirrored": true, "files": []string{writeFile(t, "a.txt", "plain words")}},
	} {
		if _, err := invoke(l.ctx, l.deps, testSession, "/any", opDMStatus, mustJSON(body)); err == nil {
			t.Errorf("%s: dm-status took it", name)
		}
	}
}

func TestABoardQuestionPostsWithNoRecommendationOrNoPresetAnswers(t *testing.T) {
	fakePresent(t)
	l := startDaemon(t)
	loggedIn(t, l)
	unrecommended := mirroredCard(t, l, map[string]any{"card": statusCard{Status: "needs_you", Title: "Which one?", Waits: "pick", Options: []string{"This", "That"}}})
	blocks := l.postedAt(t, unrecommended.ts).blocks
	if strings.Contains(blocks, cardRecommended) || !strings.Contains(blocks, "This") || !strings.Contains(blocks, "That") {
		t.Fatalf("blocks %s, want both answers and neither marked recommended", blocks)
	}
	var answer cardAnswerResult
	if err := json.Unmarshal(l.do(opCardAnswer, map[string]any{"waits": "pick", "url": unrecommended.url(), "label": "That", "via": "http://127.0.0.1:7000/p/asks--ab12"}).Body, &answer); err != nil {
		t.Fatal(err)
	}
	if !answer.Answered {
		t.Fatalf("board answer = %+v, want it to win", answer)
	}

	written := mirroredCard(t, l, map[string]any{"card": statusCard{Status: "needs_you", Title: "What should the title say?", Waits: "title"}})
	blocks = l.postedAt(t, written.ts).blocks
	if !strings.Contains(blocks, cardAnswerLabel) || !strings.Contains(blocks, cardWriteHint) || strings.Contains(blocks, `"text":"`+cardOtherLabel+`"`) {
		t.Fatalf("blocks %s, want one Answer button and its hint", blocks)
	}

	handPosted := statusCard{Status: "needs_you", Title: "Which one?", Waits: "pick-2", Options: []string{"This", "That"}}
	if _, err := invoke(l.ctx, l.deps, testSession, "/any", opDMStatus, mustJSON(map[string]any{"card": handPosted})); err == nil {
		t.Fatal("a hand-posted question with no recommendation was taken")
	}
}

func TestAReplyAboutAWaitingCardLandsInThatCardsThread(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	c := postWaiting(t, l, mergeCard())
	l.do(opDMStatus, map[string]any{"text": "Merging lands #41 before the freeze; holding waits for Monday.", "about": "step-3:merge"})
	posts := l.slack.sent()
	if got := posts[len(posts)-1]; got.threadTS != c.ts || !strings.HasPrefix(got.text, "Merging lands #41") {
		t.Fatalf("reply = %+v, want it in the card's thread %s", got, c.ts)
	}
	raw := mustJSON(map[string]any{"text": "and one more thing", "about": "step-9:nothing"})
	if _, err := invoke(l.ctx, l.deps, testSession, "/any", opDMStatus, raw); err == nil || !strings.Contains(err.Error(), `no waiting card "step-9:nothing"`) {
		t.Fatalf("a reply about an unknown card: %v", err)
	}
}

func TestAMirroredCardLandsAtTheDMRootAndARedrawEditsIt(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	c := mirroredCard(t, l, map[string]any{"card": mergeCard()})
	if post := l.postedAt(t, c.ts); post.threadTS != "" || c.thread != c.ts {
		t.Fatalf("mirrored card = %+v at thread %s, want it at the DM root with its own thread", post, c.thread)
	}
	redrawn := mergeCard()
	redrawn.Title = "Merge #41 before the freeze?"
	again := mirroredCard(t, l, map[string]any{"card": redrawn, "thread_ts": c.thread})
	if again.ts != c.ts || again.thread != c.thread || len(l.slack.sent()) != 1 || len(editsOf(l.slack.edited(), c.ts)) != 1 {
		t.Fatalf("a redraw gave %+v over %+v with posts %+v: want the same message edited", again, c, l.slack.sent())
	}
}

func TestNothingPostsIntoAnOldStatusThread(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	id, err := loadIdentity(l.ctx, l.srv.DB())
	if err != nil {
		t.Fatal(err)
	}
	const root, nested = "1800000000.000100", "1800000000.000200"
	l.do(opDMStatus, map[string]any{"text": "the old status thread", "thread_ts": root})
	card := mergeCard()
	waiting := &liveMessage{TS: nested, Card: card}
	l.app.cards.mu.Lock()
	l.app.cards.threads[threadKey(id.DMChannelID, root)] = &liveThread{
		Channel: id.DMChannelID, Thread: root, Root: &liveMessage{TS: root}, Reply: waiting,
		Waiting: map[string]*liveMessage{card.Waits: waiting}, Answers: map[string]askAnswer{"earlier": {Label: "Go"}}, Updated: time.Now(),
	}
	l.app.cards.mu.Unlock()
	before := len(l.slack.sent())

	c := postWaiting(t, l, card)
	l.do(opDMStatus, map[string]string{"text": "an update"})
	l.do(opDMStatus, cardBody("Landed"))
	for _, post := range l.slack.sent()[before:] {
		if post.threadTS != "" {
			t.Fatalf("post %+v, want every message at the DM root", post)
		}
	}
	if c.thread == root || !slices.ContainsFunc(l.slack.deleted(), func(d fakeEdit) bool { return d.ts == nested }) {
		t.Fatalf("a redrawn card landed at %+v and deleted %v, want it moved out of the old thread", c, l.slack.deleted())
	}
	if _, done := l.app.cards.answer(id.DMChannelID, c.thread, "earlier"); !done {
		t.Fatal("the moved card lost the answers its old thread held")
	}
	if edits := l.slack.edited(); len(edits) != 0 {
		t.Fatalf("edits = %+v, want no edit of a message left in the old thread", edits)
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

func TestAHandPostedQuestionCardIsPostedWithAWarningNamingTheBoard(t *testing.T) {
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
	if res["ts"] == "" || !strings.Contains(res["warning"], "cc-present update-block") {
		t.Fatalf("a hand-posted question card got %v, want it posted with a warning naming cc-present update-block", res)
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

func TestPermissionCardTapNamesItsGrantOrRevokeLabel(t *testing.T) {
	options := []string{"Yes", "No"}
	for _, c := range []struct {
		tap  askTap
		want string
	}{
		{askTap{ID: "slack:C0APPFOLLOW:1791859635.513139:0a1b2c3d4e5f", Label: "Yes", Options: options}, "slack-grant:C0APPFOLLOW/1791859635.513139"},
		{askTap{ID: "slack:U0PRIYA0001::0a1b2c3d4e5f", Label: "Yes", Options: options}, "slack-grant:U0PRIYA0001"},
		{askTap{ID: "slack:C0APPFOLLOW:1791859635.513139:0a1b2c3d4e5f", Label: "No", Options: options}, "slack-revoke:C0APPFOLLOW/1791859635.513139"},
		{askTap{ID: "slack:C0APPFOLLOW:1791859635.513139:0a1b2c3d4e5f", Label: "No", Options: []string{"No", "Yes"}}, "slack-revoke:C0APPFOLLOW/1791859635.513139"},
		{askTap{ID: "slack:C0APPFOLLOW:1791859635.513139:0a1b2c3d4e5f", Label: "Post it", Options: []string{"Post it", "Hold"}}, ""},
		{askTap{ID: "slack:C0APPFOLLOW:1791859635.513139:0a1b2c3d4e5f", Label: "only after I review it", Options: options, Other: true}, ""},
		{askTap{ID: "step-3:merge", Label: "Merge", Options: []string{"Merge", "Hold"}}, ""},
	} {
		if got := slackLabel(c.tap); got != c.want {
			t.Errorf("slackLabel(%+v) = %q, want %q", c.tap, got, c.want)
		}
	}
}
