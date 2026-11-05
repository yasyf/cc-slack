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
	linkPreviewLimit = 3
	linkPreviewChars = 600
	linkPreviewsLead = "What this links to:"
	linkPreviewsMore = "link-previews-more"
	rowLanded        = "landed"
	rowClosed        = "closed"
	rowTapped        = "tapped"
	rowOpen          = "open"
	rowNone          = "none"
)

var (
	prLink     = regexp.MustCompile(`https://github\.com/([\w-]+)/([\w.-]+)/pull/(\d+)`)
	prLinkLine = regexp.MustCompile(`^[-*•\s]*(?:<?https://github\.com/[\w-]+/[\w.-]+/pull/\d+(?:\|[^>\n]*)?>?[\s,]*)+$`)
	buildLink  = regexp.MustCompile(`https://buildkite\.com/[\w.-]+/[\w.-]+/builds/\d+`)
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
	case r.Hold != "" && r.Hold != rowNone:
		return "*On hold.*" + r.checks()
	}
	waits := mrkdwnEscapes.Replace(strings.TrimRight(r.WaitsOn, ". ")) + "."
	if r.State == rowTapped {
		return "*" + waits + "*" + r.checks()
	}
	return "*Waiting:* " + waits + r.checks()
}

func (r ledgerRow) checks() string {
	if r.CI == "" {
		return ""
	}
	return " CI " + mrkdwnEscapes.Replace(r.CI) + "."
}

func (a *app) linkPreviews(ctx context.Context, id identity, channelID, text string, onCard []refID) (string, []slack.Attachment) {
	if channelID != id.DMChannelID {
		return text, nil
	}
	text, previews, more := a.prPreviews(ctx, id, text, onCard)
	builds, moreBuilds := a.buildPreviews(ctx, id, text, linkPreviewLimit-len(previews))
	previews = append(previews, builds...)
	if more += moreBuilds; more > 0 {
		line := fmt.Sprintf("+%d more not shown here", more)
		previews = append(previews, slack.Attachment{Fallback: line, Blocks: slack.Blocks{BlockSet: []slack.Block{mrkdwn(linkPreviewsMore, line)}}})
	}
	return text, previews
}

func (a *app) prPreviews(ctx context.Context, id identity, text string, onCard []refID) (string, []slack.Attachment, int) {
	command := id.Config.PRLinks.PreviewCommand
	prs := linkedPRRefs(text)
	if len(command) == 0 || len(prs) == 0 {
		return text, nil, 0
	}
	drawn := map[string]bool{}
	for _, pr := range onCard {
		drawn[pr.key()] = true
	}
	prs = slices.DeleteFunc(prs, func(pr refID) bool { return drawn[pr.key()] })
	if len(prs) == 0 {
		return withoutRepeatedLinks(text, drawn), nil, 0
	}
	rows, err := readLedgerRows(ctx, command, prs)
	if err != nil {
		a.poller.log.Printf("post keeps its PR links with no previews: %v", err)
		return text, nil, 0
	}
	var previews []slack.Attachment
	more := 0
	for _, pr := range prs {
		row, ok := rows[pr.key()]
		if !ok {
			continue
		}
		if len(previews) == linkPreviewLimit {
			more++
			continue
		}
		previews = append(previews, a.prPreview(id, pr, row))
		drawn[pr.key()] = true
	}
	return withoutRepeatedLinks(text, drawn), previews, more
}

func (a *app) buildPreviews(ctx context.Context, id identity, text string, room int) ([]slack.Attachment, int) {
	var links []string
	for _, link := range buildLink.FindAllString(text, -1) {
		if !slices.Contains(links, link) {
			links = append(links, link)
		}
	}
	if id.Config.Buildkite == nil || len(links) == 0 {
		return nil, 0
	}
	lookup, cancel := context.WithTimeout(ctx, prLookupPatience)
	defer cancel()
	token := a.cards.host(refHostBuildkite, id.Config).token
	var previews []slack.Attachment
	for i, link := range links {
		if len(previews) == room {
			return previews, len(links) - i
		}
		ref, ok := parseRef(link)
		if !ok {
			continue
		}
		build, err := a.cards.client.readBuild(lookup, token, ref)
		if err != nil {
			a.poller.log.Printf("%s stays a link: %v", link, err)
			continue
		}
		previews = append(previews, build.preview(link, ref))
	}
	return previews, 0
}

func (b buildkiteBuild) preview(link string, ref refID) slack.Attachment {
	title := fmt.Sprintf("%s build %d", ref.Repo, ref.Number)
	lines := []string{"*" + slackLink(link, title) + "*"}
	if message, _, _ := strings.Cut(strings.TrimSpace(b.Message), "\n"); message != "" {
		lines = append(lines, mrkdwnEscapes.Replace(clipRunes(message, linkPreviewChars)))
	}
	state := strings.ReplaceAll(b.State, "_", " ")
	lines = append(lines, "*"+strings.ToUpper(state[:1])+state[1:]+".*")
	return slack.Attachment{Fallback: title, Blocks: slack.Blocks{BlockSet: []slack.Block{section("", strings.Join(lines, "\n"))}}}
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
	standing := row.standing()
	if ts, thread, ok := a.cards.openPRCard(id.DMChannelID, pr); ok && row.ready() {
		standing += " " + slackLink(id.permalink(id.DMChannelID, ts, thread), "Open the card")
	}
	body := strings.Join([]string{"*" + slackLink(prURL(pr), title) + "*", plainMrkdwn(row.ForUsers), standing}, "\n\n")
	return slack.Attachment{Fallback: title, Blocks: slack.Blocks{BlockSet: slices.Concat(bodyPages(body, cardMaxBlocks, cardMaxVisible, false)...)}}
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
