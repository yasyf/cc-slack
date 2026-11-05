package main

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/slack-go/slack"
)

const (
	linkPreviewLimit = 3
	linkPreviewChars = 600
	prAboutChars     = 1500
	prFieldFloor     = 75
	metadataLimit    = 3900
	linkPreviewsLead = "What this links to:"
	linkPreviewsMore = "link-previews-more"
	rowLanded        = "landed"
	rowClosed        = "closed"
	rowTapped        = "tapped"
	rowOpen          = "open"
	rowNone          = "none"
	rowConflict      = "conflict"
	rowQueued        = "queued"
	ciGreen          = "green"
	ciRed            = "red"
	gradePass        = "pass"
	gradeFail        = "fail"
	tagGreen         = "green"
	tagRed           = "red"
	tagBlue          = "blue"
	tagYellow        = "yellow"
	tagGray          = "gray"
	prEntityType     = "slack#/entities/item"
	prEntityRefType  = "github-pr"
	prDisplayType    = "Pull request"
	prProductName    = "GitHub"
	prOpenLabel      = "Open on GitHub"
)

var (
	prLink     = regexp.MustCompile(`https://github\.com/([\w-]+)/([\w.-]+)/pull/(\d+)`)
	prLinkLine = regexp.MustCompile(`^[-*•\s]*(?:<?https://github\.com/[\w-]+/[\w.-]+/pull/\d+(?:\|[^>\n]*)?>?[\s,]*)+$`)
	buildLink  = regexp.MustCompile(`https://buildkite\.com/[\w.-]+/[\w.-]+/builds/\d+`)
)

type ledgerRow struct {
	Repo          string `json:"repo"`
	Number        int    `json:"number"`
	Title         string `json:"title"`
	Lane          string `json:"lane"`
	ForUsers      string `json:"for_users"`
	Risk          string `json:"risk"`
	Description   string `json:"description"`
	WaitsOn       string `json:"waits_on"`
	ReadyForTap   bool   `json:"ready_for_tap"`
	State         string `json:"state"`
	CI            string `json:"ci"`
	Approvers     string `json:"approvers"`
	Grade         string `json:"grade"`
	GradeFindings string `json:"grade_findings"`
	Mergeable     string `json:"mergeable"`
	Queue         string `json:"queue"`
	Hold          string `json:"hold"`
	CardURL       string `json:"card_url"`
}

func (r ledgerRow) ready() bool { return r.ReadyForTap && r.State == rowOpen }

func (r ledgerRow) ended() bool { return r.State == rowLanded || r.State == rowClosed }

func (r ledgerRow) approved() bool { return r.Approvers != "" && r.Approvers != rowNone }

func (r ledgerRow) held() bool { return r.Hold != "" && r.Hold != rowNone }

func (r ledgerRow) mergeTag() prField {
	field := prField{key: "status", label: "Merge"}
	switch {
	case r.State == rowLanded:
		field.value, field.color = "Merged", tagGreen
	case r.State == rowClosed:
		field.value, field.color = "Closed", tagGray
	case r.ready():
		field.value, field.color = "Ready for your tap", tagBlue
	case r.State == rowTapped:
		field.value, field.color = "Tapped", tagBlue
	case r.held():
		field.value, field.color = "On hold", tagYellow
	case r.CI == ciRed:
		field.value, field.color = "Blocked: CI red", tagRed
	case r.Mergeable == rowConflict:
		field.value, field.color = "Blocked: conflict", tagRed
	case !r.approved():
		field.value, field.color = "Blocked: review", tagYellow
	case r.Queue == rowQueued || r.State == rowQueued:
		field.value, field.color = "Queued", tagYellow
	default:
		field.value, field.color = "Open", tagBlue
	}
	return field
}

func (r ledgerRow) ciTag() prField {
	field := prField{key: "ci", label: "CI", value: "Running", color: tagBlue}
	switch r.CI {
	case ciGreen:
		field.value, field.color = "Green", tagGreen
	case ciRed:
		field.value, field.color = "Red", tagRed
	}
	return field
}

func (r ledgerRow) reviewTag() prField {
	if r.approved() {
		return prField{key: "review", label: "Review", value: "Approved", color: tagGreen}
	}
	return prField{key: "review", label: "Review", value: "Waiting", color: tagBlue}
}

func (r ledgerRow) gradeTag() prField {
	field := prField{key: "grade", label: "Grade", value: "Pending", color: tagBlue}
	switch r.Grade {
	case gradePass:
		field.value, field.color = "Pass", tagGreen
	case gradeFail:
		field.value, field.color = "Fail", tagRed
	}
	return field
}

func (r ledgerRow) waiting() string {
	if r.ended() || r.ready() || r.held() {
		return ""
	}
	return r.WaitsOn
}

func (r ledgerRow) findings() string {
	if r.GradeFindings == rowNone {
		return ""
	}
	return r.GradeFindings
}

type prField struct {
	key, label, value, color, link string
}

func (f prField) custom(limit int) map[string]any {
	custom := map[string]any{"key": f.key, "label": f.label, "type": "string", "value": clipRunes(f.value, limit)}
	if f.color != "" {
		custom["tag_color"] = f.color
	}
	if f.link != "" {
		custom["link"] = f.link
	}
	return custom
}

func (r ledgerRow) entity(url, cardLink string) slack.WorkObjectEntity {
	lines := []prField{
		r.mergeTag(), r.ciTag(), r.reviewTag(), r.gradeTag(),
		{key: "changes", label: "What it changes", value: strings.TrimSpace(r.ForUsers)},
		{key: "waits", label: "Waiting on", value: strings.TrimSpace(r.waiting())},
		{key: "risk", label: "Risk", value: strings.TrimSpace(r.Risk)},
		{key: "findings", label: "Findings", value: r.findings()},
		{key: "lane", label: "Lane", value: r.Lane},
	}
	if cardLink != "" {
		lines = append(lines, prField{key: "card", label: "Your tap", value: "Open the card", link: cardLink})
	}
	custom := make([]map[string]any, 0, len(lines)+1)
	order := make([]string, 0, len(lines)+1)
	for _, line := range lines {
		if line.value != "" {
			custom = append(custom, line.custom(linkPreviewChars))
			order = append(order, line.key)
		}
	}
	if about := strings.TrimSpace(r.Description); about != "" {
		custom = append(custom, prField{key: "about", label: "Description", value: about}.custom(prAboutChars))
		order = append(order, "about")
	}
	shown := "#" + strconv.Itoa(r.Number)
	return slack.WorkObjectEntity{
		URL:         cmp.Or(r.CardURL, url),
		ExternalRef: slack.WorkObjectExternalRef{ID: strings.ReplaceAll(r.Repo, ".", "_") + ":" + strconv.Itoa(r.Number), Type: prEntityRefType},
		EntityType:  prEntityType,
		EntityPayload: map[string]any{
			"attributes": map[string]any{
				"title":                  map[string]any{"text": strings.TrimSpace(shown + " " + r.Title)},
				"display_type":           prDisplayType,
				"display_id":             shown,
				"product_name":           prProductName,
				"metadata_last_modified": time.Now().Unix(),
			},
			"custom_fields": custom,
			"display_order": order,
			"actions": map[string]any{"primary_actions": []map[string]any{
				{"text": prOpenLabel, "action_id": cardLinkAction + "github", "url": url},
			}},
		},
	}
}

func linkPreviewsText(prs []slack.WorkObjectEntity) string {
	lines := make([]string, 0, 1+len(prs))
	lines = append(lines, linkPreviewsLead)
	for _, pr := range prs {
		attributes, _ := pr.EntityPayload["attributes"].(map[string]any)
		title, _ := attributes["title"].(map[string]any)
		text, _ := title["text"].(string)
		actions, _ := pr.EntityPayload["actions"].(map[string]any)
		primary, _ := actions["primary_actions"].([]map[string]any)
		link, _ := primary[0]["url"].(string)
		lines = append(lines, slackLink(link, text))
	}
	return strings.Join(lines, "\n")
}

func entitiesOption(entities []slack.WorkObjectEntity) []slack.MsgOption {
	entities = fitEntities(entities)
	if len(entities) == 0 {
		return nil
	}
	return []slack.MsgOption{slack.MsgOptionWorkObjectMetadata(slack.WorkObjectMetadata{Entities: entities})}
}

func fitEntities(entities []slack.WorkObjectEntity) []slack.WorkObjectEntity {
	for !fitsMetadata(entities) {
		entities = entities[:len(entities)-1]
	}
	return entities
}

func fitsMetadata(entities []slack.WorkObjectEntity) bool {
	for limit := prAboutChars; limit >= prFieldFloor; limit /= 2 {
		clipEntityFields(entities, limit)
		if encoded, err := json.Marshal(entities); err == nil && len(encoded) <= metadataLimit {
			return true
		}
	}
	return false
}

func clipEntityFields(entities []slack.WorkObjectEntity, limit int) {
	for _, entity := range entities {
		custom, _ := entity.EntityPayload["custom_fields"].([]map[string]any)
		for _, field := range custom {
			if value, ok := field["value"].(string); ok {
				field["value"] = clipRunes(value, limit)
			}
		}
	}
}

func (a *app) linkPreviews(ctx context.Context, id identity, channelID, text string) (string, []slack.WorkObjectEntity, []slack.Attachment) {
	if channelID != id.DMChannelID {
		return text, nil, nil
	}
	text, prs, more := a.prPreviews(ctx, id, text)
	previews, moreBuilds := a.buildPreviews(ctx, id, text, linkPreviewLimit-len(prs))
	if more += moreBuilds; more > 0 {
		line := fmt.Sprintf("+%d more not shown here", more)
		previews = append(previews, slack.Attachment{Fallback: line, Blocks: slack.Blocks{BlockSet: []slack.Block{mrkdwn(linkPreviewsMore, line)}}})
	}
	return text, prs, previews
}

func (a *app) prPreviews(ctx context.Context, id identity, text string) (string, []slack.WorkObjectEntity, int) {
	command := id.Config.PRLinks.PreviewCommand
	prs := linkedPRRefs(text)
	if len(command) == 0 || len(prs) == 0 {
		return text, nil, 0
	}
	rows, err := readLedgerRows(ctx, command, prs)
	if err != nil {
		a.poller.log.Printf("post keeps its PR links with no previews: %v", err)
		return text, nil, 0
	}
	var previews []slack.WorkObjectEntity
	drawn := map[string]bool{}
	more := 0
	for _, pr := range prs {
		row, ok := rows[pr.key()]
		if !ok {
			continue
		}
		next := append(slices.Clip(previews), a.prPreview(id, pr, row))
		if len(next) > linkPreviewLimit || !fitsMetadata(next) {
			more++
			continue
		}
		previews = next
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

func (a *app) prPreview(id identity, pr refID, row ledgerRow) slack.WorkObjectEntity {
	cardLink := ""
	if ts, thread, ok := a.cards.openPRCard(id.DMChannelID, pr); ok && row.ready() {
		cardLink = id.permalink(id.DMChannelID, ts, thread)
	}
	return row.entity(prURL(pr), cardLink)
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
