package main

import (
	"errors"
	"fmt"
	"maps"
	"strings"
)

const slackMaxPreviews = 5

var errEditPastFive = errors.New("this edit links more than five Slack messages, and Slack draws five a message while an edit cannot add a page: post the extra lines as a new line in the thread")

type crowdedLine struct {
	line, links int
}

func (c crowdedLine) refusal() string {
	return fmt.Sprintf("line %d links %d Slack messages, and Slack draws five a message: put each card on its own line", c.line, c.links)
}

func (c crowdedLine) String() string {
	return fmt.Sprintf("%d:1 links-past-five: %s", c.line, c.refusal())
}

func drawnLinks(text string, draws func(permalinkRef) bool) map[string]bool {
	text = withoutTables(text)
	links := map[string]bool{}
	for _, link := range messageLink.FindAllString(text, -1) {
		if ref, err := parsePermalink(link); err == nil && draws(ref) {
			links[link] = true
		}
	}
	for _, link := range alertLink.FindAllString(text, -1) {
		links[link] = true
	}
	return links
}

func crowdedLineIn(text string, draws func(permalinkRef) bool) (crowdedLine, bool) {
	for number, line := range strings.Split(text, "\n") {
		if links := len(drawnLinks(line, draws)); links > slackMaxPreviews {
			return crowdedLine{number + 1, links}, true
		}
	}
	return crowdedLine{}, false
}

func linkPages(text string, draws func(permalinkRef) bool) ([]string, error) {
	if crowded, found := crowdedLineIn(text, draws); found {
		return nil, errors.New(crowded.refusal())
	}
	lines := strings.Split(text, "\n")
	var pages []string
	for start := 0; ; {
		end := pageEnd(lines, start, draws)
		if end == len(lines) {
			return append(pages, strings.Join(lines[start:], "\n")), nil
		}
		pages = append(pages, strings.TrimRight(strings.Join(lines[start:end], "\n"), " \t\n"))
		start = end
		for strings.TrimSpace(lines[start]) == "" {
			start++
		}
	}
}

func pageEnd(lines []string, start int, draws func(permalinkRef) bool) int {
	seen := map[string]bool{}
	blank, content := start, false
	for i := start; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "" {
			if content {
				blank = i
			}
			continue
		}
		maps.Copy(seen, drawnLinks(lines[i], draws))
		if len(seen) > slackMaxPreviews {
			if blank > start {
				return blank
			}
			return i
		}
		content = true
	}
	return len(lines)
}

func (b opBody) pagesIn(id identity, channelID string, draws func(permalinkRef) bool) ([]string, error) {
	if channelID != id.DMChannelID || len(b.Blocks) > 0 || b.Card != nil {
		return []string{b.Text}, nil
	}
	return linkPages(withoutHidden(b.Text), draws)
}

func refuseEditPastFive(id identity, b opBody, draws func(permalinkRef) bool) error {
	if b.ChannelID != id.DMChannelID && b.ChannelID != id.UserID || len(drawnLinks(withoutHidden(b.Text), draws)) <= slackMaxPreviews {
		return nil
	}
	return errEditPastFive
}
