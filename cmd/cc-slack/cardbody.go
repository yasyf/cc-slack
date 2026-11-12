package main

import (
	"cmp"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"github.com/slack-go/slack"
)

const (
	cardMaxBlocks    = 50
	cardMaxVisible   = 9000
	cardMaxPageBlock = 45
	cardMaxTableRows = 100
	cardMaxTableCols = 20
	cardEmptyCell    = "—"
	slackMaxCaption  = 150
	slackMaxHeader   = 150
	slackMaxSection  = 3000
	slackMaxField    = 2000
	slackMaxTable    = 10000
	slackMaxFields   = 10
	slackMaxActions  = 25
	slackMaxButton   = 75
	slackMaxValue    = 2000
	slackMaxID       = 255
	slackMaxCarousel = 10
	fencedTableRows  = 3
	fencedTableCols  = 3
)

var (
	mdBold          = regexp.MustCompile(`\*\*([^*\n]+)\*\*`)
	mdLink          = regexp.MustCompile(`\[([^\]\n]+)\]\((https?://[^)\s]+)\)`)
	mdHeading       = regexp.MustCompile(`^#{1,6} +(.+?)\s*#*$`)
	mdBullet        = regexp.MustCompile(`(?m)^([ \t]*)[-*] +`)
	mdEndsOnHeading = regexp.MustCompile(`(?:^|\n)(?:#{1,6} +[^\n]*|\*{1,2}[^*\n]+\*{1,2})\s*$`)
	mdRule          = regexp.MustCompile(`^\|?[\s:|-]*-[\s:|-]*\|?$`)
	mdPicture       = regexp.MustCompile(`^!\[([^\]\n]*)\]\(\s*(?:<([^<>\n]+)>|([^\s()<>]+))\s*\)$`)
	mdMarks         = regexp.MustCompile(`(?m)\*\*[^*\n]+\*\*|\]\(https?://|^#{1,6} |^\s*\||^` + "```" + `chart\s*$`)
	cellToken       = regexp.MustCompile("<(https?://[^|>\\s]+)\\|([^>]+)>|\\[([^\\]\\n]+)\\]\\((https?://[^)\\s]+)\\)|\\*\\*?([^*\\n]+)\\*\\*?|`([^`\\n]+)`")
	htmlComment     = regexp.MustCompile(`(?s)[ \t]*<!--.*?-->`)
	htmlSummary     = regexp.MustCompile(`(?is)<summary>(.*?)</summary>`)
	htmlFold        = regexp.MustCompile(`(?i)</?details[^>]*>|<br\s*/?>`)
	slackEscapable  = regexp.MustCompile(`<(?:https?://|mailto:|[@#!])[^<>\n]*>|&(?:amp|lt|gt);|[&<>]`)
	bareURL         = regexp.MustCompile("<[^<>\\n]*>|<https?://[^\\s<>]*|https?://[^\\s<>\"`]+")
)

type bodyPiece struct {
	text     string
	line     int
	table    [][]string
	picture  *cardImage
	gallery  []*slack.ImageBlock
	chart    *slack.DataVisualizationBlock
	chartErr error
}

func bodyPieces(body string) []bodyPiece {
	var pieces []bodyPiece
	var para []string
	number := strings.Count(body[:len(body)-len(strings.TrimLeftFunc(body, unicode.IsSpace))], "\n")
	paraLine := 0
	fence := make([]string, 0, 1)
	var table [][]string
	flushPara := func() {
		if len(para) > 0 {
			pieces = append(pieces, bodyPiece{text: strings.Join(para, "\n"), line: paraLine})
			para = nil
		}
	}
	addPara := func(line string) {
		if len(para) == 0 {
			paraLine = number
		}
		para = append(para, line)
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
		number++
		switch {
		case strings.HasPrefix(trimmed, "```") && inFence:
			pieces = append(pieces, fencedPieces(fence[0], fence[1:], line)...)
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
		case mdPicture.MatchString(trimmed):
			flushPara()
			flushTable()
			m := mdPicture.FindStringSubmatch(trimmed)
			pieces = append(pieces, bodyPiece{picture: &cardImage{Title: strings.TrimSpace(m[1]), Source: cmp.Or(m[2], m[3])}})
		case mdHeading.MatchString(trimmed):
			flushPara()
			flushTable()
			addPara("*" + strings.Trim(mrkdwnLine(mdHeading.FindStringSubmatch(trimmed)[1]), "*") + "*")
		default:
			flushTable()
			addPara(mrkdwnLine(line))
		}
	}
	flushPara()
	flushTable()
	if inFence {
		pieces = append(pieces, fencedPieces(fence[0], fence[1:], "```")...)
	}
	return pieces
}

func fencedPieces(open string, lines []string, closing string) []bodyPiece {
	if strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(open), "```")) == "chart" {
		source := strings.Join(lines, "\n")
		chart, err := chartBlock(source)
		return []bodyPiece{{text: open + "\n" + strings.Trim(source, "\n") + "\n" + closing, chart: chart, chartErr: err}}
	}
	var pieces []bodyPiece
	code := func(lines []string) {
		if kept := strings.Trim(strings.Join(lines, "\n"), "\n"); strings.TrimSpace(kept) != "" {
			pieces = append(pieces, bodyPiece{text: open + "\n" + slackEscaped(kept) + "\n" + closing})
		}
	}
	from := 0
	for at := 0; at < len(lines); {
		rows, used := columnRows(lines[at:])
		if len(rows) < fencedTableRows {
			at++
			continue
		}
		code(lines[from:at])
		pieces = append(pieces, bodyPiece{table: rows})
		at += used
		from = at
	}
	code(lines[from:])
	return pieces
}

func columnRows(lines []string) ([][]string, int) {
	head := []rune(strings.TrimRight(lines[0], " "))
	var starts []int
	for at, r := range head {
		if r != ' ' && (at == 0 || at >= 2 && head[at-1] == ' ' && head[at-2] == ' ') {
			starts = append(starts, at)
		}
	}
	if len(starts) < fencedTableCols || starts[0] != 0 {
		return nil, 0
	}
	rows := [][]string{columnCells(head, starts)}
	for used, line := range lines[1:] {
		if trimmed := strings.TrimSpace(line); trimmed != "" && mdRule.MatchString(trimmed) {
			continue
		}
		row := []rune(strings.TrimRight(line, " "))
		cells := columnCells(row, starts)
		if len(row) == 0 || row[0] == ' ' || slices.ContainsFunc(starts[1:], func(at int) bool { return at >= len(row) || row[at-1] != ' ' && row[at] != ' ' }) || slices.Contains(cells, "") {
			return rows, used + 1
		}
		rows = append(rows, cells)
	}
	return rows, len(lines)
}

func columnCells(row []rune, starts []int) []string {
	cells := make([]string, len(starts))
	for i, at := range starts {
		end := len(row)
		if i+1 < len(starts) {
			end = min(starts[i+1], len(row))
		}
		cells[i] = strings.TrimSpace(string(row[min(at, end):end]))
	}
	return cells
}

func mrkdwnText(text string) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		lines[i] = mrkdwnLine(line)
	}
	return strings.Join(lines, "\n")
}

func withoutTables(text string) string {
	return strings.Join(slices.DeleteFunc(strings.Split(text, "\n"), func(line string) bool {
		return strings.HasPrefix(strings.TrimSpace(line), "|")
	}), "\n")
}

func mrkdwnLine(line string) string {
	line = slackEscaped(mdLink.ReplaceAllString(line, "<$2|$1>"))
	line = mdBold.ReplaceAllString(line, "*$1*")
	return mdBullet.ReplaceAllString(line, "$1• ")
}

func explicitLinks(text string) string {
	return bareURL.ReplaceAllStringFunc(text, func(found string) string {
		if strings.HasPrefix(found, "<") {
			return found
		}
		link := prURLTrailing.ReplaceAllString(found, "")
		return "<" + link + ">" + found[len(link):]
	})
}

func slackEscaped(text string) string {
	return slackEscapable.ReplaceAllStringFunc(text, func(found string) string {
		if len(found) > 1 {
			return found
		}
		return cardWordEscapes.Replace(found)
	})
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

func tableBlock(rows [][]string) *slack.TableBlock {
	columns := 0
	for _, row := range rows {
		columns = max(columns, len(row))
	}
	table := slack.NewTableBlock("").WithColumnSettings(slices.Repeat([]slack.ColumnSetting{{Align: slack.ColumnAlignmentLeft, IsWrapped: true}}, columns)...)
	for at, row := range rows {
		cells := make([]slack.TableCell, columns)
		for i := range cells {
			text := cellAt(row, i)
			switch {
			case text == "":
				cells[i] = slack.NewTableRawTextCell(cardEmptyCell)
			case at == 0:
				cells[i] = slack.NewTableRichTextCell(slack.NewRichTextSection(slack.NewRichTextSectionTextElement(text, &slack.RichTextSectionTextStyle{Bold: true})))
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

func tableCell(text string) slack.TableCell {
	found := cellToken.FindAllStringSubmatchIndex(text, -1)
	if found == nil {
		return slack.NewTableRawTextCell(text)
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
	return slack.NewTableRichTextCell(slack.NewRichTextSection(elements...))
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
	text = explicitLinks(text)
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
}

func bodyPages(body string, firstBlocks, firstChars int, firstNative bool, pictures map[string]*slack.ImageBlock) [][]slack.Block {
	page := &bodyPage{}
	var pages [][]slack.Block
	maxBlocks, maxChars := firstBlocks, firstChars
	turn := func() {
		if len(pages) == 0 || len(page.blocks) > 0 {
			pages = append(pages, page.blocks)
		}
		page = &bodyPage{}
		maxBlocks, maxChars = cardMaxPageBlock, cardMaxVisible
	}
	put := func(block slack.Block, chars, limit int) {
		if len(page.blocks) >= maxBlocks || page.chars+chars > limit {
			turn()
		}
		page.blocks = append(page.blocks, block)
		page.chars += chars
	}
	place := func(part string) {
		put(slack.NewSectionBlock(mrkdwnObject(part), nil, nil), runes(part), maxChars)
	}
	add := func(text string) {
		for _, part := range splitSection(text, cardMaxSectionLen) {
			room := maxChars - page.chars
			if len(page.blocks) < maxBlocks && len(pages) == 0 && room > 0 && runes(part) > room && !strings.HasPrefix(part, "```") {
				head := sectionCut([]rune(part), room)
				place(strings.TrimRight(head, " \n"))
				part = strings.TrimLeft(strings.TrimPrefix(part, head), " \n")
			}
			place(part)
		}
	}
	for _, piece := range galleryPieces(bodyPieces(body), pictures) {
		if block, chars, limit := piece.block(firstNative || len(pages) > 0, maxChars); block != nil {
			put(block, chars, limit)
			continue
		}
		add(piece.words())
	}
	return append(pages, page.blocks)
}

func galleryPieces(pieces []bodyPiece, pictures map[string]*slack.ImageBlock) []bodyPiece {
	var out []bodyPiece
	var run []*slack.ImageBlock
	show := func() {
		for shown := range slices.Chunk(run, slackMaxCarousel) {
			out = append(out, bodyPiece{gallery: shown})
		}
		run = nil
	}
	for _, piece := range pieces {
		if piece.picture != nil && pictures[piece.picture.Source] != nil {
			run = append(run, pictures[piece.picture.Source])
			continue
		}
		show()
		out = append(out, piece)
	}
	show()
	return out
}

func (p bodyPiece) block(native bool, limit int) (slack.Block, int, int) {
	switch {
	case p.gallery != nil:
		chars := 0
		for _, picture := range p.gallery {
			chars += runes(picture.AltText)
		}
		return gallery(p.gallery), chars, limit
	case !native:
		return nil, 0, 0
	case p.chart != nil:
		return p.chart, runes(p.chart.Title), limit
	case p.table != nil && tableFits(p.table):
		return tableBlock(p.table), tableChars(p.table), max(limit, cardMaxVisible)
	}
	return nil, 0, 0
}

func (p bodyPiece) words() string {
	switch {
	case p.picture != nil:
		return pictureCaption(*p.picture)
	case p.table == nil:
		return p.text
	case len(p.table) > 1:
		return tableLines(p.table)
	}
	return strings.Join(p.table[0], " · ")
}

func gallery(pictures []*slack.ImageBlock) slack.Block {
	if len(pictures) == 1 {
		return pictures[0]
	}
	cards := make([]*slack.CardBlock, 0, len(pictures))
	for _, picture := range pictures {
		hero := slack.NewImageBlockElementSlackFile(picture.SlackFile, picture.AltText)
		if picture.ImageURL != "" {
			hero = slack.NewImageBlockElement(picture.ImageURL, picture.AltText)
		}
		title := slack.NewTextBlockObject(slack.PlainTextType, clipRunes(picture.AltText, slackMaxCaption), false, false)
		cards = append(cards, slack.NewCardBlock().WithHeroImage(hero).WithTitle(title))
	}
	return slack.NewCarouselBlock(cards...)
}

func galleries(pictures []*slack.ImageBlock) []slack.Block {
	var blocks []slack.Block
	for run := range slices.Chunk(pictures, slackMaxCarousel) {
		blocks = append(blocks, gallery(run))
	}
	return blocks
}

func pictureCaption(picture cardImage) string {
	caption := cmp.Or(picture.Title, picture.Source)
	if !strings.HasPrefix(picture.Source, "https://") && !strings.HasPrefix(picture.Source, "http://") {
		return caption
	}
	return slackLink(picture.Source, caption)
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

func tableFits(rows [][]string) bool {
	return len(rows) > 1 && len(rows) <= cardMaxTableRows && tableChars(rows) <= slackMaxTable &&
		!slices.ContainsFunc(rows, func(row []string) bool { return len(row) > cardMaxTableCols })
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
	charts, tables := 0, 0
	for i, b := range blocks {
		refuse := func(format string, args ...any) error {
			return blockLimitError{i, string(b.BlockType()), fmt.Sprintf(format, args...)}
		}
		if _, chart := b.(*slack.DataVisualizationBlock); chart {
			charts++
		}
		if charts > slackMaxCharts {
			return refuse("a message holds at most %d charts", slackMaxCharts)
		}
		if table, ok := b.(*slack.TableBlock); ok {
			for _, row := range table.Rows {
				for _, cell := range row {
					tables += runes(cellText(cell))
				}
			}
		}
		if tables > slackMaxTable {
			return refuse("a message's tables hold at most %d characters", slackMaxTable)
		}
		if id := b.ID(); id != "" {
			if runes(id) > slackMaxID || ids[id] {
				return refuse("block_id %q repeats or runs past %d characters", id, slackMaxID)
			}
			ids[id] = true
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
	case *slack.TableBlock:
		if len(block.Rows) > cardMaxTableRows || slices.ContainsFunc(block.Rows, func(row []slack.TableCell) bool { return len(row) > cardMaxTableCols }) {
			return refuse("it runs past %d rows or %d columns", cardMaxTableRows, cardMaxTableCols)
		}
	case *slack.DataVisualizationBlock:
		return checkChart(block, refuse)
	case *slack.CarouselBlock:
		return checkCarousel(block, refuse)
	}
	return nil
}

func checkCarousel(c *slack.CarouselBlock, refuse func(string, ...any) error) error {
	if n := len(c.Elements); n == 0 || n > slackMaxCarousel {
		return refuse("it holds %d cards, not 1 to %d", n, slackMaxCarousel)
	}
	for _, card := range c.Elements {
		if card.Title != nil && runes(card.Title.Text) > slackMaxCaption {
			return refuse("a card's title is %d characters, past %d", runes(card.Title.Text), slackMaxCaption)
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
