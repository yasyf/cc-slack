package main

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/slack-go/slack"

	"github.com/yasyf/cc-interact/agent"
)

func TestStatusCardRefusesBadShapes(t *testing.T) {
	past := time.Now().Add(-time.Minute)
	ok := statusCard{Status: "needs_you", Title: "Merge the stack?", Options: []string{"merge", "hold"},
		Links: []cardLink{{Label: "PR", URL: "https://github.com/acme/app/pull/1"}}}
	if err := ok.check(time.Now(), false); err != nil {
		t.Fatalf("a well-formed card: %v", err)
	}
	for name, card := range map[string]statusCard{
		"unknown status":         {Status: "urgent", Title: "x"},
		"no title":               {Status: "info", Title: " "},
		"http link":              {Status: "info", Title: "x", Links: []cardLink{{Label: "PR", URL: "http://example.com"}}},
		"unlabeled link":         {Status: "info", Title: "x", Links: []cardLink{{URL: "https://example.com"}}},
		"five options":           {Status: "info", Title: "x", Options: []string{"a", "b", "c", "d", "e"}},
		"long option":            {Status: "info", Title: "x", Options: []string{strings.Repeat("o", cardMaxOptionLen+1)}},
		"option shadows ack":     {Status: "info", Title: "x", Options: []string{cardAckLabel}},
		"repeated option":        {Status: "info", Title: "x", Options: []string{"go", "go"}},
		"ref off github":         {Status: "info", Title: "x", Refs: []cardLink{{URL: "https://gitlab.com/acme/app/-/merge_requests/1"}}},
		"ref to an issue":        {Status: "info", Title: "x", Refs: []cardLink{{URL: "https://github.com/acme/app/issues/1"}}},
		"repeated ask":           {Status: "info", Title: "x", Asks: []cardAsk{{ID: "a", Label: "Go"}, {ID: "a", Label: "Stop"}}},
		"ask id with space":      {Status: "info", Title: "x", Asks: []cardAsk{{ID: "merge it", Label: "Go"}}},
		"expired ask":            {Status: "info", Title: "x", Asks: []cardAsk{{ID: "a", Label: "Go", ExpiresAt: &past}}},
		"unknown ask style":      {Status: "info", Title: "x", Asks: []cardAsk{{ID: "a", Label: "Go", Style: "warning"}}},
		"step past total":        {Status: "info", Title: "x", Progress: &cardProgress{Step: 4, Total: 3}},
		"eleven facts":           {Status: "info", Title: "x", Facts: slices.Repeat([]cardFact{{Label: "k", Value: "v"}}, 11)},
		"unnamed lane":           {Status: "info", Title: "x", Rollup: &cardRollup{Lanes: []rollupLane{{Status: "green"}}}},
		"http rollup pr":         {Status: "info", Title: "x", Rollup: &cardRollup{PRs: []rollupPR{{URL: "http://github.com/acme/app/pull/1"}}}},
		"waits, no options":      {Status: "needs_you", Title: "x", Waits: "step-3:merge"},
		"waits id with space":    {Status: "needs_you", Title: "x", Waits: "merge it", Options: []string{"go"}, Recommended: "go"},
		"waits with asks":        {Status: "needs_you", Title: "x", Waits: "step-3:merge", Options: []string{"go"}, Recommended: "go", Asks: []cardAsk{{ID: "a", Label: "Go"}}},
		"option shadows explain": {Status: "needs_you", Title: "x", Waits: "step-3:merge", Options: []string{"go", cardExplainLabel}, Recommended: "go"},
		"option shadows other":   {Status: "needs_you", Title: "x", Waits: "step-3:merge", Options: []string{"go", cardOtherLabel}, Recommended: "go"},
		"waits, no recommended":  {Status: "needs_you", Title: "x", Waits: "step-3:merge", Options: []string{"go", "stop"}},
		"recommends a stranger":  {Status: "needs_you", Title: "x", Waits: "step-3:merge", Options: []string{"go", "stop"}, Recommended: "wait"},
		"marks it in a label":    {Status: "needs_you", Title: "x", Waits: "step-3:merge", Options: []string{"go (Recommended)", "stop"}, Recommended: "go (Recommended)"},
		"recommends, no waits":   {Status: "needs_you", Title: "x", Options: []string{"go", "stop"}, Recommended: "go"},
	} {
		if err := card.check(time.Now(), false); err == nil {
			t.Errorf("%s: check passed, want a refusal", name)
		}
	}
	if err := (statusCard{Status: "needs_you", Title: "x", Waits: "step-3:merge"}).check(time.Now(), true); err == nil {
		t.Error("a root-only waiting card passed check, want a refusal")
	}
}

func cardButtons(t *testing.T, blocks []slack.Block) []*slack.ButtonBlockElement {
	t.Helper()
	var buttons []*slack.ButtonBlockElement
	for _, b := range blocks {
		if a, ok := b.(*slack.ActionBlock); ok {
			for _, e := range a.Elements.ElementSet {
				button, ok := e.(*slack.ButtonBlockElement)
				if !ok {
					t.Fatalf("action element %T is not a button", e)
				}
				buttons = append(buttons, button)
			}
		}
	}
	return buttons
}

func TestWaitingCardLeadsWithItsRecommendationThenOtherAndExplainMore(t *testing.T) {
	card := statusCard{Status: "needs_you", Title: "Merge #41 now?", Options: []string{"Merge", "Hold"}, Recommended: "Hold", Waits: "step-3:merge"}
	if err := card.check(time.Now(), false); err != nil {
		t.Fatalf("a well-formed waiting card: %v", err)
	}
	at := time.Date(2026, 10, 22, 17, 0, 0, 0, time.UTC)
	blocks := card.replyBlocks(cardView{at: at, now: at})
	buttons := cardButtons(t, blocks)
	var got []string
	for _, b := range buttons {
		got = append(got, b.ActionID+"="+b.Text.Text)
	}
	want := []string{
		cardWaitAction + "0:step-3:merge=Hold (Recommended)", cardWaitAction + "1:step-3:merge=Merge",
		cardOtherAction + "step-3:merge=Other", cardExplainAction + "step-3:merge=Explain more",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("buttons = %q, want the recommendation first, then the other answer, Other, and Explain more", got)
	}
	if buttons[0].Style != slack.StylePrimary || buttons[1].Style != "" {
		t.Fatalf("styles = %q, %q; want only the recommendation primary", buttons[0].Style, buttons[1].Style)
	}
	recommended := cardActionPush{ActionID: buttons[0].ActionID, Value: buttons[0].Value}
	if k, ok := recommended.tap(); !ok || !k.Waits || k.Label != "Hold" || !slices.Equal(k.Options, []string{"Hold", "Merge"}) {
		t.Fatalf("recommended tap = %+v, %v; want the bare label and the answers in card order", k, ok)
	}
	var modal askTap
	if err := json.Unmarshal([]byte(buttons[2].Value), &modal); err != nil || modal.ID != "step-3:merge" || modal.Question != "Merge #41 now?" ||
		!slices.Equal(modal.Options, []string{"Hold", "Merge"}) {
		t.Fatalf("Other button value %q, want the id, question, and answers the backend's modal shows (%v)", buttons[2].Value, err)
	}
	if _, ok := (cardActionPush{ActionID: buttons[2].ActionID, Value: buttons[2].Value}).tap(); ok {
		t.Fatal("a bare Other tap parsed as an answer; only the modal's written answer answers")
	}
	explain := cardActionPush{ActionID: buttons[3].ActionID, Value: buttons[3].Value}
	if k, ok := explain.tap(); !ok || !k.Explain || !k.Waits || k.ID != "step-3:merge" || k.Label != cardExplainLabel || k.Question != "Merge #41 now?" {
		t.Fatalf("explain tap = %+v, %v; want Explain more on the waiting ask", k, ok)
	}
	if got := blocksJSON(t, blocks); !strings.Contains(got, "stopped until you answer") || strings.Contains(got, cardAckAction) {
		t.Fatalf("waiting card %s, want the wait hint and no Ack or Pause", got)
	}
	if merged := card.merged(card); merged.Waits != "" || merged.Recommended != "" {
		t.Fatal("the root card kept the reply's waits or recommendation")
	}
}

func blocksJSON(t *testing.T, blocks []slack.Block) string {
	t.Helper()
	raw, err := json.Marshal(blocks)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestReplyCardOffersOptionsThenAckAndPauseUnderTheSessionTag(t *testing.T) {
	card := statusCard{Status: "incident", Title: "Origin errors are the PCI scanner", Body: "Monitor fix is green.",
		Drive: "cloudflare-alert", Phase: "monitor fix", Options: []string{"ship it"},
		Links: []cardLink{{Label: "PR", URL: "https://github.com/acme/app/pull/1"}}}
	tag := cardTag{Session: "0123abcd", Repo: "app", Branch: "dev", Worktree: "cf-alert"}
	at := time.Date(2026, 10, 15, 8, 33, 0, 0, time.UTC)
	blocks := card.replyBlocks(cardView{tag: tag, at: at, now: at})
	var actionIDs []string
	for _, b := range blocks {
		if a, ok := b.(*slack.ActionBlock); ok {
			for _, e := range a.Elements.ElementSet {
				button, ok := e.(*slack.ButtonBlockElement)
				if !ok {
					t.Fatalf("action element %T is not a button", e)
				}
				actionIDs = append(actionIDs, button.ActionID)
			}
		}
	}
	want := []string{cardLinkAction + "0", cardOptionAction + "0", cardAckAction, cardPauseAction}
	if strings.Join(actionIDs, ",") != strings.Join(want, ",") {
		t.Fatalf("buttons = %v, want %v", actionIDs, want)
	}
	got := blocksJSON(t, blocks)
	for _, part := range []string{"🚨 Origin errors are the PCI scanner", "*cloudflare-alert* · monitor fix · `app@dev` · worktree `cf-alert` · session `0123abcd` · 1:33am"} {
		if !strings.Contains(got, part) {
			t.Fatalf("reply card %s lacks %q", got, part)
		}
	}
	if card.text() != "🚨 Origin errors are the PCI scanner — Monitor fix is green." {
		t.Fatalf("notification text = %q", card.text())
	}
}

func TestRootCardNamesTheDriveElseTheBranchAndCarriesNoReplyButtons(t *testing.T) {
	at := time.Date(2026, 10, 15, 8, 33, 0, 0, time.UTC)
	tag := cardTag{Session: "0123abcd", Repo: "app", Branch: "dev"}
	for _, c := range []struct {
		card   statusCard
		header string
	}{
		{statusCard{Status: "progress", Title: "t", Drive: "sessions-and-runs"}, "🔄 sessions-and-runs"},
		{statusCard{Status: "done", Title: "t"}, "✅ app@dev"},
	} {
		card, header := c.card, c.header
		got := blocksJSON(t, card.rootBlocks(cardView{tag: tag, at: at, now: at}))
		if !strings.Contains(got, header) || !strings.Contains(got, "updated 1:33am") || strings.Contains(got, cardActionsBlock) {
			t.Fatalf("root card %s, want header %q, an update time, and no reply buttons", got, header)
		}
	}
	if got := blocksJSON(t, (statusCard{Status: "info", Title: "t"}).rootBlocks(cardView{tag: cardTag{Session: "0123abcd"}, at: at, now: at})); !strings.Contains(got, "Claude session 0123abcd") {
		t.Fatalf("root card outside a repo = %s, want the session named", got)
	}
}

func cardBody(title string, options ...string) map[string]any {
	return map[string]any{"card": statusCard{Status: "needs_you", Title: title, Body: "Two PRs are green.", Options: options}}
}

func TestDMStatusCardOpensALiveRootAndRedrawsIt(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	var first, second map[string]string
	if err := json.Unmarshal(l.do(opDMStatus, cardBody("Merge the stack?", "merge")).Body, &first); err != nil {
		t.Fatal(err)
	}
	sent := l.slack.sent()
	if len(sent) != 2 || sent[0].threadTS != "" || sent[1].threadTS != sent[0].ts || first["thread_ts"] != sent[0].ts {
		t.Fatalf("first card posts = %+v, result %v, want a root then a reply in its thread", sent, first)
	}
	if strings.Contains(sent[0].blocks, cardActionsBlock) || !strings.Contains(sent[1].blocks, cardAckAction) {
		t.Fatalf("root %s, reply %s: want buttons on the reply only", sent[0].blocks, sent[1].blocks)
	}
	if sent[1].text != "🙋 Merge the stack? — Two PRs are green." {
		t.Fatalf("notification text = %q", sent[1].text)
	}
	if err := json.Unmarshal(l.do(opDMStatus, cardBody("Landed")).Body, &second); err != nil {
		t.Fatal(err)
	}
	sent = l.slack.sent()
	edits := l.slack.edited()
	if second["thread_ts"] != first["thread_ts"] || len(sent) != 3 || sent[2].threadTS != first["thread_ts"] {
		t.Fatalf("second card posts = %+v, want one more reply in the thread", sent)
	}
	if len(edits) != 1 || edits[0].ts != first["thread_ts"] || !strings.Contains(edits[0].blocks, "*Landed*") {
		t.Fatalf("root edits = %+v, want the root redrawn with the latest card", edits)
	}
	l.do(opDMStatus, map[string]string{"text": "a quick aside"})
	if len(l.slack.edited()) != 1 {
		t.Fatalf("a plain-text status redrew the root: %+v", l.slack.edited())
	}
}

func TestDMStatusCardLeavesAShortcutRootAlone(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	const dmTS = "1800000050.000000"
	body := cardBody("On it")
	body["thread_ts"] = dmTS
	l.do(opDMStatus, body)
	l.do(opDMStatus, cardBody("Halfway"))
	sent := l.slack.sent()
	if len(sent) != 2 || sent[0].threadTS != dmTS || sent[1].threadTS != dmTS || len(l.slack.edited()) != 0 {
		t.Fatalf("posts %+v, edits %+v: want replies under the shortcut DM and no edit of it", sent, l.slack.edited())
	}
}

func TestDMStatusRefusesACardWithBlocksOrABadStatus(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	raw := mustJSON(map[string]any{"blocks": []map[string]any{{"type": "divider"}}, "text": "x", "card": statusCard{Status: "info", Title: "t"}})
	if _, err := invoke(l.ctx, l.deps, testSession, "/any", opDMStatus, raw); err == nil || !strings.Contains(err.Error(), "never both") {
		t.Fatalf("card with blocks: %v", err)
	}
	raw = mustJSON(map[string]any{"card": statusCard{Status: "urgent", Title: "t"}})
	if _, err := invoke(l.ctx, l.deps, testSession, "/any", opDMStatus, raw); err == nil || !strings.Contains(err.Error(), "needs_you") {
		t.Fatalf("card with a bad status: %v", err)
	}
}

func (l *liveDaemon) tap(id identity, push cardActionPush) {
	l.t.Helper()
	frame := mustJSON(map[string]any{"event_type": cardActionEventType, "event_payload": push})
	if err := socketFrames(l.app.shortcuts, l.app.poller, l.app.replay)(l.ctx, id, frame); err != nil {
		l.t.Fatal(err)
	}
}

func TestCardTapSteersTheSessionOnceAndMarksTheCard(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	l.do(opDMStatus, cardBody("Merge the stack?", "merge it"))
	sent := l.slack.sent()
	root, reply := sent[0].ts, sent[1].ts
	id, err := loadIdentity(l.ctx, l.srv.DB())
	if err != nil {
		t.Fatal(err)
	}
	push := cardActionPush{User: testUser, Channel: id.DMChannelID, MessageTS: reply, ThreadTS: root,
		ActionID: cardOptionAction + "0", Value: "merge it", ActionTS: "1800000100.000000", Seq: "001800000100000-abcd0123"}
	l.tap(id, push)
	got := l.inject(agent.TopLevel)
	if !strings.Contains(got, `they chose "merge it"`) || !strings.Contains(got, "nothing waits on this tap") {
		t.Fatalf("directive = %q, want the chosen option as operator steering", got)
	}
	if types := l.channelTypes(); !slices.Contains(types, eventAction) {
		t.Fatalf("channel saw %v, want %s", types, eventAction)
	}
	edits := editsOf(l.slack.edited(), reply)
	if len(edits) != 1 || !strings.Contains(edits[0], "✓ *merge it*") || strings.Contains(edits[0], cardAckAction) {
		t.Fatalf("reply edits = %v, want the reply's buttons replaced by the choice", edits)
	}
	if l.app.replay.latest() != push.Seq {
		t.Fatalf("replay cursor = %q, want it past the tap", l.app.replay.latest())
	}
	l.tap(id, push)
	if got := l.inject(agent.TopLevel); got != "" {
		t.Fatalf("a replayed tap steered again: %q", got)
	}
}

func TestCardTapPauseAndStrangersAreHandled(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	l.do(opDMStatus, cardBody("Rolling out"))
	sent := l.slack.sent()
	id, err := loadIdentity(l.ctx, l.srv.DB())
	if err != nil {
		t.Fatal(err)
	}
	push := cardActionPush{User: "U0STRANGER", Channel: id.DMChannelID, MessageTS: sent[1].ts, ThreadTS: sent[0].ts,
		ActionID: cardPauseAction, Value: cardPauseLabel, ActionTS: "1800000100.000000"}
	l.tap(id, push)
	if got := l.inject(agent.TopLevel); got != "" || len(l.slack.edited()) != 0 {
		t.Fatalf("another user's tap steered %q and edited %+v", got, l.slack.edited())
	}
	push.User = testUser
	l.tap(id, push)
	if got := l.inject(agent.TopLevel); !strings.Contains(got, "Start no new lanes") {
		t.Fatalf("pause directive = %q", got)
	}
}

func TestFillCompletesACardPostedWithTextAndAMarkedLabel(t *testing.T) {
	c := statusCard{Waits: "step-3:merge", Options: []string{"Merge (Recommended)", "Hold"}}
	c.fill("Merge #41 now?\nCI is green.")
	want := statusCard{Status: "needs_you", Title: "Merge #41 now?", Body: "Merge #41 now?\nCI is green.", Waits: "step-3:merge", Options: []string{"Merge", "Hold"}, Recommended: "Merge"}
	if g, w := string(mustJSON(c)), string(mustJSON(want)); g != w {
		t.Fatalf("filled card = %s\nwant %s", g, w)
	}
	if err := c.check(time.Now(), false); err != nil {
		t.Fatalf("a filled card: %v", err)
	}
	info := statusCard{Title: "Batch 3 landed"}
	if info.fill(""); info.Status != "info" {
		t.Fatalf("a card with no waits filled status %q, want info", info.Status)
	}
}

func TestCheckNamesEveryProblemAtOnce(t *testing.T) {
	err := statusCard{Status: "urgent", Title: "x", Options: []string{"go", "go"}}.check(time.Now(), false)
	if err == nil || !strings.Contains(err.Error(), "urgent") || !strings.Contains(err.Error(), "repeats a button") {
		t.Fatalf("check = %v, want both the status and the repeated option named", err)
	}
}
