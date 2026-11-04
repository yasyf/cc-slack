package main

import (
	"fmt"
	"slices"
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

func tables(blocks []slack.Block) []*slack.TableBlock {
	var found []*slack.TableBlock
	for _, b := range blocks {
		if table, ok := b.(*slack.TableBlock); ok {
			found = append(found, table)
		}
	}
	return found
}

func fullPR(n int) cardPR {
	return cardPR{
		URL: fmt.Sprintf("https://github.com/acme/app/pull/%d", n), Title: "One credential per team", Head: "7bf3ea0123",
		Change: "Launches stop waiting on a new credential", Plan: "Nothing differed", Risk: "A stale credential on disk", Blocks: "Your approval",
	}
}

func TestABodyRendersItsMarkdownAsBlocksNeverAsRawMarks(t *testing.T) {
	body := "## Storage\n**Fixed** in [the PR](https://github.com/acme/app/pull/2).\n- one\n- two\n\n" +
		"| check | state |\n|---|---|\n| **lint** | [green](https://ci.example.com/1) |\n| tests | |\n\nDone."
	pages := bodyPages(body, cardMaxBlocks, cardMaxVisible, true)
	if len(pages) != 1 {
		t.Fatalf("pages = %d, want the short body on one", len(pages))
	}
	got := strings.NewReplacer(`\u003c`, "<", `\u003e`, ">").Replace(blocksJSON(t, pages[0]))
	for _, raw := range []string{"**", "](http", "## ", "|---|", "| check"} {
		if strings.Contains(got, raw) {
			t.Fatalf("blocks %s carry the raw mark %q", got, raw)
		}
	}
	for _, want := range []string{"*Storage*", "*Fixed* in <https://github.com/acme/app/pull/2|the PR>", "• one", `"type":"table"`, `"url":"https://ci.example.com/1"`, `"bold":true`, cardEmptyCell} {
		if !strings.Contains(got, want) {
			t.Fatalf("blocks %s lack %q", got, want)
		}
	}
	table := tables(pages[0])
	if len(table) != 1 || len(table[0].Rows) != 3 || len(table[0].Rows[0]) != 2 {
		t.Fatalf("tables = %+v, want one of three rows and two columns with the rule row gone", table)
	}
}

func TestAMessageCarriesOneTableAndAnyOtherAsLines(t *testing.T) {
	body := "| a | b |\n|---|---|\n| 1 | 2 |\n\nBetween.\n\n| c | d |\n|---|---|\n| 3 | 4 |"
	blocks := bodyPages(body, cardMaxBlocks, cardMaxVisible, true)[0]
	if len(tables(blocks)) != 1 {
		t.Fatalf("blocks %s, want one table block, the most Slack takes on a message", blocksJSON(t, blocks))
	}
	if got := sectionTexts(blocks); !slices.Contains(got, "• *c:* 3 · *d:* 4") {
		t.Fatalf("sections %q, want the second table's row as a line of its own", got)
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
	pages := bodyPages(body, 40, cardMaxVisible, true)
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
	for _, text := range sectionTexts(bodyPages(body, cardMaxBlocks, cardMaxVisible, true)[0]) {
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

func TestAPRCardShowsItsLinkCIFourLinesAndDescription(t *testing.T) {
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
	link := asSection(t, blocks[1]).Text.Text
	for _, want := range []string{"<https://github.com/acme/app/pull/49622|app PR 49622>", "One credential per team", "`7bf3ea0`", "CI passed"} {
		if !strings.Contains(link, want) {
			t.Fatalf("link line %q lacks %q", link, want)
		}
	}
	fields := asSection(t, blocks[2]).Fields
	if len(fields) != 4 || !strings.HasPrefix(fields[0].Text, "*What changes*\nLaunches") || !strings.HasPrefix(fields[3].Text, "*Blocks landing*\nYour") {
		t.Fatalf("fields = %+v, want the four lines", fields)
	}
	if got := blocksJSON(t, blocks); !strings.Contains(got, "It keeps one storage credential per team.") {
		t.Fatalf("blocks %s lack the PR's description", got)
	}
	buttons := cardButtons(t, blocks)
	if buttons[0].Text.Text != "Approve (Recommended)" || buttons[1].Text.Text != "Hold" {
		t.Fatalf("buttons %q %q, want Approve and Hold", buttons[0].Text.Text, buttons[1].Text.Text)
	}
	if ids := card.refIDs(); len(ids) != 1 || ids[0] != id {
		t.Fatalf("refs = %+v, want the PR kept live so its CI state redraws", ids)
	}
}

func TestAPRCardMissingALinePostsWithTheLinesItHas(t *testing.T) {
	pr := fullPR(49722)
	pr.Risk, pr.Blocks = "", " "
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
		if !strings.Contains(got, "*What changes*") || !strings.Contains(got, "*Against the plan*") || strings.Contains(got, "*Risk*") || strings.Contains(got, "*Blocks landing*") {
			t.Fatalf("%s: blocks %s, want the two lines it has and no empty field", name, got)
		}
	}
	bare := cardPR{URL: pr.URL}
	if err := checkBlocks(bare.blocks(view(), cardQuietBody)); err != nil {
		t.Fatalf("a PR with no lines at all draws a block Slack refuses: %v", err)
	}
}

func TestAnOverLongLineOrHintPostsWholeInSections(t *testing.T) {
	pr := fullPR(7)
	pr.Risk = strings.Repeat("A risk worth a long explanation. ", 100)
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
	if strings.Count(got, "A risk worth") != 100 || strings.Count(got, "A reason to drop") != 200 {
		t.Fatalf("a line or hint was cut: %d of 100, %d of 200", strings.Count(got, "A risk worth"), strings.Count(got, "A reason to drop"))
	}
}

func TestAPRCardWithNoGitHubToReadStillPosts(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	l.do(opDMStatus, map[string]any{"card": statusCard{Status: "info", Title: "The relay fix", PRs: []cardPR{fullPR(7)}}})
	posts := l.slack.sent()
	if len(posts) != 1 || !strings.Contains(posts[0].blocks, "What changes") {
		t.Fatalf("posts = %+v, want the card out with its lines", posts)
	}
}

func TestAPRDescriptionDropsCommentsAndFolds(t *testing.T) {
	got := prDescription("<!-- bot -->Summary\n<details><summary>Changes</summary>\nA file\n</details>\n")
	if got != "Summary\n**Changes**\nA file" {
		t.Fatalf("description = %q", got)
	}
}

func TestALongPRDescriptionFoldsTheRest(t *testing.T) {
	pr := fullPR(7)
	pr.Description = strings.Repeat("A sentence of the description. ", 200)
	card := statusCard{Status: "needs_you", Title: "Approve?", Options: []string{"Approve", "Hold"}, Recommended: "Approve", Waits: "m7", PRs: []cardPR{pr}}
	blocks, fold := card.layout(view())
	if len(fold) != 1 || !strings.Contains(blocksJSON(t, fold[0]), "the rest of its description") {
		t.Fatalf("fold = %s, want the rest behind Explain more", blocksJSON(t, slices.Concat(fold...)))
	}
	if strings.Count(blocksJSON(t, blocks), "description.")+strings.Count(blocksJSON(t, fold[0]), "description.") != 200 {
		t.Fatal("a sentence of the description was lost")
	}
	if got := blocksJSON(t, blocks); !strings.Contains(got, cardExplainAction+"m7") || strings.Contains(got, cardFoldAction) {
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
		t.Fatalf("%d blocks and %d folds, want the card under Slack's %d with one fold per long description", len(blocks), len(fold), cardMaxBlocks)
	}
	over := statusCard{Status: "needs_you", Title: "Login", Group: "pg:login", Rows: append(slices.Clone(rows), cardRow{ID: "pg:9:abc", PR: rows[0].PR, Buttons: []string{"Hold"}})}
	if err := over.check(bodyAt, false, ""); err == nil {
		t.Fatal("a group card past the PR-row limit passed its check")
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
	stripped := tappedBlocks(blocks, cardActionsBlock, "✓ picked")
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
		"with waits":   {Status: "done", Title: "x", Replaces: posted, Waits: "w", Options: []string{"go"}, Recommended: "go"},
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
		"two tables":    {[]slack.Block{tableBlock([][]string{{"a"}}), tableBlock([][]string{{"b"}})}, "block 2 (table): a message holds one table"},
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

func TestAStatusCardShowsItsFirstSentencesAndFoldsTheWall(t *testing.T) {
	sentence := "The credential landed and launches stopped waiting on a new one. "
	card := statusCard{Status: "info", Title: "Storage credential", Body: strings.Repeat(sentence, 30)}
	blocks, fold := card.layout(view())
	shown := sectionTexts(blocks)
	if len(shown) != 1 || runes(shown[0]) > cardQuietBody || !strings.HasSuffix(shown[0], ".") {
		t.Fatalf("shown = %q, want one section under %d characters ending on a whole sentence", shown, cardQuietBody)
	}
	if len(fold) == 0 || !strings.Contains(blocksJSON(t, blocks), cardFoldAction) {
		t.Fatalf("fold = %d pages, want the rest behind Explain more", len(fold))
	}
	if got := strings.Count(blocksJSON(t, blocks), "landed") + strings.Count(blocksJSON(t, slices.Concat(fold...)), "landed"); got != 30 {
		t.Fatalf("%d sentences across the card and its fold, want all 30", got)
	}
	short := statusCard{Status: "info", Title: "Storage credential", Body: sentence}
	if _, fold := short.layout(view()); len(fold) != 0 {
		t.Fatalf("a one-sentence body folded %d pages", len(fold))
	}
	table := statusCard{Status: "info", Title: "Checks", Body: "| check | state |\n|---|---|\n| " + strings.Repeat("a long cell ", 50) + " | green |"}
	if got := tables(table.replyBlocks(view())); len(got) != 1 {
		t.Fatal("a table folded away: a table is what he reads at a glance")
	}
}

func TestAPickCardKeepsMoreOfItsBodyInView(t *testing.T) {
	card := statusCard{Status: "needs_you", Title: "Which relay?", Options: []string{"Land", "Hold"}, Recommended: "Land", Waits: "w", Body: strings.Repeat("One more reason to pick. ", 40)}
	shown := sectionTexts(card.replyBlocks(view()))
	if len(shown) == 0 || runes(shown[0]) <= cardQuietBody || runes(shown[0]) > cardWaitBody {
		t.Fatalf("shown = %d characters, want between %d and %d", runes(shown[0]), cardQuietBody, cardWaitBody)
	}
}
