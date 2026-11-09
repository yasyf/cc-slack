package main

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/slack-go/slack"
)

const (
	cardMaxBlocks    = 50
	cardMaxVisible   = 9000
	cardMaxPageBlock = 45
	cardMaxTableRows = 100
	cardMaxTableCols = 20
	cardEmptyCell    = "—"
	cardTablePage    = 10
	slackMaxCaption  = 150
	slackMaxHeader   = 150
	slackMaxSection  = 3000
	slackMaxField    = 2000
	slackMaxFields   = 10
	slackMaxActions  = 25
	slackMaxButton   = 75
	slackMaxValue    = 2000
	slackMaxID       = 255
)

var (
	mdBold          = regexp.MustCompile(`\*\*([^*\n]+)\*\*`)
	mdLink          = regexp.MustCompile(`\[([^\]\n]+)\]\((https?://[^)\s]+)\)`)
	mdHeading       = regexp.MustCompile(`^#{1,6} +(.+?)\s*#*$`)
	mdBullet        = regexp.MustCompile(`(?m)^([ \t]*)[-*] +`)
	mdEndsOnHeading = regexp.MustCompile(`(?:^|\n)(?:#{1,6} +[^\n]*|\*{1,2}[^*\n]+\*{1,2})\s*$`)
	mdRule          = regexp.MustCompile(`^\|?[\s:|-]*-[\s:|-]*\|?$`)
	mdMarks         = regexp.MustCompile(`(?m)\*\*[^*\n]+\*\*|\]\(https?://|^#{1,6} |^\s*\|`)
	cellToken       = regexp.MustCompile("<(https?://[^|>\\s]+)\\|([^>]+)>|\\[([^\\]\\n]+)\\]\\((https?://[^)\\s]+)\\)|\\*\\*?([^*\\n]+)\\*\\*?|`([^`\\n]+)`")
	htmlComment     = regexp.MustCompile(`(?s)[ \t]*<!--.*?-->`)
	htmlSummary     = regexp.MustCompile(`(?is)<summary>(.*?)</summary>`)
	htmlFold        = regexp.MustCompile(`(?i)</?details[^>]*>|<br\s*/?>`)
)

type bodyPiece struct {
	text  string
	table [][]string
}

func bodyPieces(body string) []bodyPiece {
	var pieces []bodyPiece
	var para []string
	fence := make([]string, 0, 1)
	var table [][]string
	flushPara := func() {
		if len(para) > 0 {
			pieces = append(pieces, bodyPiece{text: strings.Join(para, "\n")})
			para = nil
		}
	}
	flushTable := func() {
		if len(table) > 0 {
			pieces = append(pieces, bodyPiece{table: table})
			table = nil
		}
	}
	inFence := false
	for line := range strings.SplitSeq(strings.TrimSpace(body), "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "```") && inFence:
			pieces = append(pieces, bodyPiece{text: strings.Join(append(fence, line), "\n")})
			fence, inFence = fence[:0], false
		case inFence:
			fence = append(fence, line)
		case strings.HasPrefix(trimmed, "```"):
			flushPara()
			flushTable()
			fence, inFence = append(fence[:0], line), true
		case strings.HasPrefix(trimmed, "|"):
			flushPara()
			if !mdRule.MatchString(trimmed) {
				table = append(table, tableCells(trimmed))
			}
		case trimmed == "":
			flushPara()
			flushTable()
		case mdHeading.MatchString(trimmed):
			flushPara()
			flushTable()
			para = append(para, "*"+strings.Trim(mrkdwnLine(mdHeading.FindStringSubmatch(trimmed)[1]), "*")+"*")
		default:
			flushTable()
			para = append(para, mrkdwnLine(line))
		}
	}
	flushPara()
	flushTable()
	if inFence {
		pieces = append(pieces, bodyPiece{text: strings.Join(append(fence, "```"), "\n")})
	}
	return pieces
}

func mrkdwnLine(line string) string {
	line = mdLink.ReplaceAllString(line, "<$2|$1>")
	line = mdBold.ReplaceAllString(line, "*$1*")
	return mdBullet.ReplaceAllString(line, "$1• ")
}

func tableCells(line string) []string {
	var cells []string
	var cell strings.Builder
	inLink := false
	for _, r := range strings.TrimSuffix(strings.TrimPrefix(line, "|"), "|") {
		switch {
		case r == '<':
			inLink = true
		case r == '>':
			inLink = false
		case r == '|' && !inLink:
			cells = append(cells, strings.TrimSpace(cell.String()))
			cell.Reset()
			continue
		}
		cell.WriteRune(r)
	}
	return append(cells, strings.TrimSpace(cell.String()))
}

func tableBlock(rows [][]string) *slack.DataTableBlock {
	columns := 0
	for _, row := range rows {
		columns = max(columns, len(row))
	}
	numbers := make([]bool, columns)
	for i := range numbers {
		numbers[i] = len(rows) > 1 && !slices.ContainsFunc(rows[1:], func(row []string) bool {
			_, err := strconv.ParseFloat(cellAt(row, i), 64)
			return err != nil
		})
	}
	table := slack.NewDataTableBlock(clipRunes(strings.Join(rows[0], " · "), slackMaxCaption)).WithPageSize(min(max(len(rows)-1, 1), cardTablePage))
	for at, row := range rows {
		cells := make([]slack.DataTableCell, columns)
		for i := range cells {
			text := cellAt(row, i)
			switch {
			case text == "":
				cells[i] = slack.NewDataTableRawTextCell(cardEmptyCell)
			case at == 0:
				cells[i] = slack.NewDataTableRawTextCell(text)
			case numbers[i]:
				value, _ := strconv.ParseFloat(text, 64)
				cells[i] = slack.NewDataTableRawNumberCell(value).WithText(text)
			default:
				cells[i] = tableCell(text)
			}
		}
		table = table.AddRow(cells...)
	}
	return table
}

func cellAt(row []string, i int) string {
	if i < len(row) {
		return row[i]
	}
	return ""
}

func tableCell(text string) slack.DataTableCell {
	found := cellToken.FindAllStringSubmatchIndex(text, -1)
	if found == nil {
		return slack.NewDataTableRawTextCell(text)
	}
	var elements []slack.RichTextSectionElement
	plain := func(s string) {
		if s != "" {
			elements = append(elements, slack.NewRichTextSectionTextElement(s, nil))
		}
	}
	part := func(m []int, group int) string {
		if m[group*2] < 0 {
			return ""
		}
		return text[m[group*2]:m[group*2+1]]
	}
	at := 0
	for _, m := range found {
		plain(text[at:m[0]])
		at = m[1]
		switch {
		case part(m, 1) != "":
			elements = append(elements, slack.NewRichTextSectionLinkElement(part(m, 1), part(m, 2), nil))
		case part(m, 4) != "":
			elements = append(elements, slack.NewRichTextSectionLinkElement(part(m, 4), part(m, 3), nil))
		case part(m, 5) != "":
			elements = append(elements, slack.NewRichTextSectionTextElement(part(m, 5), &slack.RichTextSectionTextStyle{Bold: true}))
		default:
			elements = append(elements, slack.NewRichTextSectionTextElement(part(m, 6), &slack.RichTextSectionTextStyle{Code: true}))
		}
	}
	plain(text[at:])
	return slack.NewDataTableRichTextCell(slack.NewRichTextSection(elements...))
}

func tableLines(rows [][]string) string {
	head := rows[0]
	lines := make([]string, 0, len(rows)-1)
	for _, row := range rows[1:] {
		parts := make([]string, 0, len(row))
		for i, cell := range row {
			if i < len(head) && head[i] != "" {
				cell = "*" + head[i] + ":* " + cell
			}
			parts = append(parts, mrkdwnLine(cell))
		}
		lines = append(lines, "• "+strings.Join(parts, " · "))
	}
	return strings.Join(lines, "\n")
}

func splitSection(text string, limit int) []string {
	if inner, ok := strings.CutPrefix(text, "```"); ok && runes(text) > limit {
		parts := splitSection(strings.TrimSuffix(strings.TrimSpace(inner), "```"), limit-len("```\n\n```"))
		for i, part := range parts {
			parts[i] = "```\n" + strings.TrimSpace(part) + "\n```"
		}
		return parts
	}
	var out []string
	for runes(text) > limit {
		cut := sectionCut([]rune(text), limit)
		out = append(out, strings.TrimRight(cut, " \n"))
		text = strings.TrimLeft(strings.TrimPrefix(text, cut), " \n")
	}
	return append(out, text)
}

func sectionCut(text []rune, limit int) string {
	head := string(text[:limit])
	for _, sep := range []struct {
		text string
		keep int
	}{{"\n\n", 0}, {"\n", 0}, {". ", 1}, {" ", 0}} {
		for at := strings.LastIndex(head, sep.text); at > 0; at = strings.LastIndex(head[:at], sep.text) {
			if strings.LastIndex(head[:at], "<") <= strings.LastIndex(head[:at], ">") && !mdEndsOnHeading.MatchString(head[:at+sep.keep]) {
				return head[:at+sep.keep]
			}
		}
	}
	return head
}

type bodyPage struct {
	blocks []slack.Block
	chars  int
	table  bool
}

func bodyPages(body string, firstBlocks, firstChars int, firstTable bool) [][]slack.Block {
	page := &bodyPage{table: !firstTable}
	var pages [][]slack.Block
	maxBlocks, maxChars := firstBlocks, firstChars
	turn := func() {
		if len(pages) == 0 || len(page.blocks) > 0 {
			pages = append(pages, page.blocks)
		}
		page = &bodyPage{}
		maxBlocks, maxChars = cardMaxPageBlock, cardMaxVisible
	}
	place := func(part string) {
		page.blocks = append(page.blocks, slack.NewSectionBlock(slack.NewTextBlockObject(slack.MarkdownType, part, false, false), nil, nil))
		page.chars += runes(part)
	}
	add := func(text string) {
		for _, part := range splitSection(text, cardMaxSectionLen) {
			room := maxChars - page.chars
			if len(page.blocks) < maxBlocks && len(pages) == 0 && room > 0 && runes(part) > room && !strings.HasPrefix(part, "```") {
				head := sectionCut([]rune(part), room)
				place(strings.TrimRight(head, " \n"))
				part = strings.TrimLeft(strings.TrimPrefix(part, head), " \n")
			}
			if len(page.blocks) >= maxBlocks || page.chars+runes(part) > maxChars {
				turn()
			}
			place(part)
		}
	}
	for _, piece := range bodyPieces(body) {
		switch {
		case piece.table == nil:
			add(piece.text)
		case len(piece.table) <= cardMaxTableRows && !tableTooWide(piece.table) && !page.table:
			size := tableChars(piece.table)
			if len(page.blocks) >= maxBlocks || page.chars+size > max(maxChars, cardMaxVisible) {
				turn()
			}
			page.blocks = append(page.blocks, tableBlock(piece.table))
			page.chars += size
			page.table = true
		case len(piece.table) > 1:
			add(tableLines(piece.table))
		default:
			add(strings.Join(piece.table[0], " · "))
		}
	}
	return append(pages, page.blocks)
}

func tableChars(rows [][]string) int {
	total := 0
	for _, row := range rows {
		for _, cell := range row {
			total += runes(cell)
		}
	}
	return total
}

func tableTooWide(rows [][]string) bool {
	return slices.ContainsFunc(rows, func(row []string) bool { return len(row) > cardMaxTableCols })
}

func prDescription(body string) string {
	body = htmlComment.ReplaceAllString(body, "")
	body = htmlSummary.ReplaceAllString(body, "**$1**")
	return strings.TrimSpace(htmlFold.ReplaceAllString(body, ""))
}

type blockLimitError struct {
	index   int
	kind    string
	problem string
}

func (e blockLimitError) Error() string {
	return fmt.Sprintf("Slack would refuse block %d (%s): %s", e.index+1, e.kind, e.problem)
}

func checkBlocks(blocks []slack.Block) error {
	if len(blocks) > cardMaxBlocks {
		return blockLimitError{cardMaxBlocks, "message", fmt.Sprintf("the message holds %d blocks, past Slack's %d", len(blocks), cardMaxBlocks)}
	}
	ids := map[string]bool{}
	tables := 0
	for i, b := range blocks {
		refuse := func(format string, args ...any) error {
			return blockLimitError{i, string(b.BlockType()), fmt.Sprintf(format, args...)}
		}
		if id := b.ID(); id != "" {
			if runes(id) > slackMaxID || ids[id] {
				return refuse("block_id %q repeats or runs past %d characters", id, slackMaxID)
			}
			ids[id] = true
		}
		if _, table := b.(*slack.DataTableBlock); table {
			tables++
		}
		if tables > 1 {
			return refuse("a message holds one table")
		}
		if err := checkBlock(b, refuse); err != nil {
			return err
		}
	}
	return nil
}

func checkBlock(b slack.Block, refuse func(string, ...any) error) error {
	switch block := b.(type) {
	case *slack.HeaderBlock:
		if n := runes(block.Text.Text); n == 0 || n > slackMaxHeader {
			return refuse("its text is %d characters, not 1 to %d", n, slackMaxHeader)
		}
	case *slack.SectionBlock:
		return checkSection(block, refuse)
	case *slack.ActionBlock:
		if n := len(block.Elements.ElementSet); n == 0 || n > slackMaxActions {
			return refuse("it holds %d elements, not 1 to %d", n, slackMaxActions)
		}
		for _, e := range block.Elements.ElementSet {
			if button, ok := e.(*slack.ButtonBlockElement); ok {
				if err := checkButton(button, refuse); err != nil {
					return err
				}
			}
		}
	case *slack.DataTableBlock:
		if len(block.Rows) > cardMaxTableRows || slices.ContainsFunc(block.Rows, func(row []slack.DataTableCell) bool { return len(row) > cardMaxTableCols }) {
			return refuse("it runs past %d rows or %d columns", cardMaxTableRows, cardMaxTableCols)
		}
	}
	return nil
}

func checkSection(s *slack.SectionBlock, refuse func(string, ...any) error) error {
	if s.Text == nil && len(s.Fields) == 0 {
		return refuse("it has neither text nor fields")
	}
	if s.Text != nil {
		if n := runes(s.Text.Text); n == 0 || n > slackMaxSection {
			return refuse("its text is %d characters, not 1 to %d", n, slackMaxSection)
		}
	}
	if len(s.Fields) > slackMaxFields {
		return refuse("it holds %d fields, past %d", len(s.Fields), slackMaxFields)
	}
	for _, f := range s.Fields {
		if n := runes(f.Text); n == 0 || n > slackMaxField {
			return refuse("a field is %d characters, not 1 to %d", n, slackMaxField)
		}
	}
	if s.Accessory != nil && s.Accessory.ButtonElement != nil {
		return checkButton(s.Accessory.ButtonElement, refuse)
	}
	return nil
}

func checkButton(b *slack.ButtonBlockElement, refuse func(string, ...any) error) error {
	switch {
	case runes(b.Text.Text) == 0 || runes(b.Text.Text) > slackMaxButton:
		return refuse("button %q is not 1 to %d characters", b.Text.Text, slackMaxButton)
	case runes(b.Value) > slackMaxValue:
		return refuse("button %q carries a value of %d characters, past %d", b.Text.Text, runes(b.Value), slackMaxValue)
	case runes(b.ActionID) > slackMaxID:
		return refuse("button %q has an action_id past %d characters", b.Text.Text, slackMaxID)
	}
	return nil
}
