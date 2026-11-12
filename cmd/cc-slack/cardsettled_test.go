package main

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/slack-go/slack"
)

func exampleCard() statusCard {
	return statusCard{
		Status: "needs_you",
		Title:  "Post this to Anubhav in the #platform-oncall thread about Chime's refused runs?",
		Body: "Why: Anubhav said at 4:15 PM he may start more backfill runs. <!-- cci 441268 root -->\n" +
			"Where: the #platform-oncall thread Wadsworth started at 3:53pm\n\n" +
			"Before you start more backfill runs: the retry fix has been live on plat since 4:14 PM.",
		Options:     []string{"Yes, post it", "No"},
		Recommended: "Yes, post it",
		Waits:       "ask-chime",
	}
}

func TestASettledCardDrawsOnlyItsTitleAndOutcome(t *testing.T) {
	card := exampleCard()
	const line = "*Withdrawn:* removed from the board · 5:05pm"
	v := view()
	v.tapped = line

	blocks := card.replyBlocks(v)
	if len(blocks) != 2 {
		t.Fatalf("blocks = %s, want the header and the outcome", blocksJSON(t, blocks))
	}
	if h, ok := blocks[0].(*slack.HeaderBlock); !ok || h.Text.Text != card.Title {
		t.Fatalf("first block %s, want the title with no needs-you emoji", blocksJSON(t, blocks[:1]))
	}
	if got := blocksJSON(t, blocks[1:]); !strings.Contains(got, line) {
		t.Fatalf("second block %s, want the outcome", got)
	}
	if got := card.replyText(v); got != card.Title {
		t.Fatalf("text = %q, want the title alone", got)
	}
	if again := blocksJSON(t, card.replyBlocks(v)); again != blocksJSON(t, blocks) {
		t.Fatalf("a second draw = %s, want the same card", again)
	}
	if _, fold := card.layout(v); fold != nil {
		t.Fatalf("a settled card still folds %d pages", len(fold))
	}

	live := blocksJSON(t, card.replyBlocks(view()))
	for _, part := range []string{"🙋", "Why: Anubhav", "Where: the #platform-oncall", "the retry fix", "*1. Yes, post it*", "*2. No*", cardWaitAction} {
		if !strings.Contains(live, part) {
			t.Fatalf("a live card lost %q: %s", part, live)
		}
	}
}

func TestEverySettleRouteCollapsesTheCardOnceAndPostsNothingInItsThread(t *testing.T) {
	const board = "http://127.0.0.1:7000/p/asks--ab12"
	answer := func(l *liveDaemon, c postedCard, fields map[string]any) {
		fields["waits"], fields["url"] = "step-3:merge", c.url()
		l.do(opCardAnswer, fields)
	}
	n := 0
	routes := map[string]struct {
		settle func(*liveDaemon, postedCard)
		want   string
		undo   bool
	}{
		"tap": {func(l *liveDaemon, c postedCard) {
			n++
			l.tap(c.id, cardActionPush{User: testUser, Channel: c.id.DMChannelID, MessageTS: c.ts, ThreadTS: c.thread,
				ActionID: cardWaitAction + "0:step-3:merge", Value: string(mustJSON(askTap{ID: "step-3:merge", Label: "Hold", Question: "Merge #41 now?"})),
				ActionTS: fmt.Sprintf("%d.%06d", time.Now().Unix(), n), Seq: fmt.Sprintf("001800000100000-abce%04d", n)})
		}, "✓ *Hold* · answered", true},
		"answered elsewhere": {func(l *liveDaemon, c postedCard) {
			answer(l, c, map[string]any{"label": "Hold", "via": board})
		}, board + "|the board", false},
		"settled": {func(l *liveDaemon, c postedCard) {
			answer(l, c, map[string]any{"label": "Landed as 5480445", "other": true, "settled": true, "via": "https://github.com/acme/app/pull/41"})
		}, "https://github.com/acme/app/pull/41|Landed as 5480445", false},
		"withdrawn": {func(l *liveDaemon, c postedCard) {
			answer(l, c, map[string]any{"label": "superseded by step 4", "withdrawn": true, "via": board})
		}, "*Withdrawn:* superseded by step 4", false},
	}
	for name, route := range routes {
		t.Run(name, func(t *testing.T) {
			l := startDaemon(t)
			loggedIn(t, l)
			c := postWaiting(t, l, mergeCard())
			posts := len(l.slack.sent())

			route.settle(l, c)
			edits := editsOf(l.slack.edited(), c.ts)
			if len(edits) == 0 {
				t.Fatal("the settle edited nothing")
			}
			last := edits[len(edits)-1]
			var drawn []map[string]any
			if err := json.Unmarshal([]byte(last), &drawn); err != nil {
				t.Fatal(err)
			}
			blocks := 2
			if route.undo {
				blocks++
			}
			if len(drawn) != blocks || !strings.Contains(last, "Merge #41 now?") || !strings.Contains(last, route.want) || strings.Contains(last, cardUndoAction) != route.undo {
				t.Fatalf("last edit %s, want the title and %q alone, with Undo only after his own tap", last, route.want)
			}
			if strings.Contains(last, "CI is green.") || strings.Contains(last, cardWaitAction) {
				t.Fatalf("last edit %s keeps the body or the buttons", last)
			}

			route.settle(l, c)
			if again := editsOf(l.slack.edited(), c.ts); len(again) != len(edits) {
				t.Fatalf("a second settle edited the card again: %v", again[len(edits):])
			}
			if got := len(l.slack.sent()); got != posts {
				t.Fatalf("posts = %d after settling, want %d: settling adds nothing to the thread", got, posts)
			}
		})
	}
}

func TestACardOutsideTheLiveStoreCollapsesOnce(t *testing.T) {
	blocks := exampleCard().replyBlocks(view())
	blocks[0] = header(":raising_hand: Post it?")
	text, settled := settledMessage(blocks, "✓ *No* · 5:05pm")
	if got := blocksJSON(t, settled); len(settled) != 2 || strings.Contains(got, "raising_hand") || strings.Contains(got, "Why:") || strings.Contains(got, cardWaitAction) {
		t.Fatalf("settled = %s, want the bare title and the outcome", got)
	}
	if text != "Post it?" {
		t.Fatalf("text = %q", text)
	}
	if _, twice := settledMessage(settled, "✓ *Yes* · 5:06pm"); twice != nil {
		t.Fatalf("a settled card settled again: %s", blocksJSON(t, twice))
	}
}

func TestAnAckedCardKeepsItsOpenAsks(t *testing.T) {
	card := statusCard{Status: "progress", Title: "Rolling out", Body: "Half the fleet is done.",
		Asks: []cardAsk{{ID: "merge", Label: "Merge #41"}, {ID: "hold", Label: "Hold #41"}}}
	v := view()
	v.tapped = "✓ *Ack* · 5:05pm"
	v.answers = map[string]askAnswer{"hold": {At: bodyAt, Label: "Hold #41"}}
	blocks := card.replyBlocks(v)
	got := blocksJSON(t, blocks)
	if len(blocks) != 3 || !strings.Contains(got, cardAskAction+"merge") || strings.Contains(got, "Hold #41") || strings.Contains(got, "Half the fleet") {
		t.Fatalf("blocks %s, want the title, the open ask and the outcome", got)
	}
	_, forgotten := settledMessage(card.replyBlocks(view()), v.tapped)
	if got := blocksJSON(t, forgotten); len(forgotten) != 4 || !strings.Contains(got, cardAskAction+"merge") || !strings.Contains(got, cardAskAction+"hold") || strings.Contains(got, "Half the fleet") {
		t.Fatalf("blocks %s, want a forgotten card to keep the asks it still offers", got)
	}
}

func TestACardWhoseTitleIsOnlyACommentSettlesWithAHeader(t *testing.T) {
	v := view()
	v.tapped = "✓ *No* · 5:05pm"
	card := statusCard{Status: "needs_you", Title: "<!-- cci 1 -->", Options: []string{"Yes", "No"}, Recommended: "Yes", Waits: "w"}
	if err := checkBlocks(card.replyBlocks(v)); err != nil {
		t.Fatalf("Slack would refuse it: %v", err)
	}
	_, forgotten := settledMessage([]slack.Block{header(":raising_hand:"), slack.NewActionBlock(cardActionsBlock)}, v.tapped)
	if err := checkBlocks(forgotten); err != nil {
		t.Fatalf("Slack would refuse it: %v", err)
	}
}

func TestASettledPRCardLinksItsPROnce(t *testing.T) {
	card := mergeRowCard().Card
	v := view()
	v.tapped = "✓ *Merge* · answered 5:05pm"
	v.refs = map[string]refState{"pr:acme/app#41": {State: "merged"}}
	got := blocksJSON(t, card.replyBlocks(v))
	if !strings.Contains(got, "https://github.com/acme/app/pull/41|app PR 41") || !strings.Contains(got, "🟣 landed") || strings.Contains(got, "chats load faster") {
		t.Fatalf("blocks %s, want the PR's link and state in place of its body", got)
	}
	v.tapped = "✓ " + slackLink("https://github.com/acme/app/pull/41", "Landed") + " · 5:05pm"
	if got := blocksJSON(t, card.replyBlocks(v)); strings.Count(got, "pull/41") != 1 {
		t.Fatalf("blocks %s, want one link to the PR", got)
	}
}

func TestACardWhosePRsAllEndedCollapsesToItsTitleAndOutcome(t *testing.T) {
	card := statusCard{
		Status: "needs_you", Title: "Fix: approve the chat list?", Options: []string{"Approve", "Hold"}, Recommended: "Approve", Waits: testWaits,
		PRs: []cardPR{{URL: "https://github.com/acme/app/pull/41", Surprise: "Chats load faster."}, {URL: "https://github.com/acme/app/pull/42", Surprise: "The list pages."}},
	}
	v := view()
	v.refs = map[string]refState{"pr:acme/app#41": {State: "merged"}, "pr:acme/app#42": {State: "open"}}
	if got := blocksJSON(t, card.replyBlocks(v)); !strings.Contains(got, "Chats load faster") || !strings.Contains(got, `"type":"button"`) {
		t.Fatalf("blocks %s, want the whole card with its buttons while one PR is open", got)
	}
	for state, outcome := range map[string]string{"merged": "🟣 Merged", "closed": "🟣 1 merged, ⚪ 1 closed without merging"} {
		v.refs["pr:acme/app#42"] = refState{State: state}
		blocks := card.replyBlocks(v)
		got := blocksJSON(t, blocks)
		if err := checkBlocks(blocks); err != nil || len(blocks) != 3 || !strings.Contains(got, outcome) || strings.Contains(got, "Chats load faster") || strings.Contains(got, `"type":"button"`) {
			t.Fatalf("blocks %s (%v), want the title, its PRs' lines and %q with no button", got, err, outcome)
		}
		if text := card.replyText(v); text != card.Title {
			t.Fatalf("text %q, want the title alone, since its block draws %q", text, outcome)
		}
	}
}

func TestAWithdrawalNamingAnotherPostSaysReplacedAndLinksIt(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	c := postWaiting(t, l, mergeCard())
	post := c.id.permalink(c.id.DMChannelID, "1800000009.000100", "")
	l.do(opCardAnswer, map[string]any{"waits": "step-3:merge", "url": c.url(), "label": "posted without a tap <!-- cci 442236 -->", "withdrawn": true, "via": post})
	edits := editsOf(l.slack.edited(), c.ts)
	if len(edits) == 0 || !strings.Contains(edits[len(edits)-1], "*Replaced:* posted without a tap") || !strings.Contains(edits[len(edits)-1], post+"|what replaced it") || strings.Contains(edits[len(edits)-1], "442236") {
		t.Fatalf("card edits %v, want the post linked and the comment gone", edits)
	}

	own := postWaiting(t, l, statusCard{Status: "needs_you", Title: "Hold?", Options: []string{"Hold", "Merge"}, Recommended: "Hold", Waits: "hold"})
	l.do(opCardAnswer, map[string]any{"waits": "hold", "url": own.url(), "label": "removed from the board", "withdrawn": true, "via": own.url()})
	edits = editsOf(l.slack.edited(), own.ts)
	if len(edits) == 0 || strings.Contains(edits[len(edits)-1], "what replaced it") || !strings.Contains(edits[len(edits)-1], "*Withdrawn:* removed from the board") {
		t.Fatalf("card edits %v, want it withdrawn with no link to the card itself", edits)
	}
}

func TestAnAnsweredRowFoldsToItsPRLine(t *testing.T) {
	row := prCardRow()
	row.PR.Head = "abc1234def"
	card := groupCard(row, prRow(42))
	v := view()
	v.answers = map[string]askAnswer{row.ID: {At: bodyAt, Label: "Approve"}}
	got := blocksJSON(t, card.replyBlocks(v))
	for _, want := range []string{"app PR 41", "✓ *Approve* · answered"} {
		if !strings.Contains(got, want) {
			t.Fatalf("blocks %s lack %q, want the PR's line and the answer", got, want)
		}
	}
	if strings.Contains(got, "Checked.") || strings.Contains(got, "`abc1234`") {
		t.Fatalf("blocks %s, want the answered row folded to its PR line, with no lines or commit", got)
	}
}

func TestATappedPRCardFoldsToOneLinePerPR(t *testing.T) {
	pr := fullPR(52590)
	card := statusCard{Status: "needs_you", Title: "#52590, release status names the envs", Options: []string{"Approve", "Hold"}, Recommended: "Approve", Waits: "merge-52590", PRs: []cardPR{pr}}
	v := view()
	v.tapped = "✓ *Approve* · answered 1:44am"
	blocks := card.replyBlocks(v)
	if err := checkBlocks(blocks); err != nil {
		t.Fatalf("Slack would refuse the tapped card: %v", err)
	}
	got := blocksJSON(t, blocks)
	if !strings.Contains(got, "app PR 52590") || !strings.Contains(got, "✓ *Approve* · answered") || strings.Contains(got, cardRefresher) || strings.Contains(got, "7bf3ea0") {
		t.Fatalf("blocks %s, want the PR's line and the answer, and no lines or commit", got)
	}
}

func TestPlainWritesDrawNoHiddenComment(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	l.do(opDMStatus, map[string]any{"text": "**Done** <!-- cci 442236 --> and more"})
	post := l.slack.sent()[0]
	if strings.Contains(post.text, "442236") || strings.Contains(post.blocks, "442236") {
		t.Fatalf("a paged status post drew its comment: %+v", post)
	}
	l.post(opEdit, map[string]string{"channel_id": testChannel, "ts": "1800000000.000100", "text": "fixed <!-- cci 442236 -->"})
	if edits := l.slack.edited(); len(edits) != 1 || edits[0].text != "fixed" {
		t.Fatalf("edits = %+v, want the comment gone", edits)
	}
}

func TestAMarkdownStatusLineReadsBackOnce(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	l.do(opDMStatus, map[string]any{"text": "**The relay fix landed.** It deletes nothing."})
	post := l.slack.sent()[0]
	var blocks slack.Blocks
	if err := json.Unmarshal([]byte(post.blocks), &blocks); err != nil {
		t.Fatal(err)
	}
	read := messageText(slack.Message{Msg: slack.Msg{Text: post.text, Blocks: blocks}})
	if want := "*The relay fix landed.* It deletes nothing."; post.text != want || read != want {
		t.Fatalf("posted text %q reads back as %q, want %q once", post.text, read, want)
	}
}

func TestTheLinesReadBackNamesEachLineSlackDoesNotHold(t *testing.T) {
	card := statusCard{Status: "needs_you", Title: "Approve both?", Options: []string{"Approve", "Hold"}, Recommended: "Approve", Waits: "merge-1", PRs: []cardPR{fullPR(1), fullPR(2)}}
	held := card.replyBlocks(view())
	if missing := linesLacking(held, card); len(missing) != 0 {
		t.Fatalf("missing = %q from the card as drawn, want none", missing)
	}
	noSurprise := make([]slack.Block, 0, len(held))
	for _, b := range held {
		if s, ok := b.(*slack.SectionBlock); ok && s.Text != nil && strings.Contains(s.Text.Text, "*"+cardSurprise+"*") && strings.Contains(s.Text.Text, fullPR(1).Refresher) {
			continue
		}
		noSurprise = append(noSurprise, b)
	}
	if missing := linesLacking(noSurprise, card); !slices.Contains(missing, cardSurprise) {
		t.Fatalf("missing = %q, want %s named", missing, cardSurprise)
	}
	v := view()
	v.tapped = "✓ *Approve* · answered 1:44am"
	settled := sectionText(card.replyBlocks(v))
	if strings.Contains(settled, "*"+cardRefresher+"*") || !strings.Contains(settled, fullPR(1).URL) || !strings.Contains(settled, fullPR(2).URL) {
		t.Fatalf("tapped card says %q, want it folded to one line per PR", settled)
	}
}

func TestAPostedPRCardThatSlackHoldsWholeWarnsNothing(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	card := statusCard{Status: "needs_you", Title: "Approve #49622?", Options: []string{"Approve", "Hold"}, Recommended: "Approve", Waits: "merge-49622", PRs: []cardPR{fullPR(49622)}}
	var sent map[string]string
	if err := json.Unmarshal(l.do(opDMStatus, map[string]any{"card": card}).Body, &sent); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sent["warning"], "NO LINES") {
		t.Fatalf("warning = %q, want none for a card Slack holds whole", sent["warning"])
	}
}
