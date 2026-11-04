package main

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/slack-go/slack"
)

const (
	prPreviewLimit = 5
	prPreviewChars = 600
	prPreviewsLead = "The PRs this names:"
	prPreviewsMore = "pr-previews-more"
	rowLanded      = "landed"
	rowClosed      = "closed"
	rowOpen        = "open"
	rowNoHold      = "none"
)

var (
	prLink     = regexp.MustCompile(`https://github\.com/([\w-]+)/([\w.-]+)/pull/(\d+)`)
	prLinkLine = regexp.MustCompile(`^[-*•\s]*(?:<?https://github\.com/[\w-]+/[\w.-]+/pull/\d+(?:\|[^>\n]*)?>?[\s,]*)+$`)
	sentence   = regexp.MustCompile(`(?m)^.+?(?:[.!?](?:\s|$)|$)`)
)

type ledgerRow struct {
	Repo        string `json:"repo"`
	Number      int    `json:"number"`
	Title       string `json:"title"`
	ForUsers    string `json:"for_users"`
	WaitsOn     string `json:"waits_on"`
	ReadyForTap bool   `json:"ready_for_tap"`
	State       string `json:"state"`
	CI          string `json:"ci"`
	Hold        string `json:"hold"`
}

func (r ledgerRow) ready() bool { return r.ReadyForTap && r.State == rowOpen }

func (r ledgerRow) standing() string {
	switch {
	case r.State == rowLanded:
		return "*Landed.*"
	case r.State == rowClosed:
		return "*Closed.*"
	case r.ready():
		return "*Ready for your tap.*" + r.checks()
	case r.Hold != "" && r.Hold != rowNoHold:
		return "*On hold.*" + r.checks()
	}
	return "*Waiting:* " + mrkdwnEscapes.Replace(clipRunes(strings.TrimRight(r.WaitsOn, ". "), prPreviewChars)) + "." + r.checks()
}

func (r ledgerRow) checks() string {
	if r.CI == "" {
		return ""
	}
	return " CI " + mrkdwnEscapes.Replace(r.CI) + "."
}

func (r ledgerRow) words() string {
	return mrkdwnEscapes.Replace(clipRunes(strings.TrimSpace(sentence.FindString(strings.TrimSpace(r.ForUsers))), prPreviewChars))
}

func (a *app) prPreviews(ctx context.Context, id identity, channelID, text string) (string, []slack.Attachment) {
	command := id.Config.PRLinks.PreviewCommand
	prs := linkedPRRefs(text)
	if channelID != id.DMChannelID || len(command) == 0 || len(prs) == 0 {
		return text, nil
	}
	rows, err := readLedgerRows(ctx, command, prs)
	if err != nil {
		a.poller.log.Printf("post keeps its PR links with no previews: %v", err)
		return text, nil
	}
	var previews []slack.Attachment
	drawn := map[string]bool{}
	more := 0
	for _, pr := range prs {
		row, ok := rows[pr.key()]
		if !ok {
			continue
		}
		if len(previews) == prPreviewLimit {
			more++
			continue
		}
		previews = append(previews, a.prPreview(id, pr, row))
		drawn[pr.key()] = true
	}
	if more > 0 {
		line := fmt.Sprintf("+%d more not shown here", more)
		previews = append(previews, slack.Attachment{Fallback: line, Blocks: slack.Blocks{BlockSet: []slack.Block{mrkdwn(prPreviewsMore, line)}}})
	}
	return withoutRepeatedLinks(text, drawn), previews
}

func linkedPRRefs(text string) []refID {
	var prs []refID
	for _, m := range prLink.FindAllStringSubmatch(text, -1) {
		pr := repoRef(m[1], m[2])
		pr.Number, _ = strconv.Atoi(m[3])
		if !slices.Contains(prs, pr) {
			prs = append(prs, pr)
		}
	}
	return prs
}

func readLedgerRows(ctx context.Context, command []string, prs []refID) (map[string]ledgerRow, error) {
	lookup, cancel := context.WithTimeout(ctx, prLookupPatience)
	defer cancel()
	asked := make([]map[string]any, len(prs))
	for i, pr := range prs {
		asked[i] = map[string]any{"repo": pr.Owner + "/" + pr.Repo, "number": pr.Number}
	}
	out, err := runCardCommand(lookup, command, map[string]any{"prs": asked})
	if err != nil {
		return nil, err
	}
	var rows []ledgerRow
	if err := json.Unmarshal(out, &rows); err != nil {
		return nil, fmt.Errorf("decode %s: %w", command[0], err)
	}
	byPR := make(map[string]ledgerRow, len(rows))
	for _, row := range rows {
		ref, ok := parseRepo(row.Repo)
		if !ok {
			return nil, fmt.Errorf("%s named %q as the repo of #%d, not owner/name", command[0], row.Repo, row.Number)
		}
		ref.Number = row.Number
		byPR[ref.key()] = row
	}
	return byPR, nil
}

func (a *app) prPreview(id identity, pr refID, row ledgerRow) slack.Attachment {
	title := fmt.Sprintf("#%d %s", pr.Number, row.Title)
	lines := []string{"*" + slackLink(prURL(pr), title) + "*"}
	if words := row.words(); words != "" {
		lines = append(lines, words)
	}
	standing := row.standing()
	if ts, thread, ok := a.cards.openPRCard(id.DMChannelID, pr); ok && row.ready() {
		standing += " " + slackLink(id.permalink(id.DMChannelID, ts, thread), "Open the card")
	}
	return slack.Attachment{Fallback: title, Blocks: slack.Blocks{BlockSet: []slack.Block{section("", strings.Join(append(lines, standing), "\n"))}}}
}

func withoutRepeatedLinks(text string, drawn map[string]bool) string {
	lines := strings.Split(text, "\n")
	shown := maps.Clone(drawn)
	for _, line := range lines {
		if !prLinkLine.MatchString(line) {
			for _, pr := range linkedPRRefs(line) {
				shown[pr.key()] = true
			}
		}
	}
	kept := slices.DeleteFunc(slices.Clone(lines), func(line string) bool {
		return prLinkLine.MatchString(line) && !slices.ContainsFunc(linkedPRRefs(line), func(pr refID) bool { return !shown[pr.key()] })
	})
	if strings.TrimSpace(strings.Join(kept, "\n")) == "" {
		return text
	}
	return strings.TrimRight(strings.Join(kept, "\n"), "\n")
}
