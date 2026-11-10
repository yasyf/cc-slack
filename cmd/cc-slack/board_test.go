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
	for _, part := range []string{`"waits":true`, `"label":"Hold"`, `"via":"http://127.0.0.1:7000/p/asks--ab12"`} {
		if !strings.Contains(payloads, part) {
			t.Fatalf("channel payloads %s lack %s", payloads, part)
		}
	}
	if strings.Contains(payloads, `"capture":`) {
		t.Fatalf("channel payloads %s carry the capture, which the directive already holds", payloads)
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

func TestARelayedAnswerSettlesACardWhoseSessionHasEnded(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	c := postWaiting(t, l, mergeCard())
	if _, err := l.srv.DB().ExecContext(l.ctx, `UPDATE watches SET status = 'stopped' WHERE kind = 'dm'`); err != nil {
		t.Fatal(err)
	}
	reply := c.id.permalink(c.id.DMChannelID, "1800000009.000100", "")

	var res cardAnswerResult
	if err := json.Unmarshal(l.do(opCardAnswer, map[string]any{"url": c.url(), "label": "hold it until the stack lands", "other": true, "via": reply}).Body, &res); err != nil || !res.Answered {
		t.Fatalf("answer on an ended session's card = %+v, %v; want it settled", res, err)
	}
	if edits := editsOf(l.slack.edited(), c.ts); len(edits) == 0 || !strings.Contains(edits[len(edits)-1], reply+"|hold it until the stack lands") || strings.Contains(edits[len(edits)-1], cardWaitAction) {
		t.Fatalf("card edits %v, want the answer drawn in place of the buttons", edits)
	}
	if got := l.inject(agent.TopLevel); strings.Contains(got, "hold it until the stack lands") {
		t.Fatalf("directive = %q, want no session told of an answer whose asker ended", got)
	}
}

func TestAReplyACardRecordedAsItsAnswerIsNotOwed(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	c := postWaiting(t, l, mergeCard())
	if _, _, err := insertWatch(l.ctx, l.srv.DB(), l.subID, agent.TopLevel, kindChannel, c.id.DMChannelID, "", "1900000000.000000", time.Now()); err != nil {
		t.Fatal(err)
	}
	before, after := formatTS(time.Now().Add(-time.Minute)), formatTS(time.Now().Add(time.Hour))
	for _, ts := range []string{before, after} {
		if err := l.app.owed.owe(l.ctx, owedReply{ThreadTS: c.thread, TS: ts, Text: "1+2"}); err != nil {
			t.Fatal(err)
		}
	}

	var res cardAnswerResult
	if err := json.Unmarshal(l.do(opCardAnswer, map[string]any{"url": c.url(), "label": "1+2: merge it", "other": true, "via": c.id.permalink(c.id.DMChannelID, before, c.thread)}).Body, &res); err != nil || !res.Answered {
		t.Fatalf("answer = %+v, %v; want the card answered", res, err)
	}

	var owed dmOwedResult
	if err := json.Unmarshal(l.do(opDMOwed, nil).Body, &owed); err != nil {
		t.Fatal(err)
	}
	if len(owed.Replies) != 1 || owed.Replies[0].TS != after {
		t.Fatalf("owed %+v, want only the reply after the card's answer", owed.Replies)
	}
	stored, err := l.app.owed.list(l.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 {
		t.Fatalf("stored owed %+v, want the answered reply gone for good", stored)
	}
}

func TestCardAnswerFindsTheCardAtItsPermalinkWhateverIDTheCallerKnowsItBy(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	boardCard := mergeCard()
	boardCard.Waits = "card-rmw8sh2p2"
	c := postWaiting(t, l, boardCard)
	via := "http://127.0.0.1:7000/p/asks--ab12"

	var res cardAnswerResult
	if err := json.Unmarshal(l.do(opCardAnswer, map[string]any{"waits": "rmw8sh2p2", "url": c.url(), "label": "Hold", "via": via}).Body, &res); err != nil || !res.Answered {
		t.Fatalf("answer under the board's id = %+v, %v; want the card at the permalink answered", res, err)
	}
	if got := l.inject(agent.TopLevel); !strings.Contains(got, `answered the waiting ask "card-rmw8sh2p2" with "Hold"`) {
		t.Fatalf("directive = %q, want the answer under the card's own waits id", got)
	}

	unnamed := statusCard{Status: "needs_you", Title: "Release now?", Body: "The build is green.", Options: []string{"Release", "Wait"}, Recommended: "Wait", Waits: "step-5:release"}
	c2 := postWaiting(t, l, unnamed)
	res = cardAnswerResult{}
	if err := json.Unmarshal(l.do(opCardAnswer, map[string]any{"url": c2.url(), "label": "Wait", "via": via}).Body, &res); err != nil || !res.Answered {
		t.Fatalf("answer with no waits id = %+v, %v; want the card at the permalink answered", res, err)
	}

	nowhere := c.id.permalink(c.id.DMChannelID, "1800000099.000100", "")
	if _, err := invoke(l.ctx, l.deps, testSession, "/any", opCardAnswer, mustJSON(map[string]any{"url": nowhere, "label": "Hold", "via": via})); err == nil || !strings.Contains(err.Error(), "no waiting card") {
		t.Fatalf("an answer at a permalink with no waiting card: %v", err)
	}
}

func TestAWithdrawnCardKeepsItsWholeReason(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	c := postWaiting(t, l, mergeCard())
	reason := strings.Repeat("Nothing is owed from you, since the judge decides once. ", 10) + "Last words."
	l.do(opCardAnswer, map[string]any{"waits": "step-3:merge", "url": c.url(), "label": reason, "withdrawn": true, "via": "http://127.0.0.1:7000/p/asks--ab12"})
	edits := editsOf(l.slack.edited(), c.ts)
	if len(edits) == 0 || !strings.Contains(edits[len(edits)-1], "Last words.") {
		t.Fatalf("card edits %v, want the reason whole", edits)
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

func TestReusedOpenWaitsIDEditsTheCardInPlaceAndPostsItAgainBesideIt(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	c := postWaiting(t, l, mergeCard())
	posts := len(l.slack.sent())
	if same := postWaiting(t, l, mergeCard()); same.ts != c.ts || len(l.slack.sent()) != posts {
		t.Fatalf("asking the same card again posted %d new messages, want none", len(l.slack.sent())-posts)
	}

	redrawn := mergeCard()
	redrawn.Title = "Merge #41 before the freeze?"
	c2 := postWaiting(t, l, redrawn)
	if c2.ts != c.ts {
		t.Fatalf("redraw landed at %s, want the card at %s edited in place", c2.ts, c.ts)
	}
	if edits := editsOf(l.slack.edited(), c.ts); len(edits) == 0 || !strings.Contains(edits[len(edits)-1], "Merge #41 before the freeze?") {
		t.Fatalf("card edits %v, want the new question drawn over the old card", edits)
	}
	if sent := l.slack.sent()[posts:]; len(sent) != 1 || sent[0].threadTS != "" || sent[0].text != cardCopyLead || !strings.Contains(sent[0].blocks, "Merge #41 before the freeze?") {
		t.Fatalf("posts after the redraw = %+v, want one copy of the new card at the DM root", sent)
	}
	answerOnBoard(t, l, c2, "Merge")
	raw := mustJSON(map[string]any{"card": redrawn})
	if _, err := invoke(l.ctx, l.deps, testSession, "/any", opDMStatus, raw); err == nil || !strings.Contains(err.Error(), `was answered with "Merge"`) {
		t.Fatalf("re-asking an answered card: %v", err)
	}
}

func TestACopyOfAWaitingCardFollowsItsRedraw(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	c := postWaiting(t, l, mergeCard())
	var thread map[string]string
	if err := json.Unmarshal(l.do(opDMStatus, map[string]any{"text": "About the merge."}).Body, &thread); err != nil {
		t.Fatal(err)
	}
	var copied postResult
	if err := json.Unmarshal(l.do(opCardCopy, map[string]any{"url": c.url(), "thread_ts": thread["thread_ts"]}).Body, &copied); err != nil {
		t.Fatal(err)
	}

	redrawn := mergeCard()
	redrawn.Title = "Merge #41 before the freeze?"
	postWaiting(t, l, redrawn)

	if edits := editsOf(l.slack.edited(), copied.TS); len(edits) != 1 || !strings.Contains(edits[0], "Merge #41 before the freeze?") || !strings.Contains(edits[0], cardCopyAction) {
		t.Fatalf("copy edits = %v, want the copy redrawn with the new question and its buttons", edits)
	}
}

func TestAWithdrawnWaitsIDAskedAgainRevivesTheCardInPlace(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	c := postWaiting(t, l, mergeCard())
	l.do(opCardAnswer, map[string]any{"waits": "step-3:merge", "url": c.url(), "label": "acme/app#41 is closed", "withdrawn": true, "via": c.url()})
	posts, unreacted := len(l.slack.sent()), l.slack.callCount("reactions.remove")

	again := postWaiting(t, l, mergeCard())

	if again.ts != c.ts || again.thread != c.thread || len(l.slack.sent()) != posts+1 {
		t.Fatalf("asking again posted %d new messages at %s, want the card at %s revived in place and one copy of it", len(l.slack.sent())-posts, again.ts, c.ts)
	}
	if copied := l.slack.sent()[posts]; copied.threadTS != "" || copied.text != cardCopyLead || !strings.Contains(copied.blocks, "Merge #41 now?") {
		t.Fatalf("copy = %+v, want the revived card posted again at the DM root", copied)
	}
	if edits := editsOf(l.slack.edited(), c.ts); len(edits) == 0 || strings.Contains(edits[len(edits)-1], "Withdrawn") || !strings.Contains(edits[len(edits)-1], "Merge #41 now?") {
		t.Fatalf("card edits %v, want the question and its buttons back with the withdrawn line gone", edits)
	}
	if l.slack.callCount("reactions.remove") == unreacted {
		t.Fatal("the revived card kept its withdrawn mark")
	}
	if res := answerOnBoard(t, l, again, "Merge"); !res.Answered {
		t.Fatalf("a tap on the revived card was not taken: %+v", res)
	}
	raw := mustJSON(map[string]any{"card": mergeCard()})
	if _, err := invoke(l.ctx, l.deps, testSession, "/any", opDMStatus, raw); err == nil || !strings.Contains(err.Error(), `was answered with "Merge"`) {
		t.Fatalf("re-asking a card he answered: %v", err)
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
	if ids := imageIDs(t, post.blocks); len(ids) != 0 {
		t.Fatalf("first post names files %v before they are shared", ids)
	}
	shares := slices.DeleteFunc(l.slack.sent(), func(p fakePost) bool { return p.threadTS == "" || p.threadTS == c.thread || p.blocks != "" })
	if len(shares) != 1 || shares[0].channelID != c.id.DMChannelID {
		t.Fatalf("shares %+v, want the card's one picture shared into the DM's picture thread, never the card's thread %s", shares, c.thread)
	}
	shown := slices.DeleteFunc(l.slack.edited(), func(e fakeEdit) bool { return e.ts != c.ts })
	if len(shown) == 0 || len(imageIDs(t, shown[0].blocks)) != 1 {
		t.Fatalf("edits %+v, want the card to show its one shared image", shown)
	}
	if !strings.Contains(shown[0].blocks, "Feed first.png") {
		t.Fatalf("blocks %s lack the image's title", shown[0].blocks)
	}
	for _, button := range []string{"Hold (Recommended)", "Merge", cardOtherLabel, cardExplainLabel} {
		if !strings.Contains(shown[0].blocks, button) {
			t.Fatalf("blocks %s lack the %s button", shown[0].blocks, button)
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

func TestAResentCardKeepsItsSharedPicturesAndOnlyNewOnesShareAgain(t *testing.T) {
	fakePresent(t)
	l := startDaemon(t)
	loggedIn(t, l)
	card := mergeCard()
	png := writeFile(t, "Feed first.png", pngBytes)
	c := mirroredCard(t, l, map[string]any{"card": card, "files": []string{png}})

	l.do(opDMStatus, map[string]any{"card": card, "mirrored": true, "files": []string{png}})
	if done := l.slack.completed(); len(done) != 1 {
		t.Fatalf("completed uploads %+v after the same picture came again, want the one share", done)
	}
	edits := slices.DeleteFunc(l.slack.edited(), func(e fakeEdit) bool { return e.ts != c.ts })
	if ids := imageIDs(t, edits[len(edits)-1].blocks); len(ids) != 1 {
		t.Fatalf("image blocks %v after the same picture came again, want it still shown", ids)
	}
	first := imageIDs(t, edits[len(edits)-1].blocks)[0]

	l.do(opDMStatus, map[string]any{"card": card, "mirrored": true, "files": []string{writeFile(t, "Feed first.png", gifBytes)}})
	if done := l.slack.completed(); len(done) != 2 {
		t.Fatalf("completed uploads %+v after a new picture came, want a second share", done)
	}
	edits = slices.DeleteFunc(l.slack.edited(), func(e fakeEdit) bool { return e.ts != c.ts })
	if ids := imageIDs(t, edits[len(edits)-1].blocks); len(ids) != 1 || ids[0] == first {
		t.Fatalf("image blocks %v, want the new picture in place of %s", ids, first)
	}
}

func TestPicturesArrivingAfterATapKeepTheTap(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	var res map[string]string
	if err := json.Unmarshal(l.do(opDMStatus, map[string]any{"card": statusCard{Status: "info", Title: "Status"}}).Body, &res); err != nil {
		t.Fatal(err)
	}
	id, err := loadIdentity(l.ctx, l.srv.DB())
	if err != nil {
		t.Fatal(err)
	}
	thread, ts := res["thread_ts"], res["ts"]
	if !l.app.cards.markReply(id.DMChannelID, thread, ts, func(m *liveMessage) { m.Tapped = "Pause" }) {
		t.Fatalf("no live card at %s", ts)
	}

	l.app.showCardPictures(l.ctx, l.srv.DB(), id, &statusCard{Status: "info", Title: "Status"}, []slack.FileSummary{{ID: "Fa", Title: "a.png"}}, thread, ts)

	tapped := ""
	l.app.cards.markReply(id.DMChannelID, thread, ts, func(m *liveMessage) { tapped = m.Tapped })
	if tapped != "Pause" {
		t.Fatalf("tap = %q after the pictures arrived, want Pause kept", tapped)
	}
}

func TestOnlyAWaitingCardCarriesImages(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	png := writeFile(t, "a.png", pngBytes)
	card := mergeCard()
	for name, body := range map[string]map[string]any{
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

func TestAMirroredCardLandsAtTheDMRootAndARedrawEditsItAndPostsItAgain(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	c := mirroredCard(t, l, map[string]any{"card": mergeCard()})
	if post := l.postedAt(t, c.ts); post.threadTS != "" || c.thread != c.ts {
		t.Fatalf("mirrored card = %+v at thread %s, want it at the DM root with its own thread", post, c.thread)
	}
	redrawn := mergeCard()
	redrawn.Title = "Merge #41 before the freeze?"
	again := mirroredCard(t, l, map[string]any{"card": redrawn, "thread_ts": c.thread})
	sent := l.slack.sent()
	if again.ts != c.ts || again.thread != c.thread || len(sent) != 2 || sent[1].threadTS != "" || sent[1].text != cardCopyLead || len(editsOf(l.slack.edited(), c.ts)) != 1 {
		t.Fatalf("a redraw gave %+v over %+v with posts %+v: want the same message edited and one copy at the DM root", again, c, sent)
	}
}

func portCard(answered postedCard) statusCard {
	card := mergeCard()
	card.Title = "Port, old tree to new: dev #41: Merge it now?"
	card.Body = "CI is green on dev."
	card.Replaces = answered.url()
	return card
}

func TestNewWordsOnAnAnsweredBoardCardRedrawItInPlaceAndAskNothing(t *testing.T) {
	calls := fakePresent(t)
	l := startDaemon(t)
	loggedIn(t, l)
	c := mirroredCard(t, l, map[string]any{"card": mergeCard()})
	answerOnBoard(t, l, c, "Hold")
	l.inject(agent.TopLevel)
	posts, edits, told := len(l.slack.sent()), len(editsOf(l.slack.edited(), c.ts)), presentCalls(t, calls)

	again := mirroredCard(t, l, map[string]any{"card": portCard(c)})

	if again.ts != c.ts || again.thread != c.thread || len(l.slack.sent()) != posts {
		t.Fatalf("new words on an answered card gave %+v and %d new posts, want the card at %s and no post", again, len(l.slack.sent())-posts, c.ts)
	}
	drawn := editsOf(l.slack.edited(), c.ts)
	if len(drawn) != edits+1 {
		t.Fatalf("card edits %v, want one more after the new words", drawn)
	}
	last := drawn[len(drawn)-1]
	if !strings.Contains(last, "Port, old tree to new: dev #41: Merge it now?") || !strings.Contains(last, "✓ *Hold* · answered") || strings.Contains(last, cardWaitAction) {
		t.Fatalf("card redrawn as %s, want the new title over his answer and no buttons", last)
	}
	if got := l.inject(agent.TopLevel); got != "" {
		t.Fatalf("new words on an answered card steered %q", got)
	}
	if got := presentCalls(t, calls); got != told {
		t.Fatalf("new words on an answered card told the board %q", strings.TrimPrefix(got, told))
	}
	if ans, done := l.app.cards.answer(c.id.DMChannelID, c.thread, "step-3:merge"); !done || ans.Label != "Hold" {
		t.Fatalf("answer after new words = %+v, %v; want his Hold kept", ans, done)
	}

	redrawCard(t, l, c)
	if redrawn := editsOf(l.slack.edited(), c.ts); len(redrawn) != edits+2 || !strings.Contains(redrawn[len(redrawn)-1], "Port, old tree to new: dev #41: Merge it now?") ||
		strings.Contains(redrawn[len(redrawn)-1], "Merge #41 now?") || !strings.Contains(redrawn[len(redrawn)-1], "✓ *Hold* · answered") {
		t.Fatalf("card edits %v, want the next redraw to keep the new title over his answer", redrawn)
	}
}

func TestNewWordsOnAWithdrawnBoardCardKeepItWithdrawn(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	c := mirroredCard(t, l, map[string]any{"card": mergeCard()})
	l.do(opCardAnswer, map[string]any{"waits": "step-3:merge", "url": c.url(), "label": "the port card asks it now", "withdrawn": true, "via": c.id.permalink(c.id.DMChannelID, "1800000009.000100", "")})
	posts, edits, unreacted := len(l.slack.sent()), len(editsOf(l.slack.edited(), c.ts)), l.slack.callCount("reactions.remove")

	again := mirroredCard(t, l, map[string]any{"card": portCard(c)})

	drawn := editsOf(l.slack.edited(), c.ts)
	if again.ts != c.ts || len(l.slack.sent()) != posts || len(drawn) != edits+1 {
		t.Fatalf("new words on a withdrawn card gave %+v, %d new posts and edits %v, want one edit in place and no post", again, len(l.slack.sent())-posts, drawn)
	}
	if last := drawn[len(drawn)-1]; !strings.Contains(last, "Port, old tree to new: dev #41: Merge it now?") || !strings.Contains(last, "*Replaced:*") || strings.Contains(last, cardWaitAction) {
		t.Fatalf("card redrawn as %s, want the new title over its replaced line and no buttons", last)
	}
	if ans, done := l.app.cards.answer(c.id.DMChannelID, c.thread, "step-3:merge"); !done || !ans.Withdrawn || l.slack.callCount("reactions.remove") != unreacted {
		t.Fatalf("answer after new words = %+v, %v; want the card still withdrawn with its mark", ans, done)
	}
}

func redrawCard(t *testing.T, l *liveDaemon, c postedCard) {
	t.Helper()
	if _, err := l.app.cards.redraw(l.ctx, l.app.newAPI(c.id.Token), c.id.DMChannelID, c.thread, c.ts, time.Now()); err != nil {
		t.Fatal(err)
	}
}

func handEdit(t *testing.T, l *liveDaemon, c postedCard, blocks ...map[string]any) {
	t.Helper()
	title := map[string]any{"type": "header", "text": map[string]any{"type": "plain_text", "text": "Port, old tree to new: dev #41: Merge it now?"}}
	l.post(opEdit, map[string]any{"channel_id": testUser, "ts": c.ts, "text": "Port, old tree to new: dev #41: Merge it now?", "blocks": append([]map[string]any{title}, blocks...)})
}

func TestAHandEditedCardPostKeepsItsWordsWhenItsCardRedraws(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	c := mirroredCard(t, l, map[string]any{"card": mergeCard()})
	l.do(opCardAnswer, map[string]any{"waits": "step-3:merge", "url": c.url(), "label": "the port card asks it now", "withdrawn": true, "via": c.id.permalink(c.id.DMChannelID, "1800000009.000100", "")})
	if edits := editsOf(l.slack.edited(), c.ts); len(edits) == 0 || !strings.Contains(edits[len(edits)-1], "*Replaced:*") {
		t.Fatalf("card edits %v, want the replaced line drawn", edits)
	}
	handEdit(t, l, c, map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": "Landed on dev."}})
	edits := len(editsOf(l.slack.edited(), c.ts))

	redrawCard(t, l, c)

	if got := editsOf(l.slack.edited(), c.ts); len(got) != edits || strings.Contains(got[len(got)-1], "Replaced:") || !strings.Contains(got[len(got)-1], "Landed on dev.") {
		t.Fatalf("card edits %v after a redraw, want the hand edit left as it is", got)
	}
}

func TestATapOnAHandEditedCardPostDrawsOnTheEditedPost(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	c := mirroredCard(t, l, map[string]any{"card": mergeCard()})
	handEdit(t, l, c, map[string]any{"type": "section", "block_id": cardWaitBlock, "text": map[string]any{"type": "mrkdwn", "text": "Waiting on you."}})

	l.tap(c.id, cardActionPush{User: testUser, Channel: c.id.DMChannelID, MessageTS: c.ts, ThreadTS: c.thread,
		ActionID: cardExplainAction + "step-3:merge", Value: string(mustJSON(askTap{ID: "step-3:merge", Label: cardExplainLabel, Question: "Merge #41 now?"})),
		ActionTS: fmt.Sprintf("%d.000700", time.Now().Unix()), Seq: "001800000100000-abcd0700"})

	edits := editsOf(l.slack.edited(), c.ts)
	if last := edits[len(edits)-1]; !strings.Contains(last, "Port, old tree to new: dev #41: Merge it now?") || !strings.Contains(last, "You asked for more") || strings.Contains(last, "Merge #41 now?") {
		t.Fatalf("card after Explain more = %s, want the hint drawn on the hand-edited post", last)
	}
}

func TestAResendOfABoardCardCCSlackNoLongerHoldsRefusesAClosedPost(t *testing.T) {
	for name, forget := range map[string]func(*liveDaemon, postedCard){
		"its live cards were pruned": func(l *liveDaemon, c postedCard) {
			l.app.cards.mu.Lock()
			delete(l.app.cards.threads, threadKey(c.id.DMChannelID, c.thread))
			l.app.cards.mu.Unlock()
		},
		"its session let go of its thread": func(l *liveDaemon, c postedCard) {
			l.do(opUnfollow, map[string]string{"channel_id": c.id.DMChannelID, "thread_ts": c.thread})
		},
	} {
		t.Run(name, func(t *testing.T) {
			l := startDaemon(t)
			loggedIn(t, l)
			c := mirroredCard(t, l, map[string]any{"card": mergeCard()})
			answerOnBoard(t, l, c, "Hold")
			forget(l, c)
			posts, edits := len(l.slack.sent()), len(l.slack.edited())

			raw := mustJSON(map[string]any{"card": portCard(c), "mirrored": true})
			if _, err := invoke(l.ctx, l.deps, testSession, "/any", opDMStatus, raw); err == nil || !strings.Contains(err.Error(), "no answer left to tap") || !strings.Contains(err.Error(), errAskedAgain.Error()) {
				t.Fatalf("a resend over his answered post: %v, want it refused", err)
			}
			if len(l.slack.sent()) != posts || len(l.slack.edited()) != edits {
				t.Fatal("a refused resend wrote to Slack")
			}
		})
	}
}

func TestAResendOfAnOpenBoardCardCCSlackForgotStillRedrawsIt(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	c := mirroredCard(t, l, map[string]any{"card": mergeCard()})
	l.app.cards.mu.Lock()
	delete(l.app.cards.threads, threadKey(c.id.DMChannelID, c.thread))
	l.app.cards.mu.Unlock()

	again := mirroredCard(t, l, map[string]any{"card": portCard(c)})

	if edits := editsOf(l.slack.edited(), c.ts); again.ts != c.ts || len(edits) != 1 || !strings.Contains(edits[0], "Port, old tree to new: dev #41: Merge it now?") || !strings.Contains(edits[0], cardWaitAction) {
		t.Fatalf("resend of an open card gave %+v with edits %v, want it redrawn in place with its buttons", again, edits)
	}
}

func TestAnAnsweredBoardCardRefusesNewAnswers(t *testing.T) {
	for name, change := range map[string]func(*statusCard){
		"a relabeled option":  func(c *statusCard) { c.Options = []string{"Merge now", "Hold"} },
		"reordered options":   func(c *statusCard) { c.Options = []string{"Hold", "Merge"} },
		"another recommended": func(c *statusCard) { c.Recommended = "Merge" },
		"an added option":     func(c *statusCard) { c.Options = append(c.Options, "Ship") },
		"another PR head":     func(c *statusCard) { c.PRs = []cardPR{{URL: "https://github.com/acme/app/pull/41", Head: "c0ffee1"}} },
	} {
		t.Run(name, func(t *testing.T) {
			l := startDaemon(t)
			loggedIn(t, l)
			first := mergeCard()
			first.PRs = []cardPR{{URL: "https://github.com/acme/app/pull/41", Head: "5480445"}}
			c := mirroredCard(t, l, map[string]any{"card": first})
			answerOnBoard(t, l, c, "Hold")
			posts, edits := len(l.slack.sent()), len(editsOf(l.slack.edited(), c.ts))

			card := portCard(c)
			card.PRs = first.PRs
			change(&card)
			raw := mustJSON(map[string]any{"card": card, "mirrored": true})
			if _, err := invoke(l.ctx, l.deps, testSession, "/any", opDMStatus, raw); err == nil || !strings.Contains(err.Error(), `was answered with "Hold"`) || !strings.Contains(err.Error(), errAskedAgain.Error()) {
				t.Fatalf("%s on an answered card: %v, want it refused", name, err)
			}
			if len(l.slack.sent()) != posts || len(editsOf(l.slack.edited(), c.ts)) != edits {
				t.Fatalf("%s on an answered card wrote to Slack", name)
			}
		})
	}
}

func TestNewWordsOnAnOpenBoardCardStillAskItAgain(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	c := mirroredCard(t, l, map[string]any{"card": mergeCard()})

	again := mirroredCard(t, l, map[string]any{"card": portCard(c)})

	sent, edits := l.slack.sent(), editsOf(l.slack.edited(), c.ts)
	if again.ts != c.ts || len(sent) != 2 || sent[1].text != cardCopyLead || len(edits) != 1 {
		t.Fatalf("new words on an open card gave %+v with posts %+v and edits %v, want it edited in place and one copy beside it", again, sent, edits)
	}
	if !strings.Contains(edits[0], "Port, old tree to new: dev #41: Merge it now?") || !strings.Contains(edits[0], cardWaitAction) {
		t.Fatalf("open card redrawn as %s, want the new title and its buttons", edits[0])
	}
}

func TestAnOldThreadTakesNoNewPostAndACardThereStaysPut(t *testing.T) {
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
	if c.thread != root || c.ts != nested || len(l.slack.deleted()) != 0 {
		t.Fatalf("a redrawn card landed at %+v and deleted %v, want it edited where it is", c, l.slack.deleted())
	}
	if _, done := l.app.cards.answer(id.DMChannelID, c.thread, "earlier"); !done {
		t.Fatal("the card lost the answers its thread held")
	}
	if edits := editsOf(l.slack.edited(), nested); len(edits) != 1 {
		t.Fatalf("edits = %+v, want the one card edited in its thread", edits)
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
	if edits := editsOf(l.slack.edited(), c.ts); len(edits) == 0 || !strings.Contains(edits[len(edits)-1], "this card now asks something else") {
		t.Fatalf("card edits %v, want the card to say the answer reached no one", edits)
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

func TestADriveQuestionCardPostsWithNoBoardWarning(t *testing.T) {
	fakePresent(t)
	l := startDaemon(t)
	loggedIn(t, l)
	post := func(card statusCard) map[string]string {
		t.Helper()
		var res map[string]string
		if err := json.Unmarshal(l.do(opDMStatus, map[string]any{"card": card}).Body, &res); err != nil {
			t.Fatal(err)
		}
		return res
	}
	driven := mergeCard()
	driven.Drive = "checkout-rewrite"
	if res := post(driven); res["ts"] == "" || res["warning"] != "" {
		t.Fatalf("a drive's question card got %v, want it posted with no warning", res)
	}
	undriven := mergeCard()
	undriven.Waits = "step-4:deploy"
	if res := post(undriven); res["ts"] == "" || !strings.Contains(res["warning"], "cc-present update-block") {
		t.Fatalf("a question card naming no drive got %v, want it posted with the board warning", res)
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
