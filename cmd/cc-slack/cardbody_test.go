package main

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/slack-go/slack"
	"github.com/yasyf/cc-interact/agent"
)

var bodyAt = time.Date(2026, 11, 4, 8, 30, 0, 0, time.UTC)

func view() cardView { return cardView{at: bodyAt, now: bodyAt} }

func asSection(t *testing.T, b slack.Block) *slack.SectionBlock {
	t.Helper()
	s, ok := b.(*slack.SectionBlock)
	if !ok {
		t.Fatalf("block %T, want a section", b)
	}
	return s
}

func sectionTexts(blocks []slack.Block) []string {
	var texts []string
	for _, b := range blocks {
		if s, ok := b.(*slack.SectionBlock); ok && s.Text != nil {
			texts = append(texts, s.Text.Text)
		}
	}
	return texts
}

func tables(blocks []slack.Block) []*slack.DataTableBlock {
	var found []*slack.DataTableBlock
	for _, b := range blocks {
		if table, ok := b.(*slack.DataTableBlock); ok {
			found = append(found, table)
		}
	}
	return found
}

func fullPR(n int) cardPR {
	return cardPR{
		URL: fmt.Sprintf("https://github.com/acme/app/pull/%d", n), Title: "One credential per team", Head: "7bf3ea0123",
		Surprise: "Launches stop waiting on a new credential", Plan: "Nothing differed", Refresher: "A stale credential on disk", Blocks: "Your approval",
	}
}

func TestABodyRendersItsMarkdownAsBlocksNeverAsRawMarks(t *testing.T) {
	body := "## Storage\n**Fixed** in [the PR](https://github.com/acme/app/pull/2).\n- one\n- two\n\n" +
		"| check | state |\n|---|---|\n| **lint** | [green](https://ci.example.com/1) |\n| tests | |\n\nDone."
	pages := bodyPages(body, cardMaxBlocks, cardMaxVisible, true, nil)
	if len(pages) != 1 {
		t.Fatalf("pages = %d, want the short body on one", len(pages))
	}
	got := strings.NewReplacer(`\u003c`, "<", `\u003e`, ">").Replace(blocksJSON(t, pages[0]))
	for _, raw := range []string{"**", "](http", "## ", "|---|", "| check"} {
		if strings.Contains(got, raw) {
			t.Fatalf("blocks %s carry the raw mark %q", got, raw)
		}
	}
	for _, want := range []string{"*Storage*", "*Fixed* in <https://github.com/acme/app/pull/2|the PR>", "• one", `"type":"data_table"`, `"url":"https://ci.example.com/1"`, `"bold":true`, cardEmptyCell} {
		if !strings.Contains(got, want) {
			t.Fatalf("blocks %s lack %q", got, want)
		}
	}
	table := tables(pages[0])
	if len(table) != 1 || len(table[0].Rows) != 3 || len(table[0].Rows[0]) != 2 {
		t.Fatalf("tables = %+v, want one of three rows and two columns with the rule row gone", table)
	}
}

func TestEveryTableInABodyDrawsAsItsOwnDataTable(t *testing.T) {
	body := "| Who creates it | Network call per run | Who can make a database |\n|---|---|---|\n| Run's own writer | none | only the run |\n| API | one call | only the API |\n\n" +
		"Between.\n\n| Started by | Acts as |\n|---|---|\n| a run | run:<run id> |\n| a cloud agent | env:<env id> |"
	blocks := bodyPages(body, cardMaxBlocks, cardMaxVisible, true, nil)[0]
	if err := checkBlocks(blocks); err != nil {
		t.Fatalf("Slack would refuse the body: %v", err)
	}
	drawn := tables(blocks)
	if len(blocks) != 3 || len(drawn) != 2 || len(drawn[0].Rows[0]) != 3 || len(drawn[1].Rows[0]) != 2 || !slices.Equal(sectionTexts(blocks), []string{"Between."}) {
		t.Fatalf("blocks %s, want both tables as data tables around the line between them", blocksJSON(t, blocks))
	}
	if cell, ok := drawn[1].Rows[1][1].(*slack.DataTableRawTextCell); !ok || cell.Text != "run:<run id>" {
		t.Fatalf("cell = %#v, want the second table's text as it was written", drawn[1].Rows[1][1])
	}
}

func TestATableSlackWouldRefuseDrawsAsLines(t *testing.T) {
	wide := strings.Repeat("x", slackMaxTable/(2*(cardMaxTableRows-1))+1)
	rows := append([]string{"| a | b |", "|---|---|"}, slices.Repeat([]string{"| " + wide + " | " + wide + " |"}, cardMaxTableRows-1)...)
	for name, body := range map[string]string{
		"past 20,000 characters": strings.Join(rows, "\n"),
		"a header alone":         "| a | b |\n|---|---|",
	} {
		pages := bodyPages(body, cardMaxBlocks, cardMaxVisible, true, nil)
		if drawn := tables(slices.Concat(pages...)); len(drawn) != 0 {
			t.Errorf("%s: %d data tables, want lines, since Slack refuses a table past %d characters or without a row under its header", name, len(drawn), slackMaxTable)
		}
	}
}

func TestAPictureOnALineOfItsOwnDrawsWhereItSitsAndAnyOtherStaysText(t *testing.T) {
	queue := slack.NewImageBlock("https://bucket.example.com/queue.png", "The queue after", "", nil)
	body := "Before the picture.\n![The queue after](/shots/queue.png)\nInline ![x](/shots/x.png) stays.\n\n" +
		"![The board](<https://example.com/my board.png>)\n\n```\n![in a fence](/shots/f.png)\n```"
	blocks := bodyPages(body, cardMaxBlocks, cardMaxVisible, true, map[string]*slack.ImageBlock{"/shots/queue.png": queue})[0]
	if len(blocks) != 5 || blocks[1] != slack.Block(queue) {
		t.Fatalf("blocks %s, want the drawn picture second, between the lines around it", blocksJSON(t, blocks))
	}
	want := []string{"Before the picture.", "Inline ![x](/shots/x.png) stays.", "<https://example.com/my board.png|The board>", "```\n![in a fence](/shots/f.png)\n```"}
	if got := sectionTexts(blocks); !slices.Equal(got, want) {
		t.Fatalf("sections %q, want %q: an inline image and a fenced one stay text, and an undrawn picture shows its caption", got, want)
	}
}

func TestATableCellKeepsALinkCCSlackAlreadyMade(t *testing.T) {
	cells := tableCells("| <https://github.com/acme/app/pull/2|#2> | landed |")
	if !slices.Equal(cells, []string{"<https://github.com/acme/app/pull/2|#2>", "landed"}) {
		t.Fatalf("cells = %q, want the link's pipe kept inside its cell", cells)
	}
}

func TestALongBodySplitsAtItsLimitsAndLosesNoWord(t *testing.T) {
	words := make([]string, 0, 6000)
	for i := range 6000 {
		words = append(words, fmt.Sprintf("word%d", i))
	}
	body := strings.Join(words, " ") + "\n\n<https://example.com/a|a link with spaces in its label>"
	pages := bodyPages(body, 40, cardMaxVisible, true, nil)
	if len(pages) < 2 {
		t.Fatalf("pages = %d, want what passes the card's budget folded", len(pages))
	}
	var seen strings.Builder
	for _, page := range pages {
		if len(page) > cardMaxBlocks {
			t.Fatalf("a page holds %d blocks, past Slack's %d", len(page), cardMaxBlocks)
		}
		for _, text := range sectionTexts(page) {
			if runes(text) > cardMaxSectionLen {
				t.Fatalf("a section runs %d characters, past Slack's %d", runes(text), cardMaxSectionLen)
			}
			if strings.Count(text, "<") != strings.Count(text, ">") {
				t.Fatalf("a section cut a link in two: %q", text[max(0, len(text)-80):])
			}
			seen.WriteString(text + " ")
		}
	}
	got := strings.Fields(seen.String())
	if !slices.Equal(got[:len(words)], words) {
		t.Fatal("a word was lost or reordered across the pages")
	}
}

func TestALongCodeBlockSplitsIntoClosedCodeBlocks(t *testing.T) {
	body := "```\n" + strings.Repeat("a line of output\n", 400) + "```"
	for _, text := range sectionTexts(bodyPages(body, cardMaxBlocks, cardMaxVisible, true, nil)[0]) {
		if !strings.HasPrefix(text, "```\n") || !strings.HasSuffix(text, "\n```") || runes(text) > cardMaxSectionLen {
			t.Fatalf("section %d runes opens %q and closes %q, want a closed code block under the limit", runes(text), text[:6], text[len(text)-6:])
		}
	}
}

func TestACardOverABudgetFoldsBehindExplainMoreAndNeverCuts(t *testing.T) {
	body := strings.Repeat("A paragraph that goes on and on about the design.\n\n", 400)
	card := statusCard{Status: "info", Title: "Storage eviction", Body: body}
	if err := card.check(bodyAt, false, ""); err != nil {
		t.Fatalf("a long body refused: %v", err)
	}
	blocks, fold := card.layout(view())
	if len(blocks) > cardMaxBlocks || len(fold) == 0 {
		t.Fatalf("%d blocks and %d folded pages, want the rest folded", len(blocks), len(fold))
	}
	if got := blocksJSON(t, blocks); !strings.Contains(got, cardFoldAction) || strings.Contains(got, "…") {
		t.Fatalf("blocks %s, want an Explain more button and nothing cut", got)
	}
	shown := strings.Count(blocksJSON(t, blocks), "paragraph") + strings.Count(blocksJSON(t, slices.Concat(fold...)), "paragraph")
	if shown != 400 {
		t.Fatalf("%d paragraphs across the card and its fold, want all 400", shown)
	}
	v := view()
	v.unfolded = true
	if got := blocksJSON(t, card.replyBlocks(v)); strings.Contains(got, cardFoldAction) {
		t.Fatalf("blocks %s still offer the fold they already showed", got)
	}
	if k, ok := (cardActionPush{ActionID: cardFoldAction}).tap(); !ok || !k.Fold || k.ID != "" {
		t.Fatalf("fold tap = %+v, %v", k, ok)
	}
}

func blockLead(t *testing.T, b slack.Block) string {
	t.Helper()
	switch block := b.(type) {
	case *slack.HeaderBlock:
		return block.Text.Text
	case *slack.SectionBlock:
		if block.Text == nil {
			return block.Fields[0].Text
		}
		return block.Text.Text
	case *slack.ContextBlock:
		text, ok := block.ContextElements.Elements[0].(*slack.TextBlockObject)
		if !ok {
			t.Fatalf("context element %T, want text", block.ContextElements.Elements[0])
		}
		return text.Text
	case *slack.ActionBlock:
		return block.BlockID
	case *slack.DividerBlock:
		return ""
	}
	t.Fatalf("no lead for a %T", b)
	return ""
}

func TestAPRCardStopsAtItsFirstScreen(t *testing.T) {
	pr := fullPR(50659)
	pr.Description = "## Context\n\nWhy it happened.\n\n## Summary\n\nWhat it does.\n\n## Full change list\n\n- `cancel.go`: the fix"
	card := statusCard{
		Status: "needs_you", Title: "Keep it or revert it?", Body: "It landed with no tap from you.",
		Options: []string{"Keep it", "Revert it now"}, Recommended: "Keep it", Waits: "m50659", PRs: []cardPR{pr},
	}
	blocks, fold := card.layout(view())
	want := []string{
		"🙋 Keep it or revert it?", "It landed with no tap from you.", "", "*One credential per team*",
		"*Refresher*\nA stale credential on disk", "*1. Keep it*", "*2. Revert it now*", cardPRHint, cardActionsBlock, "",
	}
	if len(blocks) != len(want) {
		t.Fatalf("%d blocks %s, want %d: the first screen and nothing past it", len(blocks), blocksJSON(t, blocks), len(want))
	}
	for i, lead := range want {
		if got := blockLead(t, blocks[i]); !strings.HasPrefix(got, lead) {
			t.Errorf("block %d = %q, want it to start %q", i, got, lead)
		}
	}
	folded := blocksJSON(t, slices.Concat(fold...))
	for _, section := range []string{"pull/50659", "*Context*", "Why it happened.", "*Summary*", "What it does.", "*Full change list*", "`cancel.go`: the fix"} {
		if !strings.Contains(folded, section) {
			t.Errorf("fold %s lacks %q", folded, section)
		}
	}
}

func TestExplainMoreOnAWaitingPRCardPostsItsDescriptionThenAsksTheLane(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	pr := fullPR(7)
	pr.Description = "## Context\n\nWhy it happened."
	card := statusCard{Status: "needs_you", Title: "Approve?", Options: []string{"Approve", "Hold"}, Recommended: "Approve", Waits: "m7", PRs: []cardPR{pr}}
	l.do(opDMStatus, map[string]any{"card": card})
	posted := l.slack.sent()[0]
	if strings.Contains(posted.blocks, "Why it happened.") {
		t.Fatalf("card %s, want its description behind Explain more", posted.blocks)
	}
	id, err := loadIdentity(l.ctx, l.srv.DB())
	if err != nil {
		t.Fatal(err)
	}
	push := cardActionPush{User: testUser, Channel: id.DMChannelID, MessageTS: posted.ts, ThreadTS: posted.ts,
		ActionID: cardExplainAction + "m7", Value: string(mustJSON(askTap{ID: "m7", Label: cardExplainLabel, Question: card.Title})),
		ActionTS: fmt.Sprintf("%d.000050", time.Now().Unix()), Seq: "001800000100000-abcd0300"}
	l.tap(id, push)
	posts := l.slack.sent()
	if len(posts) != 2 || posts[1].threadTS != posted.ts || !strings.Contains(posts[1].blocks, "Why it happened.") {
		t.Fatalf("posts = %+v, want the description in the card's thread", posts)
	}
	if got := l.inject(agent.TopLevel); strings.Contains(got, "explain") {
		t.Fatalf("directive = %q, want no lane asked for what the card already holds", got)
	}
	push.ActionTS, push.Seq = fmt.Sprintf("%d.000100", time.Now().Unix()), "001800000100000-abcd0301"
	l.tap(id, push)
	if got := l.inject(agent.TopLevel); !strings.Contains(got, `explain the waiting ask "m7" more`) {
		t.Fatalf("directive = %q, want a second tap to ask the lane", got)
	}
}

func TestAPRCardThatNoLongerWaitsOnHisTapSaysWhyUnderItsPR(t *testing.T) {
	pr := fullPR(7)
	pr.Blocks = "A reader's check found it not ready: Releasable alone needs #43 live first"
	card := statusCard{Status: "info", Title: "Fix: retry flaky uploads", Waits: "merge-7", Options: []string{"Approve", "Hold"}, PRs: []cardPR{pr}}
	v := view()
	v.tapped = cardNoLongerWaits
	got := blocksJSON(t, card.replyBlocks(v))
	if !strings.Contains(got, cardNoLongerWaits) || !strings.Contains(got, "found it not ready: Releasable alone needs #43 live first") || strings.Contains(got, `"Approve"`) {
		t.Fatalf("card %s, want why it no longer waits under its PR and no Approve", got)
	}
	v.tapped = "✓ *Approve* · answered"
	if got := blocksJSON(t, card.replyBlocks(v)); strings.Contains(got, "found it not ready") {
		t.Fatalf("card %s, want a card he answered to keep its settled line alone", got)
	}
}

func TestAPRCardShowsItsLinkCIAndFourLinesAndFoldsItsDescription(t *testing.T) {
	pr := fullPR(49622)
	pr.Description = "It keeps one storage credential per team."
	card := statusCard{Status: "needs_you", Title: "Approve the team credential?", Options: []string{"Approve", "Hold"}, Recommended: "Approve", Waits: "merge-49622", PRs: []cardPR{pr}}
	if err := card.check(bodyAt, false, ""); err != nil {
		t.Fatalf("a whole PR card refused: %v", err)
	}
	id, _ := parseRef(pr.URL)
	v := view()
	v.refs = map[string]refState{id.key(): {State: "open", Checks: "passed"}}
	blocks := card.replyBlocks(v)
	if _, ok := blocks[0].(*slack.HeaderBlock); !ok {
		t.Fatalf("first block %T, want the header", blocks[0])
	}
	if _, ok := blocks[1].(*slack.DividerBlock); !ok {
		t.Fatalf("second block %T, want a divider opening the PR's section", blocks[1])
	}
	head := asSection(t, blocks[2])
	if head.Text.Text != "*One credential per team*\n<https://github.com/acme/app/pull/49622|app PR 49622>" || head.Accessory == nil || head.Accessory.ButtonElement.URL != pr.URL {
		t.Fatalf("third block %+v, want the PR's title and link first, with its Open button", head)
	}
	said := asSection(t, blocks[3]).Text.Text
	if !strings.HasPrefix(said, "*Refresher*\nA stale credential on disk") || strings.Contains(said, "*Where it stands*") {
		t.Fatalf("fourth block %q, want the refresher first and no Where it stands while CI passed", said)
	}
	for _, want := range []string{"*Biggest surprise*\nLaunches", "*Waits on*\nYour", "*Differs from the plan*\nNothing"} {
		if !strings.Contains(said, want) {
			t.Fatalf("fourth block %q lacks %q", said, want)
		}
	}
	got := blocksJSON(t, blocks)
	if strings.Contains(got, "It keeps one storage credential per team.") || strings.Contains(sectionText(blocks)+pr.headline(0, 1), "7bf3ea0") {
		t.Fatalf("blocks %s, want the PR's description behind Explain more and no commit", got)
	}
	buttons := cardButtons(t, blocks)[1:]
	if buttons[0].Text.Text != "Approve (Recommended)" || buttons[1].Text.Text != "Hold" {
		t.Fatalf("buttons %q %q, want Approve and Hold", buttons[0].Text.Text, buttons[1].Text.Text)
	}
	if ids := card.refIDs(); len(ids) != 1 || ids[0] != id {
		t.Fatalf("refs = %+v, want the PR kept live so its CI state redraws", ids)
	}
}

func TestAPRSectionOpensWithItsSummaryBeforeItsRefresher(t *testing.T) {
	pr := fullPR(49624)
	pr.Summary = "store.Credentials keeps one row per team in team_credentials, and launch reads it instead of minting one."
	card := statusCard{Status: "needs_you", Title: "Approve the team credential?", Options: []string{"Approve", "Hold"}, Recommended: "Approve", Waits: "merge-49624", PRs: []cardPR{pr}}
	if err := card.check(bodyAt, false, ""); err != nil {
		t.Fatalf("a PR card with a summary refused: %v", err)
	}
	said := asSection(t, card.replyBlocks(view())[3]).Text.Text
	if !strings.HasPrefix(said, "*Summary*\nstore.Credentials keeps one row per team") || strings.Index(said, "*Refresher*") < strings.Index(said, "*Summary*") {
		t.Fatalf("fourth block %q, want the summary first and the refresher after it", said)
	}
	if lines := pr.lines(); lines[0] != (cardFact{cardSummary, "*Summary*"}) {
		t.Fatalf("lines %+v, want the summary's line read back first", lines)
	}
}

func TestAPRCardCarryingItsLedgerReviewShowsOnlyThatAndNoLiveGitHubState(t *testing.T) {
	pr := fullPR(49623)
	pr.Review = "CI red; approved by poetic-svc; grade pass"
	card := statusCard{Status: "needs_you", Title: "Approve the team credential?", Options: []string{"Approve", "Hold"}, Recommended: "Approve", Waits: "merge-49623", PRs: []cardPR{pr}}
	id, _ := parseRef(pr.URL)
	v := view()
	v.refs = map[string]refState{id.key(): {State: "open", Checks: "passed"}}
	stands := asSection(t, card.replyBlocks(v)[3]).Text.Text
	if strings.Contains(stands, "CI passed") || !strings.Contains(stands, "*Where it stands*\nCI red; approved by poetic-svc") {
		t.Fatalf("where it stands %q, want the ledger's review alone, never GitHub's live state beside it", stands)
	}
}

func TestAPRCardMissingALinePostsWithTheLinesItHas(t *testing.T) {
	pr := fullPR(49722)
	pr.Refresher, pr.Blocks = "", " "
	for name, card := range map[string]statusCard{
		"card": {Status: "needs_you", Title: "x", PRs: []cardPR{pr}},
		"row":  {Status: "needs_you", Title: "x", Group: "g", Rows: []cardRow{{ID: "r", PR: &pr, Buttons: []string{"Approve", "Hold"}}}},
	} {
		if err := card.check(bodyAt, false, ""); err != nil {
			t.Fatalf("%s: check = %v, want the card to post", name, err)
		}
		blocks := card.replyBlocks(view())
		if err := checkBlocks(blocks); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got := blocksJSON(t, blocks)
		if !strings.Contains(got, "*Biggest surprise*") || !strings.Contains(got, "*Differs from the plan*") || strings.Contains(got, "A stale credential") || strings.Contains(got, "*Waits on*") {
			t.Fatalf("%s: blocks %s, want the two lines it has and no empty field", name, got)
		}
	}
	bare := cardPR{URL: pr.URL}
	if err := checkBlocks(bare.blocks(view(), false)); err != nil {
		t.Fatalf("a PR with no lines at all draws a block Slack refuses: %v", err)
	}
}

func TestAnOverLongLineOrHintPostsWholeInSections(t *testing.T) {
	pr := fullPR(7)
	pr.Refresher = strings.Repeat("An explainer worth a long read. ", 100)
	long := []string{"Keep it", "Drop it"}
	card := statusCard{Status: "needs_you", Title: "x", Options: long, Recommended: long[0], Waits: "w", PRs: []cardPR{pr}, Hints: map[string]string{long[1]: strings.Repeat("A reason to drop it. ", 200)}}
	if err := card.check(bodyAt, false, ""); err != nil {
		t.Fatalf("check = %v, want the card to post", err)
	}
	blocks := card.replyBlocks(view())
	if err := checkBlocks(blocks); err != nil {
		t.Fatal(err)
	}
	got := blocksJSON(t, blocks)
	if strings.Count(got, "An explainer worth") != 100 || strings.Count(got, "A reason to drop") != 200 {
		t.Fatalf("a line or hint was cut: %d of 100, %d of 200", strings.Count(got, "An explainer worth"), strings.Count(got, "A reason to drop"))
	}
}

func TestAPRCardWithNoGitHubToReadStillPosts(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	l.do(opDMStatus, map[string]any{"card": statusCard{Status: "info", Title: "The relay fix", PRs: []cardPR{fullPR(7)}}})
	posts := l.slack.sent()
	if len(posts) != 1 || !strings.Contains(posts[0].blocks, cardSurprise) {
		t.Fatalf("posts = %+v, want the card out with its lines", posts)
	}
}

func TestAPRDescriptionDropsCommentsAndFolds(t *testing.T) {
	got := prDescription("<!-- bot -->Summary\n<details><summary>Changes</summary>\nA file\n</details>\n")
	if got != "Summary\n**Changes**\nA file" {
		t.Fatalf("description = %q", got)
	}
}

func TestAPRDescriptionFromTheLedgerDrawsWithoutItsHTML(t *testing.T) {
	pr := fullPR(7)
	pr.Description = "<!-- bot -->Why it matters\n\n<details><summary>Changes</summary>\nA file\n</details>\n"
	card := statusCard{Status: "info", Title: "Status", PRs: []cardPR{pr}}
	blocks, fold := card.layout(view())
	if shown := blocksJSON(t, blocks); !strings.Contains(shown, cardFoldAction) {
		t.Fatalf("blocks %s, want an Explain more button on a card that waits on nothing", shown)
	}
	got := blocksJSON(t, slices.Concat(fold...))
	for _, gone := range []string{"details", "summary", "-- bot"} {
		if strings.Contains(got, gone) {
			t.Fatalf("blocks %s still show %q", got, gone)
		}
	}
	if !strings.Contains(got, "Why it matters") || !strings.Contains(got, "*Changes*") || !strings.Contains(got, "A file") {
		t.Fatalf("blocks %s, want the description's words kept", got)
	}
}

func TestALongPRDescriptionWaitsWholeBehindExplainMore(t *testing.T) {
	pr := fullPR(7)
	pr.Description = strings.Repeat("A sentence of the description. ", 400)
	card := statusCard{Status: "needs_you", Title: "Approve?", Options: []string{"Approve", "Hold"}, Recommended: "Approve", Waits: "m7", PRs: []cardPR{pr}}
	blocks, fold := card.layout(view())
	if err := checkBlocks(blocks); err != nil {
		t.Fatal(err)
	}
	for _, page := range fold {
		if err := checkBlocks(page); err != nil {
			t.Fatal(err)
		}
	}
	if got := strings.Count(blocksJSON(t, slices.Concat(fold...)), "description."); got != 400 {
		t.Fatalf("%d of 400 sentences behind Explain more", got)
	}
	got := blocksJSON(t, blocks)
	if strings.Contains(got, "description.") {
		t.Fatalf("blocks %s, want no sentence on the card", got)
	}
	if !strings.Contains(got, cardExplainAction+"m7") || strings.Contains(got, cardFoldAction) {
		t.Fatalf("blocks %s, want the waiting card's own Explain more to show the fold", got)
	}
}

func TestAGroupCardOfPRRowsFitsAMessage(t *testing.T) {
	rows := make([]cardRow, cardMaxPRRows)
	for i := range rows {
		pr := fullPR(100 + i)
		pr.Description = strings.Repeat("Detail. ", 200)
		rows[i] = cardRow{ID: fmt.Sprintf("pg:%d:abc", 100+i), PR: &pr, Buttons: []string{"Approve", "Hold"}, Text: "One tap also decides #5"}
	}
	card := statusCard{Status: "needs_you", Title: "Login", Body: "People can sign in again.", Group: "pg:login", Rows: rows}
	if err := card.check(bodyAt, false, ""); err != nil {
		t.Fatal(err)
	}
	blocks, fold := card.layout(view())
	if len(blocks) > cardMaxBlocks || len(fold) != cardMaxPRRows {
		t.Fatalf("%d blocks and %d folds, want the card under Slack's %d with one fold per description", len(blocks), len(fold), cardMaxBlocks)
	}
	if got := blocksJSON(t, blocks); strings.Contains(got, "Detail.") {
		t.Fatalf("blocks %s, want each row's description behind Explain more", got)
	}
	wordy := fullPR(9)
	wordy.Surprise = strings.Repeat("words ", cardMaxVisible/6)
	over := statusCard{Status: "needs_you", Title: "Login", Group: "pg:login", Rows: append(slices.Clone(rows), cardRow{ID: "pg:9:abc", PR: &wordy, Buttons: []string{"Hold"}})}
	if err := over.check(bodyAt, false, ""); err == nil {
		t.Fatal("a group card past the PR-row limit whose words overflow a screen passed its check")
	}
}

func TestOptionsWithWordsToReadListOnceEachWithItsOwnButton(t *testing.T) {
	long := []string{"Cap the run search at 200 sessions", "Run the whole search tonight"}
	card := statusCard{Status: "needs_you", Title: "How should the run search pay for itself?", Options: long, Recommended: long[1], Waits: "cost", Hints: map[string]string{long[0]: "Drops the rest."}}
	if err := card.check(bodyAt, false, ""); err != nil {
		t.Fatal(err)
	}
	blocks := card.replyBlocks(view())
	rows := slices.DeleteFunc(slices.Clone(blocks), func(b slack.Block) bool { return !strings.HasPrefix(b.ID(), cardOptionBlock) })
	if len(rows) != 2 {
		t.Fatalf("%d option rows, want one per option", len(rows))
	}
	first := asSection(t, rows[0])
	if !strings.Contains(first.Text.Text, long[1]) || !strings.Contains(first.Text.Text, cardRecommended) || first.Accessory == nil || first.Accessory.ButtonElement.Style != slack.StylePrimary {
		t.Fatalf("first row %+v, want the recommendation first with a primary button", first)
	}
	if second := asSection(t, rows[1]); !strings.Contains(second.Text.Text, "Drops the rest.") {
		t.Fatalf("second row %q, want its hint under it", second.Text.Text)
	}
	v := view()
	v.tapped = "✓ picked"
	for _, b := range card.replyBlocks(v) {
		if row, ok := b.(*slack.SectionBlock); ok && strings.HasPrefix(row.BlockID, cardOptionBlock) && row.Accessory != nil {
			t.Fatalf("answered row %q still offers its button", row.Text.Text)
		}
	}
	_, stripped := settledMessage(blocks, "✓ picked")
	if strings.Contains(blocksJSON(t, stripped), cardWaitAction) {
		t.Fatalf("a card outside the live store keeps its pick buttons after a tap: %s", blocksJSON(t, stripped))
	}
}

func TestAReplacementCardCarriesNoButtons(t *testing.T) {
	const posted = "https://acme.slack.com/archives/D0B6Z/p1793635190051179"
	card := statusCard{Status: "done", Title: "The relay fix landed", Body: "It merged.", Replaces: posted}
	if err := card.check(bodyAt, false, ""); err != nil {
		t.Fatal(err)
	}
	v := view()
	v.final = true
	if buttons := cardButtons(t, card.replyBlocks(v)); len(buttons) != 0 {
		t.Fatalf("buttons = %d, want none on a card that replaces an ask", len(buttons))
	}
	for name, bad := range map[string]statusCard{
		"no permalink": {Status: "done", Title: "x", Replaces: "1793635190.051179"},
		"with options": {Status: "done", Title: "x", Replaces: posted, Options: []string{"go"}},
		"with a key":   {Status: "done", Title: "x", Replaces: posted, Key: "k"},
	} {
		if err := bad.check(bodyAt, false, ""); err == nil {
			t.Fatalf("%s passed its check", name)
		}
	}
}

func TestAnEditOfAnAskReplacesTheSlackCopyInPlaceAndPostsNothingNew(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	ask := statusCard{Status: "needs_you", Title: "Land the relay fix?", Options: []string{"Land", "Hold"}, Recommended: "Land", Waits: "card-r6"}
	l.do(opDMStatus, map[string]any{"card": ask})
	first := l.slack.sent()[0]
	id, err := loadIdentity(l.ctx, l.srv.DB())
	if err != nil {
		t.Fatal(err)
	}
	permalink := id.permalink(id.DMChannelID, first.ts, first.ts)

	l.do(opDMStatus, map[string]any{"card": statusCard{Status: "done", Title: "The relay fix landed", Body: "It **merged**.", Replaces: permalink}})

	if posts := l.slack.sent(); len(posts) != 1 {
		t.Fatalf("posts = %d, want the edit to post nothing new", len(posts))
	}
	edits := l.slack.edited()
	if len(edits) == 0 || edits[len(edits)-1].ts != first.ts {
		t.Fatalf("edits = %+v, want the first card edited", edits)
	}
	last := edits[len(edits)-1].blocks
	if !strings.Contains(last, "The relay fix landed") || !strings.Contains(last, "*merged*") || strings.Contains(last, cardWaitAction) || strings.Contains(last, "Land the relay fix?") {
		t.Fatalf("edit %s, want the new title and body rendered and the buttons gone", last)
	}
}

func TestAWaitingCardThatReplacesAPostTakesItsPlaceWithItsButtons(t *testing.T) {
	for name, parent := range map[string]bool{"at the root": false, "in a thread": true} {
		t.Run(name, func(t *testing.T) {
			l := startDaemon(t)
			loggedIn(t, l)
			body := map[string]any{"text": "The relay fix is ready."}
			thread := ""
			if parent {
				var root map[string]string
				if err := json.Unmarshal(l.do(opDMStatus, map[string]any{"text": "The relay fix."}).Body, &root); err != nil {
					t.Fatal(err)
				}
				thread = root["ts"]
				body["thread_ts"] = thread
			}
			var first map[string]string
			if err := json.Unmarshal(l.do(opDMStatus, body).Body, &first); err != nil {
				t.Fatal(err)
			}
			before := len(l.slack.sent())
			id, err := loadIdentity(l.ctx, l.srv.DB())
			if err != nil {
				t.Fatal(err)
			}
			ask := statusCard{Status: "needs_you", Title: "Land the relay fix?", Options: []string{"Land", "Hold"}, Recommended: "Land", Waits: "card-r6", Replaces: id.permalink(id.DMChannelID, first["ts"], thread)}
			var drawn map[string]string
			if err := json.Unmarshal(l.do(opDMStatus, map[string]any{"card": ask}).Body, &drawn); err != nil {
				t.Fatal(err)
			}
			if drawn["ts"] != first["ts"] {
				t.Fatalf("card at %s, want it on the first message %s", drawn["ts"], first["ts"])
			}
			edits := l.slack.edited()
			if len(edits) == 0 || edits[len(edits)-1].ts != first["ts"] || !strings.Contains(edits[len(edits)-1].blocks, cardWaitAction) {
				t.Fatalf("edits = %+v, want the first message edited to carry the buttons", edits)
			}

			ask.Title = "Land the relay fix tonight?"
			l.do(opDMStatus, map[string]any{"card": ask})
			edits = l.slack.edited()
			sent := l.slack.sent()
			if len(sent) != before+1 || !strings.Contains(sent[before].blocks, "tonight") || !strings.Contains(sent[before].blocks, cardWaitAction) {
				t.Fatalf("posts = %+v, want the new words posted once with their buttons", sent[before:])
			}
			if last := edits[len(edits)-1]; last.ts != first["ts"] || !strings.Contains(last.blocks, "tonight") || strings.Contains(last.blocks, cardWaitAction) || !strings.Contains(last.blocks, cardRewordedLine) {
				t.Fatalf("edits = %+v, want the first message redrawn with the new words and a line in place of its buttons", edits)
			}
		})
	}
}

func TestAWaitingCardThatReplacesAMessageGoneFromSlackFailsAndPostsNothing(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	id, err := loadIdentity(l.ctx, l.srv.DB())
	if err != nil {
		t.Fatal(err)
	}
	l.slack.fail("chat.update", slack.SlackErrorResponse{Err: "message_not_found"})
	ask := statusCard{Status: "needs_you", Title: "Land the relay fix?", Options: []string{"Land", "Hold"}, Recommended: "Land", Waits: "card-r7", Replaces: id.permalink(id.DMChannelID, "1800000000.000100", "")}
	if _, err := invoke(l.ctx, l.deps, testSession, "/any", opDMStatus, mustJSON(map[string]any{"card": ask})); err == nil || !strings.Contains(err.Error(), "message_not_found") {
		t.Fatalf("err = %v, want the edit's failure returned", err)
	}
	if posts := l.slack.sent(); len(posts) != 0 {
		t.Fatalf("posts = %+v, want a failed edit to post nothing", posts)
	}
}

func TestAFoldTapPostsTheRestInTheThreadWithoutWakingTheSession(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	body := strings.Repeat("A paragraph about the design.\n\n", 500)
	l.do(opDMStatus, map[string]any{"card": statusCard{Status: "info", Title: "Storage eviction", Body: body}})
	card := l.slack.sent()[0]
	id, err := loadIdentity(l.ctx, l.srv.DB())
	if err != nil {
		t.Fatal(err)
	}
	l.tap(id, cardActionPush{User: testUser, Channel: id.DMChannelID, MessageTS: card.ts, ThreadTS: card.ts, ActionID: cardFoldAction,
		ActionTS: fmt.Sprintf("%d.000050", time.Now().Unix()), Seq: "001800000100000-abcd0200"})

	posts := l.slack.sent()
	if len(posts) < 2 || posts[1].threadTS != card.ts || !strings.Contains(posts[1].blocks, "A paragraph about the design.") {
		t.Fatalf("posts = %d, want the rest in the card's thread", len(posts))
	}
	if got := l.inject(agent.TopLevel); strings.Contains(got, "explain") {
		t.Fatalf("directive = %q, want no session woken for the card's own text", got)
	}
}

func TestABodyThatLinkingGrowsPastASectionStillPosts(t *testing.T) {
	bullets := make([]string, 0, 11)
	for i := range 11 {
		bullets = append(bullets, fmt.Sprintf("**Item %d**\n- It landed in <https://github.com/acme/app/pull/%d|#%d> and is live. %s", i, 49000+i, 49000+i, strings.Repeat("Real words here. ", 12)))
	}
	body := strings.Join(bullets, "\n\n")
	if runes(body) <= cardMaxSectionLen {
		t.Fatalf("the fixture is %d characters; it must pass one section as the card that failed did", runes(body))
	}
	card := statusCard{Status: "info", Title: "Test Mac, running now", Body: body, Drive: "drive"}
	if err := checkBlocks(card.replyBlocks(view())); err != nil {
		t.Fatalf("the card Slack refused as invalid_blocks still fails: %v", err)
	}
}

func TestABlockSlackWouldRefuseIsNamedBeforeItPosts(t *testing.T) {
	long := strings.Repeat("a", slackMaxSection+1)
	section := func(text string) slack.Block {
		return slack.NewSectionBlock(slack.NewTextBlockObject(slack.MarkdownType, text, false, false), nil, nil)
	}
	head := header("A title")
	for name, c := range map[string]struct {
		blocks []slack.Block
		want   string
	}{
		"long section":  {[]slack.Block{head, section(long)}, "block 2 (section): its text is 3001 characters"},
		"empty section": {[]slack.Block{section("")}, "block 1 (section): its text is 0 characters"},
		"long field": {[]slack.Block{slack.NewSectionBlock(nil, []*slack.TextBlockObject{slack.NewTextBlockObject(slack.MarkdownType, long, false, false)}, nil)},
			"block 1 (section): a field is 3001 characters"},
		"too many": {slices.Repeat([]slack.Block{section("a")}, cardMaxBlocks+1), "the message holds 51 blocks"},
		"same id":  {[]slack.Block{section2("x"), section2("x")}, `block 2 (section): block_id "x" repeats`},
	} {
		err := checkBlocks(c.blocks)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: check = %v, want %q", name, err, c.want)
		}
	}
	if err := checkBlocks([]slack.Block{head, section("fine")}); err != nil {
		t.Fatalf("a good message was refused: %v", err)
	}
}

func section2(id string) slack.Block {
	return slack.NewSectionBlock(slack.NewTextBlockObject(slack.MarkdownType, "a", false, false), nil, nil, slack.SectionBlockOptionBlockID(id))
}

func TestACardFooterIsOneQuietLineWithNoBookkeeping(t *testing.T) {
	tag := cardTag{Session: "0123abcd", Repo: "app", Branch: "dev", Worktree: "cf-alert"}
	for name, c := range map[string]struct {
		card statusCard
		want string
	}{
		"a drive card": {statusCard{Status: "info", Title: "t", Drive: "brook", Phase: "landing"}, "brook · landing · 12:30am"},
		"a plain card": {statusCard{Status: "info", Title: "t"}, "app · 12:30am"},
	} {
		got := blocksJSON(t, []slack.Block{c.card.context(tag, bodyAt, "")})
		if !strings.Contains(got, c.want) {
			t.Errorf("%s: footer %s, want %q", name, got, c.want)
		}
		for _, internal := range []string{"0123abcd", "cf-alert", "@dev", "session", "worktree"} {
			if strings.Contains(got, internal) {
				t.Errorf("%s: footer %s shows %q", name, got, internal)
			}
		}
	}
}

func TestAStatusCardShowsItsWholeBodyAndFoldsOnlyPastTheVisibleLimit(t *testing.T) {
	sentence := "The credential landed and launches stopped waiting on a new one. "
	card := statusCard{Status: "info", Title: "Storage credential", Body: strings.Repeat(sentence, 30)}
	blocks, fold := card.layout(view())
	if got := strings.Count(blocksJSON(t, blocks), "landed"); got != 30 || len(fold) != 0 {
		t.Fatalf("%d of 30 sentences in view with %d fold pages, want all in view and none", got, len(fold))
	}
	wall := statusCard{Status: "info", Title: "Storage credential", Body: strings.Repeat(sentence, 300)}
	blocks, fold = wall.layout(view())
	if len(fold) == 0 || !strings.Contains(blocksJSON(t, blocks), cardFoldAction) {
		t.Fatalf("fold = %d pages, want the rest behind Explain more", len(fold))
	}
	if got := strings.Count(blocksJSON(t, blocks), "landed") + strings.Count(blocksJSON(t, slices.Concat(fold...)), "landed"); got != 300 {
		t.Fatalf("%d sentences across the card and its fold, want all 300", got)
	}
	table := statusCard{Status: "info", Title: "Checks", Body: "| check | state |\n|---|---|\n| " + strings.Repeat("a long cell ", 50) + " | green |"}
	if got := tables(table.replyBlocks(view())); len(got) != 1 {
		t.Fatal("a table folded away: a table is what he reads at a glance")
	}
}

func TestACutNeverEndsOnAHeading(t *testing.T) {
	body := strings.Repeat("words ", 40)
	for _, heading := range []string{"## Summary", "**Summary**", "*Summary*"} {
		shown := strings.TrimSpace(sectionCut([]rune("The intro.\n\n"+heading+"\n\n"+body), 40))
		if shown != "The intro." {
			t.Fatalf("%s: shown %q, want the heading moved to the rest", heading, shown)
		}
	}
	parts := splitSection("## Summary\n\n"+body, 40)
	if len(parts) < 2 || parts[0] == "## Summary" {
		t.Fatalf("parts = %q, want the heading kept with its body", parts)
	}
}

func TestEveryLineOfAPRFactReadsAsTheSameBullet(t *testing.T) {
	if got := plainMrkdwn("- first\n- second\n  - third"); got != "• first\n• second\n  • third" {
		t.Fatalf("got %q, want every marker converted", got)
	}
}

func TestAPRCardSaysWhatItsButtonsDoAndClaimsNoLaneStops(t *testing.T) {
	asks := statusCard{Status: "needs_you", Title: "Approve?", Options: []string{"Approve", "Hold"}, Waits: "w"}
	if got := blocksJSON(t, asks.replyBlocks(view())); !strings.Contains(got, "stopped until you answer") {
		t.Fatalf("a lane's card lost its wait line: %s", got)
	}
	asks.PRs = []cardPR{fullPR(7)}
	got := blocksJSON(t, asks.replyBlocks(view()))
	if strings.Contains(got, "stopped") || !strings.Contains(got, cardPRHint) {
		t.Fatalf("a PR card reads %s, want only what its buttons do", got)
	}
}

func TestNoFieldOfACardDrawsAHiddenComment(t *testing.T) {
	hidden := " <!-- cci #453550 -->"
	pr := cardPR{URL: "https://github.com/o/r/pull/7", Title: "t" + hidden, Review: "CI green" + hidden, Surprise: "It changes." + hidden, Plan: "Plan." + hidden, Refresher: "Low." + hidden, Blocks: "Nothing." + hidden}
	row := pr
	card := statusCard{
		Status: "needs_you", Title: "Land it?" + hidden, Body: "First sentence." + hidden + " Second sentence.",
		Options: []string{"Keep the long chapter shape as it is", "Drop it"}, Hints: map[string]string{"Drop it": "It goes." + hidden},
		Facts: []cardFact{{"Cause", "The move." + hidden}}, PRs: []cardPR{pr}, Waits: "card-x",
		Rows: []cardRow{{ID: "a:1", Text: "A row." + hidden, Buttons: []string{"Approve"}, PR: &row}},
	}
	group := statusCard{Status: "needs_you", Title: "Which?", Group: "g", Rows: card.Rows}
	for name, got := range map[string]string{"reply": blocksJSON(t, card.replyBlocks(view())), "root": blocksJSON(t, card.rootBlocks(view())), "text": card.text(), "rows": blocksJSON(t, group.replyBlocks(view()))} {
		if strings.Contains(got, "453550") || strings.Contains(got, "!--") {
			t.Errorf("%s draws the comment: %s", name, got)
		}
	}
	if got := blocksJSON(t, card.replyBlocks(view())); !strings.Contains(got, "First sentence. Second sentence.") {
		t.Errorf("the words around a comment run on as one sentence: %s", got)
	}
	labeled := card
	labeled.Options = []string{"Keep the long chapter shape as it is" + hidden, "Drop it"}
	if got := blocksJSON(t, labeled.replyBlocks(view())); !strings.Contains(got, "*1. Keep the long chapter shape as it is*") {
		t.Errorf("a pick's row draws its comment: %s", got)
	}
	if card.Body != "First sentence."+hidden+" Second sentence." || !strings.Contains(card.PRs[0].Refresher, "453550") {
		t.Error("drawing a card edits what it stores")
	}
}

func TestAPRCardDrawsItsELI5AndWhatChangesWhole(t *testing.T) {
	change, eli5 := strings.Repeat("It changes a thing. ", 25)+"The end of the change.", strings.Repeat("Chats come back on their own. ", 30)+"The last line."
	card := statusCard{Status: "needs_you", Title: "Land it?", Waits: "merge:7:abc", PRs: []cardPR{{URL: "https://github.com/o/r/pull/7", Surprise: change, Plan: "As planned.", Refresher: eli5, Blocks: "Only your approval."}}}
	got := blocksJSON(t, card.replyBlocks(view()))
	for _, whole := range []string{"The end of the change.", "The last line."} {
		if !strings.Contains(got, whole) {
			t.Errorf("the card cuts %q: %s", whole, got)
		}
	}
	if strings.Contains(got, "…") {
		t.Errorf("the card cuts a line: %s", got)
	}
}

func TestAPRWaitingOnTheChangeBelowItSaysSoUnderBlocksLanding(t *testing.T) {
	card := statusCard{Status: "needs_you", Title: "Pick one", Options: []string{"A", "B"}, Waits: "card-x", PRs: []cardPR{{URL: "https://github.com/o/r/pull/7", Surprise: "It changes.", Plan: "Plan: step 3.", Refresher: "Plainly.", Lands: "the change below it, #6, lands"}}}
	got := blocksJSON(t, card.replyBlocks(view()))
	if !strings.Contains(got, "*Waits on*") || !strings.Contains(got, "It lands once the change below it, #6, lands.") || strings.Contains(got, "*Lands once*") {
		t.Fatalf("want the wait under Waits on: %s", got)
	}
}

func TestALongPickLabelDrawsWholeOnARowWithAShortButton(t *testing.T) {
	label := strings.Repeat("x", cardMaxOptionLen)
	card := statusCard{Status: "needs_you", Title: "Which?", Options: []string{label, "No"}, Waits: "card-x"}
	if err := card.checkOptions(false); err != nil {
		t.Fatal(err)
	}
	blocks := card.replyBlocks(view())
	if err := checkBlocks(blocks); err != nil {
		t.Fatalf("Slack would refuse it: %v", err)
	}
	if !strings.Contains(blocksJSON(t, blocks), "*1. "+label+"*") {
		t.Fatalf("the label is not whole on its row: %s", blocksJSON(t, blocks))
	}
	for _, b := range cardButtons(t, blocks) {
		if runes(b.Text.Text) > cardMaxLabelLen {
			t.Fatalf("button %q runs past Slack's limit", b.Text.Text)
		}
		if len(b.Value) > 2000 {
			t.Fatalf("button value of %d characters runs past Slack's limit", len(b.Value))
		}
	}
	card.Options[0] += "x"
	if err := card.checkOptions(false); err == nil {
		t.Fatal("a label past the limit passed")
	}
}

func TestAPRCardOpensEachPROnGitHubAndItsCopyKeepsTheButton(t *testing.T) {
	card := statusCard{Status: "needs_you", Title: "Approve #7?", Options: []string{"Approve", "Hold"}, Recommended: "Approve", Waits: "w", PRs: []cardPR{fullPR(7)}}
	opens := func(blocks []slack.Block) map[string]string {
		urls := map[string]string{}
		for _, b := range cardButtons(t, blocks) {
			if b.URL != "" {
				urls[b.URL] = b.Text.Text
			}
		}
		return urls
	}
	blocks := card.replyBlocks(view())
	if got := opens(blocks); !maps.Equal(got, map[string]string{card.PRs[0].URL: cardOpenLabel}) {
		t.Fatalf("a waiting PR card opens %v", got)
	}
	if _, answers := copiedCard(blocks, permalinkRef{channelID: "D1", ts: "1.2"}); answers != 3 {
		t.Fatalf("its copy answers %d, want Approve, Hold and Other", answers)
	}
	info := statusCard{Status: "info", Title: "Two PRs", PRs: []cardPR{fullPR(7), fullPR(8)}}
	want := map[string]string{fullPR(7).URL: cardOpenLabel, fullPR(8).URL: cardOpenLabel}
	if got := opens(info.replyBlocks(view())); !maps.Equal(got, want) {
		t.Fatalf("a card naming two PRs opens %v, want %v", got, want)
	}
}

func TestATableIsASortablePagedDataTableWithItsNumberColumnsNumeric(t *testing.T) {
	rows := make([][]string, 0, 13)
	rows = append(rows, []string{"PR", "age"})
	for i := range 12 {
		rows = append(rows, []string{fmt.Sprintf("#%d", i), fmt.Sprint(i * 3)})
	}
	table := tableBlock(rows)
	if table.Type != slack.MBTDataTable || table.PageSize != cardTablePage || table.Caption != "PR · age" {
		t.Fatalf("table = %+v, want a data_table paged at %d with its header as caption", table, cardTablePage)
	}
	if _, ok := table.Rows[0][1].(*slack.DataTableRawTextCell); !ok {
		t.Fatalf("header cell = %#v, want raw text", table.Rows[0][1])
	}
	if age, ok := table.Rows[3][1].(*slack.DataTableRawNumberCell); !ok || age.Value != 6 {
		t.Fatalf("age cell = %#v, want the number 6 so the column sorts numerically", table.Rows[3][1])
	}
	if _, ok := table.Rows[3][0].(*slack.DataTableRawTextCell); !ok {
		t.Fatalf("PR cell = %#v, want text", table.Rows[3][0])
	}
}

func TestAPRCardLeadsWithItsPortAndELI5AndShowsWhatHeWillHateMostAndShowMe(t *testing.T) {
	pr := fullPR(52857)
	pr.Port = "Port of [#51000](https://github.com/acme/app/pull/51000): the new tree keeps chats in its own database"
	pr.HateMost = "A reconnect storm still pages once."
	pr.ShowMe = "Before: a page per reconnect. After: a page when errors keep coming for five minutes."
	card := statusCard{Status: "needs_you", Title: "Port: approve #52857?", Options: []string{"Approve", "Hold"}, Recommended: "Approve", Waits: "merge-52857", PRs: []cardPR{pr}}
	blocks := card.replyBlocks(view())
	if err := checkBlocks(blocks); err != nil {
		t.Fatalf("Slack would refuse the card: %v", err)
	}
	said := asSection(t, blocks[3]).Text.Text
	if !strings.HasPrefix(said, "Port of <https://github.com/acme/app/pull/51000|#51000>") {
		t.Fatalf("fourth block %q, want the port line first under the PR's title", said)
	}
	if !strings.Contains(said, "\n\n*Refresher*\nA stale credential on disk") || strings.Contains(said, "*Show me*") {
		t.Fatalf("fourth block %q, want the refresher under the port line and the Show me in a block of its own", said)
	}
	if shown := asSection(t, blocks[4]).Text.Text; shown != "*Show me*\nBefore: a page per reconnect. After: a page when errors keep coming for five minutes." {
		t.Fatalf("fifth block %q, want the Show me under the PR's lines", shown)
	}
	got := blocksJSON(t, blocks)
	for _, want := range []string{"*What you'll hate most*\\nA reconnect storm still pages once.", "*Show me*\\nBefore: a page per reconnect."} {
		if !strings.Contains(got, want) {
			t.Fatalf("blocks %s lack %q", got, want)
		}
	}
	if strings.Contains(got, "*Risk*") || strings.Contains(got, "*Against the plan*") || strings.Contains(sectionText(blocks)+pr.headline(0, 1), "7bf3ea0") {
		t.Fatalf("blocks %s, want no old labels and no commit", got)
	}
}

const fencedTable = "*Show me*\n```\nThe 5 tables past the line (measured)\n" +
	"table  team class                  table GB  its database GB  what it did in the last day            parts at 1.7 GB\n" +
	"-----  --------------------------  --------  ---------------  -------------------------------------  ---------------\n" +
	"1      Forge, shared cluster       9.25      21.01            +4.03 GB in 5.6 hours (inserts)         6\n" +
	"2      second team, shared cluster 8.94      13.55            database 5.02 to 13.55 GB in a day      6\n" +
	"3      staging test team           7.22      7.40             no change since Nov 7                   5\n" +
	"\nby creation time: a new empty part every 2.4 hours\nby id hash:       every part takes the inserts\n```"

func TestATableLaidOutInACodeBlockDrawsAsATable(t *testing.T) {
	blocks := bodyPages("Five tables are past 1.7 GB.\n\n"+fencedTable, cardMaxBlocks, cardMaxVisible, true, nil)[0]
	table := tables(blocks)
	if len(table) != 1 || len(table[0].Rows) != 4 || len(table[0].Rows[2]) != 6 || !strings.HasPrefix(table[0].Caption, "table · team class · table GB") {
		t.Fatalf("blocks %s, want the four aligned lines as one table of six columns, its rule line gone", blocksJSON(t, blocks))
	}
	if cell, ok := table[0].Rows[2][1].(*slack.DataTableRawTextCell); !ok || cell.Text != "second team, shared cluster" {
		t.Fatalf("row 2 cell = %#v, want the whole team class though one space parts it from the next column", table[0].Rows[2][1])
	}
	want := []string{"Five tables are past 1.7 GB.", "*Show me*", "```\nThe 5 tables past the line (measured)\n```", "```\nby creation time: a new empty part every 2.4 hours\nby id hash:       every part takes the inserts\n```"}
	if got := sectionTexts(blocks); !slices.Equal(got, want) {
		t.Fatalf("sections %q, want %q: the lines around the table stay code", got, want)
	}
}

func TestIndentedAlignedCodeStaysACodeBlock(t *testing.T) {
	code := "```\nCREATE TABLE parts (\n  table_id    bigint       NOT NULL,\n  created_at  timestamptz  NOT NULL,\n  size_bytes  bigint       NOT NULL\n);\n```"
	blocks := bodyPages(code, cardMaxBlocks, cardMaxVisible, true, nil)[0]
	if got := sectionTexts(blocks); len(tables(blocks)) != 0 || !slices.Equal(got, []string{code}) {
		t.Fatalf("blocks %s, want the SQL as the one code block it was", blocksJSON(t, blocks))
	}
}

func TestAPRCardDrawsEachTableInItsShowMeAsADataTableAndItsNotificationLeavesShowMeOut(t *testing.T) {
	pr := fullPR(52919)
	pr.ShowMe = strings.TrimPrefix(fencedTable, "*Show me*\n") + "\n\n| run | took |\n|---|---|\n| before | 9 min |\n| after | 2 min |"
	blocks := pr.section(view(), 0, 1, nil)
	if err := checkBlocks(blocks); err != nil {
		t.Fatalf("Slack would refuse the PR card: %v", err)
	}
	drawn := tables(blocks)
	if len(drawn) != 2 || len(drawn[0].Rows) != 4 || len(drawn[0].Rows[0]) != 6 || len(drawn[1].Rows) != 3 {
		t.Fatalf("blocks %s, want the Show me's laid-out table and its pipe table each as a data table", blocksJSON(t, blocks))
	}
	if cell, ok := drawn[0].Rows[1][1].(*slack.DataTableRawTextCell); !ok || cell.Text != "Forge, shared cluster" {
		t.Fatalf("row 1 cell = %#v, want the team class as written", drawn[0].Rows[1][1])
	}
	shown := sectionText(blocks)
	if strings.Contains(shown, "• *table:*") || !strings.Contains(shown, "*Show me*") || !strings.Contains(shown, "```\nThe 5 tables past the line (measured)\n```") {
		t.Fatalf("sections %q, want the Show me heading and its code kept and no table drawn as lines", shown)
	}
	card := statusCard{Status: "needs_you", Title: "Split big tables?", Body: "Five tables are past 1.7 GB.\n" + fencedTable}
	if got := card.text(); got != "🙋 Split big tables? — Five tables are past 1.7 GB." {
		t.Fatalf("notification text = %q, want the title and summary without the Show me", got)
	}
}

const (
	barChart  = "```chart\n" + `{"$schema": "https://vega.github.io/schema/vega-lite/v5.json", "data": {"values": [{"what": "Slack posts", "count": 384}, {"what": "distinct alerts", "count": 41}]}, "mark": "bar", "encoding": {"y": {"field": "what", "type": "nominal", "title": null, "sort": null}, "x": {"field": "count", "type": "quantitative", "title": "in 24 hours"}}}` + "\n```"
	lineChart = "```chart\n" + `{"title": "Merge time by week", "data": {"values": [{"week": "W1", "lane": "fast", "min": 9}, {"week": "W2", "lane": "fast", "min": 7}, {"week": "W1", "lane": "slow", "min": 30}, {"week": "W2", "lane": "slow", "min": 22}]}, "mark": {"type": "line", "point": true}, "encoding": {"x": {"field": "week", "type": "ordinal"}, "y": {"field": "min", "type": "quantitative"}, "color": {"field": "lane", "type": "nominal"}}}` + "\n```"
	pieChart  = "```chart\n" + `{"data": {"values": [{"kind": "alerts", "n": 41}, {"kind": "releases", "n": 12}]}, "mark": "arc", "encoding": {"theta": {"field": "n", "type": "quantitative"}, "color": {"field": "kind", "type": "nominal"}}}` + "\n```"
)

func charts(blocks []slack.Block) []*slack.DataVisualizationBlock {
	var found []*slack.DataVisualizationBlock
	for _, b := range blocks {
		if chart, ok := b.(*slack.DataVisualizationBlock); ok {
			found = append(found, chart)
		}
	}
	return found
}

func carousels(blocks []slack.Block) []*slack.CarouselBlock {
	var found []*slack.CarouselBlock
	for _, b := range blocks {
		if carousel, ok := b.(*slack.CarouselBlock); ok {
			found = append(found, carousel)
		}
	}
	return found
}

func TestAChartFenceDrawsAsSlacksOwnChart(t *testing.T) {
	blocks := bodyPages("Posts against alerts.\n"+barChart+"\n\nMerge time.\n\n"+lineChart, cardMaxBlocks, cardMaxVisible, true, nil)[0]
	if err := checkBlocks(blocks); err != nil {
		t.Fatalf("Slack would refuse the body: %v", err)
	}
	pie := charts(bodyPages(pieChart, cardMaxBlocks, cardMaxVisible, true, nil)[0])
	drawn := append(charts(blocks), pie...)
	if len(blocks) != 4 || len(drawn) != 3 || !slices.Equal(sectionTexts(blocks), []string{"Posts against alerts.", "Merge time."}) {
		t.Fatalf("blocks %s, want each chart fence as a chart under the line above it", blocksJSON(t, blocks))
	}
	want := []slack.Block{
		slack.NewDataVisualizationBlock("count by what", slack.NewDataVisualizationBarChart(
			slack.NewDataVisualizationAxisConfig("Slack posts", "distinct alerts").WithYLabel("in 24 hours"),
			slack.NewDataVisualizationDataSeries("in 24 hours", slack.NewDataVisualizationDataPoint("Slack posts", 384), slack.NewDataVisualizationDataPoint("distinct alerts", 41)))),
		slack.NewDataVisualizationBlock("Merge time by week", slack.NewDataVisualizationLineChart(
			slack.NewDataVisualizationAxisConfig("W1", "W2").WithXLabel("week").WithYLabel("min"),
			slack.NewDataVisualizationDataSeries("fast", slack.NewDataVisualizationDataPoint("W1", 9), slack.NewDataVisualizationDataPoint("W2", 7)),
			slack.NewDataVisualizationDataSeries("slow", slack.NewDataVisualizationDataPoint("W1", 30), slack.NewDataVisualizationDataPoint("W2", 22)))),
		slack.NewDataVisualizationBlock("n by kind", slack.NewDataVisualizationPieChart(
			slack.NewDataVisualizationSegment("alerts", 41), slack.NewDataVisualizationSegment("releases", 12))),
	}
	got := make([]slack.Block, 0, len(drawn))
	for _, chart := range drawn {
		got = append(got, chart)
	}
	if blocksJSON(t, got) != blocksJSON(t, want) {
		t.Fatalf("charts %s, want %s: the bar's categories on the axis it names, the line's series by color, the arc as a pie", blocksJSON(t, got), blocksJSON(t, want))
	}
}

func TestAChartSlackCannotHoldIsRefusedAndAFenceThatIsNoChartStaysCode(t *testing.T) {
	long := strings.NewReplacer(`"Slack posts"`, `"Slack posts, each a tag today"`).Replace(barChart)
	point := strings.Replace(barChart, `"mark": "bar"`, `"mark": "point"`, 1)
	pr := fullPR(52919)
	pr.ShowMe = "```chart\nnot a spec\n```"
	for name, c := range map[string]struct {
		card statusCard
		want string
	}{
		"a label past 20":     {statusCard{Status: "info", Title: "Alerts", Body: long}, `label "Slack posts, each a tag today" is 29 characters, not 1 to 20`},
		"a mark Slack lacks":  {statusCard{Status: "info", Title: "Alerts", Body: point}, `its mark "point" is none of bar, line, area or arc`},
		"a PR's Show me spec": {statusCard{Status: "info", Title: "Alerts", PRs: []cardPR{pr}}, "its spec is no JSON object"},
	} {
		if err := c.card.check(bodyAt, false, ""); err == nil || !strings.Contains(err.Error(), "draws no Slack chart, since "+c.want) {
			t.Errorf("%s: check = %v, want it refused naming %q", name, err, c.want)
		}
	}
	if got := sectionTexts(bodyPages(point, cardMaxBlocks, cardMaxVisible, true, nil)[0]); !slices.Equal(got, []string{point}) {
		t.Fatalf("sections %q, want the fence that is no chart kept as the code it was", got)
	}
	three := slices.Concat(bodyPages(barChart+"\n\n"+lineChart+"\n\n"+pieChart, cardMaxBlocks, cardMaxVisible, true, nil)...)
	if err := checkBlocks(three); err == nil || !strings.Contains(err.Error(), "at most 2 charts") {
		t.Fatalf("checkBlocks = %v, want a third chart in one message refused", err)
	}
	wide := charts(bodyPages(long, cardMaxBlocks, cardMaxVisible, true, nil)[0])
	if err := checkBlocks([]slack.Block{wide[0]}); err == nil || !strings.Contains(err.Error(), "is 29 characters, not 1 to 20") {
		t.Fatalf("checkBlocks = %v, want a label past Slack's 20 characters refused", err)
	}
}

func TestPicturesInARowDrawAsOneGalleryEachTitledWithItsCaption(t *testing.T) {
	pictures := map[string]*slack.ImageBlock{}
	var run []string
	for i := range 14 {
		source, caption := fmt.Sprintf("/shots/%d.png", i+1), fmt.Sprintf("%d · option %d", i+1, i+1)
		pictures[source] = slack.NewImageBlock("https://bucket.example.com/"+strconv.Itoa(i+1)+".png", caption, "", slack.NewTextBlockObject(slack.PlainTextType, caption, false, false))
		if i >= 3 {
			run = append(run, "!["+caption+"]("+source+")")
		}
	}
	body := "![1 · option 1](/shots/1.png)\n\n![2 · option 2](/shots/2.png)\n\nBetween.\n\n![3 · option 3](/shots/3.png)\n\nAfter.\n\n" + strings.Join(run, "\n")
	blocks := bodyPages(body, cardMaxBlocks, cardMaxVisible, true, pictures)[0]
	if err := checkBlocks(blocks); err != nil {
		t.Fatalf("Slack would refuse the body: %v", err)
	}
	drawn := carousels(blocks)
	if len(blocks) != 6 || len(drawn) != 2 || blocks[0] != slack.Block(drawn[0]) || blocks[2] != slack.Block(pictures["/shots/3.png"]) || blocks[4] != slack.Block(drawn[1]) || blocks[5] != slack.Block(pictures["/shots/14.png"]) {
		t.Fatalf("blocks %s, want the first two pictures as one gallery, the lone one as its image, and the run of eleven as a gallery of ten and one image", blocksJSON(t, blocks))
	}
	if len(drawn[0].Elements) != 2 || len(drawn[1].Elements) != 10 {
		t.Fatalf("galleries of %d and %d cards, want 2 and 10", len(drawn[0].Elements), len(drawn[1].Elements))
	}
	for i, card := range drawn[0].Elements {
		caption := fmt.Sprintf("%d · option %d", i+1, i+1)
		if card.Title == nil || card.Title.Text != caption || card.HeroImage == nil || *card.HeroImage.ImageURL != fmt.Sprintf("https://bucket.example.com/%d.png", i+1) {
			t.Fatalf("card %d = %s, want picture %d titled %q", i, blocksJSON(t, []slack.Block{card}), i+1, caption)
		}
	}
}

func TestACardsOwnPicturesAndAPRsPicturesEachDrawAsAGallery(t *testing.T) {
	pr := fullPR(52857)
	pr.Pictures = []string{"/shots/before.png", "/shots/after.png"}
	card := statusCard{Status: "needs_you", Title: "Two layouts", PRs: []cardPR{pr}, Images: []cardImage{
		{ID: "F1", Title: "before.png"}, {ID: "F2", Title: "after.png"}, {ID: "F3", Title: "board.png"}, {ID: "F4", Title: "queue.png"},
	}}
	blocks, _ := card.layout(view())
	if err := checkBlocks(blocks); err != nil {
		t.Fatalf("Slack would refuse the card: %v", err)
	}
	drawn := carousels(blocks)
	if len(drawn) != 2 || slices.ContainsFunc(blocks, func(b slack.Block) bool { return b.BlockType() == slack.MBTImage }) {
		t.Fatalf("blocks %s, want the PR's two pictures and the card's other two as two galleries and no lone image", blocksJSON(t, blocks))
	}
	for i, want := range [][]string{{"F1", "F2"}, {"F3", "F4"}} {
		var files []string
		for _, c := range drawn[i].Elements {
			files = append(files, c.HeroImage.SlackFile.ID)
		}
		if !slices.Equal(files, want) {
			t.Fatalf("gallery %d shows %v, want %v", i, files, want)
		}
	}
}
