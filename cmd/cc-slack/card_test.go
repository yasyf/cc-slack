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
	if err := ok.check(time.Now(), false, ""); err != nil {
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
		"stranger, no waits":     {Status: "info", Title: "x", Options: []string{"go", "stop"}, Recommended: "wait"},
		"recommends no option":   {Status: "info", Title: "x", Recommended: "go"},
		"rows, no group":         {Status: "needs_you", Title: "x", Rows: []cardRow{prRow(41)}},
		"group with options":     {Status: "needs_you", Title: "x", Group: "pg:login", Options: []string{"go"}, Rows: []cardRow{prRow(41)}},
		"repeated row":           {Status: "needs_you", Title: "x", Group: "pg:login", Rows: []cardRow{prRow(41), prRow(41)}},
		"row repeats a button":   {Status: "needs_you", Title: "x", Group: "pg:login", Rows: []cardRow{{ID: "r", Text: "t", Buttons: []string{"Hold", "Hold"}}}},
		"row without buttons":    {Status: "needs_you", Title: "x", Group: "pg:login", Rows: []cardRow{{ID: "r", Text: "t"}}},
		"twenty-one rows":        {Status: "needs_you", Title: "x", Group: "pg:login", Rows: prRows(21)},
		"key with space":         {Status: "info", Title: "x", Key: "reply one"},
		"key on a waiting card":  {Status: "needs_you", Title: "x", Key: "r1", Waits: "step-3:merge", Options: []string{"go"}, Recommended: "go"},
		"key on a group":         {Status: "needs_you", Title: "x", Key: "r1", Group: "pg:login", Rows: []cardRow{prRow(41)}},
		"supersedes, no waits":   {Status: "info", Title: "x", Supersedes: []string{"https://example.slack.com/archives/D1/p1800000000000100"}},
		"supersedes a channel":   {Status: "needs_you", Title: "x", Waits: "step-3:merge", Options: []string{"go"}, Recommended: "go", Supersedes: []string{"https://example.slack.com/archives/D1"}},
	} {
		if err := card.check(time.Now(), false, ""); err == nil {
			t.Errorf("%s: check passed, want a refusal", name)
		}
	}
	if err := (statusCard{Status: "needs_you", Title: "x", Waits: "step-3:merge"}).check(time.Now(), true, ""); err == nil {
		t.Error("a root-only waiting card passed check, want a refusal")
	}
	if err := (statusCard{Title: "x", Key: "r1"}).check(time.Now(), true, ""); err == nil {
		t.Error("a root-only keyed card passed check, want a refusal")
	}
}

func TestACardLinksHTTPOnlyOnTheConfiguredTailnet(t *testing.T) {
	const tailnet = "example.ts.net"
	link := func(url string) statusCard {
		return statusCard{Status: "info", Title: "x", Links: []cardLink{{Label: "One-pager", URL: url}}}
	}
	if err := link("http://studio.example.ts.net:8642/one-pagers/memory/").check(time.Now(), false, tailnet); err != nil {
		t.Fatalf("an http link on the tailnet: %v", err)
	}
	for name, tc := range map[string]struct{ url, tailnet string }{
		"no tailnet configured":  {"http://studio.example.ts.net:8642/", ""},
		"another tailnet":        {"http://studio.other.ts.net/", tailnet},
		"a host ending the same": {"http://studio.notexample.ts.net/", tailnet},
		"the open web":           {"http://example.com/", tailnet},
	} {
		if err := link(tc.url).check(time.Now(), false, tc.tailnet); err == nil {
			t.Errorf("%s: %s passed check, want a refusal", name, tc.url)
		}
	}
	rollup := statusCard{Status: "info", Title: "x", Rollup: &cardRollup{PRs: []rollupPR{{URL: "http://studio.example.ts.net/pull/1"}}}}
	if err := rollup.check(time.Now(), false, tailnet); err == nil {
		t.Error("an http rollup pr on the tailnet passed check; only card links may be http")
	}
}

func cardButtons(t *testing.T, blocks []slack.Block) []*slack.ButtonBlockElement {
	t.Helper()
	var buttons []*slack.ButtonBlockElement
	for _, b := range blocks {
		if row, ok := b.(*slack.SectionBlock); ok && row.Accessory != nil && row.Accessory.ButtonElement != nil {
			buttons = append(buttons, row.Accessory.ButtonElement)
		}
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
	if err := card.check(time.Now(), false, ""); err != nil {
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

func TestReplyCardLeadsWithItsRecommendationAndWaitsOnNothing(t *testing.T) {
	marked := statusCard{Title: "Store drafts in the plan file?", Options: []string{"Use cci", "Plan file (Recommended)"}}
	marked.fill("")
	named := statusCard{Status: "info", Title: "Store drafts in the plan file?", Options: []string{"Use cci", "Plan file"}, Recommended: "Plan file"}
	for name, card := range map[string]statusCard{"recommended": named, "marked option": marked} {
		if err := card.check(time.Now(), false, ""); err != nil {
			t.Fatalf("%s: a reply card with a recommendation: %v", name, err)
		}
		at := time.Date(2026, 11, 9, 9, 22, 0, 0, time.UTC)
		blocks := card.replyBlocks(cardView{at: at, now: at})
		buttons := cardButtons(t, blocks)
		var got []string
		for _, b := range buttons {
			got = append(got, b.ActionID+"="+b.Text.Text)
		}
		want := []string{
			cardOptionAction + "0=Plan file (Recommended)", cardOptionAction + "1=Use cci",
			cardAckAction + "=" + cardAckLabel, cardPauseAction + "=" + cardPauseLabel,
		}
		if !slices.Equal(got, want) {
			t.Fatalf("%s: buttons = %q, want the recommendation first, then the other option, Ack, and Pause", name, got)
		}
		if buttons[0].Style != slack.StylePrimary || buttons[1].Style != "" {
			t.Fatalf("%s: styles = %q, %q; want only the recommendation primary", name, buttons[0].Style, buttons[1].Style)
		}
		if k, ok := (cardActionPush{ActionID: buttons[0].ActionID, Value: buttons[0].Value}).tap(); !ok || k.Waits || k.Label != "Plan file" {
			t.Fatalf("%s: recommended tap = %+v, %v; want the bare label with nothing waiting", name, k, ok)
		}
		if got := blocksJSON(t, blocks); strings.Contains(got, "stopped") || strings.Contains(got, cardExplainAction) || strings.Contains(got, cardOtherAction) {
			t.Fatalf("%s: reply card %s says a lane is stopped or carries an ask card's buttons", name, got)
		}
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

func TestReplyCardOffersOptionsThenAckAndPauseUnderAQuietFooter(t *testing.T) {
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
	for _, part := range []string{"🚨 Origin errors are the PCI scanner", "cloudflare-alert · monitor fix · 1:33am"} {
		if !strings.Contains(got, part) {
			t.Fatalf("reply card %s lacks %q", got, part)
		}
	}
	if card.text() != "🚨 Origin errors are the PCI scanner" {
		t.Fatalf("notification text = %q", card.text())
	}
}

func TestACardsNotificationTextIsItsTitleAloneSoAnUnfurlDrawsTheBodyOnce(t *testing.T) {
	card := statusCard{Status: "needs_you", Title: "Which strategy?", Body: "Usage picks none.\n| Strategy | Old parts |\n|---|---|\n| Hash by key | stay busy |\nThe split waits on your pick.\n*Show me*\n| a | b |"}
	if got := card.text(); got != card.emoji()+" Which strategy?" {
		t.Fatalf("notification text = %q, want the title alone: Slack draws a linked card's text beside its blocks, so a body there draws twice", got)
	}
	tapped := view()
	tapped.tapped = "✓ *Hash by key* · 5:05pm"
	if got := card.replyText(tapped); got != "Which strategy?" {
		t.Fatalf("settled notification text = %q, want the title alone, not the outcome its block draws", got)
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

func TestDMStatusCardsEachLandAtTheDMRootWithTheirOwnThread(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	var first, second map[string]string
	if err := json.Unmarshal(l.do(opDMStatus, cardBody("Merge the stack?", "merge")).Body, &first); err != nil {
		t.Fatal(err)
	}
	sent := l.slack.sent()
	if len(sent) != 1 || sent[0].threadTS != "" || first["thread_ts"] != sent[0].ts || first["ts"] != sent[0].ts {
		t.Fatalf("first card posts = %+v, result %v, want one message at the DM root", sent, first)
	}
	if !strings.Contains(sent[0].blocks, cardAckAction) {
		t.Fatalf("card %s, want its buttons on the message", sent[0].blocks)
	}
	if sent[0].text != "🙋 Merge the stack?" {
		t.Fatalf("notification text = %q", sent[0].text)
	}
	if err := json.Unmarshal(l.do(opDMStatus, cardBody("Landed")).Body, &second); err != nil {
		t.Fatal(err)
	}
	l.do(opDMStatus, map[string]string{"text": "a quick aside"})
	sent = l.slack.sent()
	if len(sent) != 3 || sent[1].threadTS != "" || sent[2].threadTS != "" || second["thread_ts"] != sent[1].ts || second["thread_ts"] == first["thread_ts"] {
		t.Fatalf("later posts = %+v, result %v, want each at the DM root with its own thread", sent, second)
	}
	if edits := l.slack.edited(); len(edits) != 0 {
		t.Fatalf("edits = %+v, want no earlier message redrawn", edits)
	}
}

func TestDMStatusCardPostsUnderItsThreadAndKeepsThatThreadWatched(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	const dmTS = "1800000050.000000"
	body := cardBody("On it")
	body["thread_ts"] = dmTS
	l.do(opDMStatus, body)
	l.do(opDMStatus, cardBody("Halfway"))
	sent := l.slack.sent()
	if len(sent) != 2 || sent[0].threadTS != dmTS || sent[1].threadTS != "" || len(l.slack.edited()) != 0 {
		t.Fatalf("posts %+v, edits %+v: want the first under its thread, the second at the DM root, no edit", sent, l.slack.edited())
	}
	if active := activeDMThreads(t, l); !slices.Contains(active, dmTS) || !slices.Contains(active, sent[1].ts) {
		t.Fatalf("active DM watches = %v, want the thread and the root card", active)
	}
}

func TestAKeyedCardInAThreadIsEditedInPlace(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	const dmTS = "1800000050.000000"
	post := func(title string) {
		body := cardBody(title)
		body["thread_ts"] = dmTS
		body["card"] = statusCard{Status: "info", Title: title, Body: "Two PRs are green.", Key: "reply-1"}
		l.do(opDMStatus, body)
	}
	post("On it")
	post("Done")
	if sent := l.slack.sent(); len(sent) != 1 || sent[0].threadTS != dmTS || len(l.slack.edited()) != 1 {
		t.Fatalf("posts %+v, edits %+v: want one post under the thread and one edit of it", sent, l.slack.edited())
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
	l.push(id, push)
	l.app.poller.taps.busy.Wait()
}

func (l *liveDaemon) push(id identity, push cardActionPush) {
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
	l.do(opDMStatus, cardBody("A later update"))
	reply := l.slack.sent()[0].ts
	id, err := loadIdentity(l.ctx, l.srv.DB())
	if err != nil {
		t.Fatal(err)
	}
	push := cardActionPush{User: testUser, Channel: id.DMChannelID, MessageTS: reply, ThreadTS: reply,
		ActionID: cardOptionAction + "0", Value: "merge it", ActionTS: "1800000100.000000", Seq: "001800000100000-abcd0123"}
	l.tap(id, push)
	got := l.inject(agent.TopLevel)
	if !strings.Contains(got, `they chose "merge it"`) || !strings.Contains(got, "nothing waits on this tap") {
		t.Fatalf("directive = %q, want the chosen option as operator steering", got)
	}
	if types := l.channelTypes(); slices.Contains(types, eventAction) {
		t.Fatalf("channel saw %v, want no %s the hook already delivered", types, eventAction)
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
	push := cardActionPush{User: "U0STRANGER", Channel: id.DMChannelID, MessageTS: sent[0].ts, ThreadTS: sent[0].ts,
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
	if err := c.check(time.Now(), false, ""); err != nil {
		t.Fatalf("a filled card: %v", err)
	}
	info := statusCard{Title: "Batch 3 landed"}
	info.fill("")
	if info.Status != "info" {
		t.Fatalf("a card with no waits filled status %q, want info", info.Status)
	}
	twice := statusCard{Waits: "step-3:merge", Title: "Merge?", Body: "CI is green on dev.", Options: []string{"Merge"}}
	twice.fill("CI is green on dev.\n")
	if twice.Body != "CI is green on dev." {
		t.Fatalf("body passed as both text and card body = %q, want it once", twice.Body)
	}
}

func TestCheckNamesEveryProblemAtOnce(t *testing.T) {
	err := statusCard{Status: "urgent", Title: "x", Options: []string{"go", "go"}}.check(time.Now(), false, "")
	if err == nil || !strings.Contains(err.Error(), "urgent") || !strings.Contains(err.Error(), "repeats a button") {
		t.Fatalf("check = %v, want both the status and the repeated option named", err)
	}
}

func TestFillLeavesAnUnmarkedOptionAlone(t *testing.T) {
	c := statusCard{Status: "info", Title: "Ship?", Options: []string{" Ship "}}
	c.fill("")
	if c.Recommended != "" || c.Options[0] != " Ship " {
		t.Fatalf("filled options %q recommended %q, want them untouched", c.Options, c.Recommended)
	}
}

func TestPermissionCardSpeaksInWords(t *testing.T) {
	plain := statusCard{
		Status: "needs_you", Title: "Post this in the #alerts thread the alert bot started?", Waits: "slack-e11b7b897370",
		Body: "Why: the alert bot's thread still says investigating, and the likely cause is in.\n" +
			"Where: the #alerts thread the alert bot started at 6:42pm: runners refusing runs on the GOVERNANCE cluster\n\n" +
			"Thanks <@U0DANA00001>, the cause is in <https://acme.slack.com/archives/C0C5SEUJK5L/p1793760152086369|the alert thread>.",
		Options: []string{"Yes", "No"}, Recommended: "Yes",
	}
	if err := plain.check(time.Now(), false, ""); err != nil {
		t.Fatalf("a permission card in plain words: %v", err)
	}
	for name, tc := range map[string]struct {
		title, body string
		fix         string
	}{
		"channel id":   {"Post this in C0C5SEUJK5L?", plain.Body, "name the channel as #name"},
		"thread ts":    {"Post this in #alerts, thread 1793760152.086369?", plain.Body, "who started the thread"},
		"session hash": {plain.Title, "Why: session 144ffadb3d797d00 asked for it.", "say what it stands for"},
		"lane name":    {plain.Title, "Why: bugs-comms-2 owes the thread an update.", "what that agent is doing"},
	} {
		for _, waits := range []string{plain.Waits, "slack:C0C5SEUJK5L:1793760152.086369:e11b7b897370"} {
			card := plain
			card.Title, card.Body, card.Waits = tc.title, tc.body, waits
			if err := card.check(time.Now(), false, ""); err == nil || !strings.Contains(err.Error(), tc.fix) {
				t.Errorf("%s under %s: want a refusal saying %q, got %v", name, waits, tc.fix, err)
			}
		}
	}
	other := plain
	other.Title, other.Waits = "Post this in C0C5SEUJK5L, thread 1793760152.086369?", "step-3:merge"
	if err := other.check(time.Now(), false, ""); err != nil {
		t.Fatalf("only a permission card is held to plain words: %v", err)
	}
}

func TestACardNamesEachOptionInWordsNeverByNumberOrPosition(t *testing.T) {
	plain := statusCard{
		Status: "needs_you", Title: "How should scripts run?", Waits: "scripts:limits",
		Body:    "This is step 1 of the migration on 0.162.3619; it merged 1 PR in 2 days, like <https://github.com/acme/app/pull/4031|#4031>.",
		Options: []string{"Own process", "Own process plus a memory cap per script, stopping a script over the cap"}, Recommended: "Own process plus a memory cap per script, stopping a script over the cap",
	}
	if err := plain.check(time.Now(), false, ""); err != nil {
		t.Fatalf("a card whose options stand alone: %v", err)
	}
	stamped := plain
	stamped.Body = `Your "Drop the limit" pick 7:41 AM removed that limit, not the landing rule. Same as 9:05 AM (job start), it held.`
	for _, check := range []func(time.Time, bool, string) error{stamped.check, stamped.checkMirrored} {
		if err := check(time.Now(), false, ""); err != nil {
			t.Fatalf("a clock time after a choice word: %v", err)
		}
	}
	for name, tc := range map[string]struct {
		title, body, option string
		quoted              string
	}{
		"as a number in an option": {plain.Title, plain.Body, "Own process plus a memory cap per script: as 1, and a script over the cap stops", `"as 1"`},
		"same as in an option":     {plain.Title, plain.Body, "Same as 2 but with a cap", `"Same as 2"`},
		"options in the body":      {plain.Title, "Option 2 is the only one; options 1 and 2 add a process.", plain.Options[1], `"Option 2"`},
		"position in the title":    {"Take the first option?", plain.Body, plain.Options[1], `"first option"`},
		"like a hash number":       {plain.Title, "Like #2, it waits.", plain.Options[1], `"Like #2"`},
		"pick before a colon":      {plain.Title, "Pick 2: it costs less.", plain.Options[1], `"Pick 2"`},
	} {
		card := plain
		card.Title, card.Body, card.Options = tc.title, tc.body, []string{plain.Options[0], tc.option}
		card.Recommended = tc.option
		for _, check := range []func(time.Time, bool, string) error{card.check, card.checkMirrored} {
			if err := check(time.Now(), false, ""); err == nil || !strings.Contains(err.Error(), tc.quoted) || !strings.Contains(err.Error(), "by number or position") {
				t.Errorf("%s: want a refusal quoting %s, got %v", name, tc.quoted, err)
			}
		}
	}
	info := statusCard{Status: "info", Title: "Option 2 landed"}
	if err := info.check(time.Now(), false, ""); err != nil {
		t.Fatalf("a card with no options draws no numbers: %v", err)
	}
	quoted := `"PROVISIONAL by recommendation: option 1. A workflow run with no agent is a session of its own"`
	answered := plain
	answered.Options = []string{plain.Options[0], plain.Options[1], cardAnsweredLabel}
	answered.Hints = map[string]string{cardAnsweredLabel: quoted}
	if err := answered.check(time.Now(), false, ""); err != nil {
		t.Fatalf("an answer the drive quotes under %s: %v", cardAnsweredLabel, err)
	}
	answered.Hints = map[string]string{plain.Options[0]: quoted}
	if err := answered.check(time.Now(), false, ""); err == nil || !strings.Contains(err.Error(), `hint for option "Own process"`) {
		t.Fatalf("a job's own hint naming option 1: want a refusal, got %v", err)
	}
}

func TestACardListsLongOptionsOnceEachBesideItsOwnButton(t *testing.T) {
	long := []string{"Cap the run search at 200 sessions and drop the rest", "Run the whole search tonight on the pass box", "Skip the search and ship the fix as is"}
	card := statusCard{Status: "needs_you", Title: "How should the run search pay for itself?", Options: long, Recommended: long[1], Waits: "cost-pick"}
	at := time.Date(2026, 11, 4, 5, 30, 0, 0, time.UTC)
	blocks := card.replyBlocks(cardView{at: at, now: at})
	for _, want := range []string{"*1. Run the whole search tonight on the pass box* _(Recommended)_", "*2. Cap the run search at 200 sessions and drop the rest*", "*3. Skip the search and ship the fix as is*"} {
		if got := blocksJSON(t, blocks); !strings.Contains(got, want) {
			t.Fatalf("blocks %s, want every option in full, numbered in button order, the recommendation marked", got)
		}
	}
	buttons := cardButtons(t, blocks)
	labels := make([]string, 0, 3)
	for _, b := range buttons[:3] {
		labels = append(labels, b.Text.Text)
		k, ok := (cardActionPush{ActionID: b.ActionID, Value: b.Value}).tap()
		if !ok || !slices.Contains(long, k.Label) {
			t.Fatalf("button %q taps %+v, %v; want the full option as the answer", b.Text.Text, k, ok)
		}
	}
	if !slices.Equal(labels, []string{cardPickLabel, cardPickLabel, cardPickLabel}) || buttons[0].Style != slack.StylePrimary {
		t.Fatalf("labels = %q, want one pick button per option with the recommendation primary", labels)
	}

	steer := statusCard{Status: "info", Title: "Which first?", Options: long[:2]}
	buttons = cardButtons(t, steer.replyBlocks(cardView{at: at, now: at}))
	if buttons[0].Text.Text != cardPickLabel || buttons[0].Value != long[0] || buttons[1].ActionID != cardOptionAction+"1" {
		t.Fatalf("option buttons %q=%q, %q; want a pick button that taps the full option", buttons[0].Text.Text, buttons[0].Value, buttons[1].ActionID)
	}
}
