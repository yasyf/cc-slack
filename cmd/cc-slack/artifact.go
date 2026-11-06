package main

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/slack-go/slack"
)

const (
	cardMaxArtifacts    = 5
	cardMaxPRObjects    = 3
	metadataLimit       = 3900
	artifactRefType     = "link"
	artifactProductName = "Drive dashboard"
	artifactOpenLabel   = "Open"
	prRefType           = "github-pr"
	prDisplayType       = "Pull request"
	prProductName       = "GitHub"
)

type prTag struct {
	key, label, value, color string
}

type cardArtifact struct {
	Title    string `json:"title"`
	Kind     string `json:"kind"`
	URL      string `json:"url"`
	Image    string `json:"image,omitempty"`
	Language string `json:"language,omitempty"`
	Size     string `json:"size,omitempty"`
}

func (c statusCard) checkArtifacts() error {
	if len(c.Artifacts) > cardMaxArtifacts {
		return fmt.Errorf("card carries %d artifacts, at most %d", len(c.Artifacts), cardMaxArtifacts)
	}
	for _, a := range c.Artifacts {
		if a.Title == "" || runes(a.Title) > cardMaxTitleLen || a.Kind == "" {
			return fmt.Errorf("card artifact %q needs a title of up to %d characters and a kind", a.Title, cardMaxTitleLen)
		}
	}
	return nil
}

func (c *statusCard) dropUnopenable(ctx context.Context, tailnet string) {
	c.Artifacts = slices.DeleteFunc(c.Artifacts, func(a cardArtifact) bool {
		if checkURL(a.URL, tailnet) {
			return false
		}
		notePostWarning(ctx, fmt.Sprintf("artifact %q posted with no Work Object, since Slack opens one only from an https url, or an http url on the [links] tailnet, of up to %d characters, and %s is neither", a.Title, cardMaxURLLen, a.URL))
		return true
	})
}

func (a cardArtifact) entity() slack.WorkObjectEntity {
	page, _ := url.Parse(a.URL)
	payload := map[string]any{
		"attributes": map[string]any{
			"title":                  map[string]any{"text": a.Title},
			"display_type":           strings.ToUpper(a.Kind[:1]) + a.Kind[1:],
			"product_name":           artifactProductName,
			"metadata_last_modified": time.Now().Unix(),
		},
		"actions": map[string]any{"primary_actions": []map[string]any{
			{"text": artifactOpenLabel, "action_id": cardLinkAction + "artifact", "url": a.URL},
		}},
	}
	var custom []map[string]any
	var order []string
	for _, fact := range []struct{ key, label, value string }{{"language", "Language", a.Language}, {"size", "Size", a.Size}} {
		if fact.value != "" {
			custom = append(custom, map[string]any{"key": fact.key, "label": fact.label, "type": "string", "value": fact.value})
			order = append(order, fact.key)
		}
	}
	if len(custom) > 0 {
		payload["custom_fields"] = custom
		payload["display_order"] = order
	}
	return slack.WorkObjectEntity{
		URL:           a.URL,
		ExternalRef:   slack.WorkObjectExternalRef{ID: page.Path, Type: artifactRefType},
		EntityType:    slack.EntityTypeContentItem,
		EntityPayload: payload,
	}
}

func (p cardPR) entity(live refState, waiting bool) slack.WorkObjectEntity {
	id, _ := parseRef(p.URL)
	shown := "#" + strconv.Itoa(id.Number)
	tags := []prTag{live.mergeTag(waiting)}
	if ci, known := live.ciTag(); known {
		tags = append(tags, ci)
	}
	custom := make([]map[string]any, len(tags))
	order := make([]string, len(tags))
	for i, tag := range tags {
		custom[i] = map[string]any{"key": tag.key, "label": tag.label, "type": "string", "value": tag.value, "tag_color": tag.color}
		order[i] = tag.key
	}
	return slack.WorkObjectEntity{
		URL:         p.URL,
		ExternalRef: slack.WorkObjectExternalRef{ID: strings.ReplaceAll(id.Owner+"/"+id.Repo, ".", "_") + ":" + strconv.Itoa(id.Number), Type: prRefType},
		EntityType:  slack.EntityTypeItem,
		EntityPayload: map[string]any{
			"attributes": map[string]any{
				"title":                  map[string]any{"text": strings.TrimSpace(shown + " " + p.Title)},
				"display_type":           prDisplayType,
				"display_id":             shown,
				"product_name":           prProductName,
				"metadata_last_modified": time.Now().Unix(),
			},
			"custom_fields": custom,
			"display_order": order,
			"actions": map[string]any{"primary_actions": []map[string]any{
				{"text": cardGitHubLabel, "action_id": cardLinkAction + "github", "url": p.URL},
			}},
		},
	}
}

func (s refState) mergeTag(waiting bool) prTag {
	tag := prTag{key: "status", label: "Merge", value: "Open", color: "blue"}
	switch {
	case s.State == "merged":
		tag.value, tag.color = "Merged", "green"
	case s.State == "closed":
		tag.value, tag.color = "Closed", "gray"
	case s.State == "queued":
		tag.value, tag.color = "Queued", "yellow"
	case waiting:
		tag.value = "Ready for your tap"
	case s.State == "draft":
		tag.value, tag.color = "Draft", "gray"
	}
	return tag
}

func (s refState) ciTag() (prTag, bool) {
	tag := prTag{key: "ci", label: "CI"}
	switch s.Checks {
	case "passed":
		tag.value, tag.color = "Green", "green"
	case "failed":
		tag.value, tag.color = "Red", "red"
	case "pending":
		tag.value, tag.color = "Running", "blue"
	default:
		return tag, false
	}
	return tag, true
}

func (c statusCard) prEntities(v cardView) []slack.WorkObjectEntity {
	live := func(p cardPR) refState {
		id, _ := parseRef(p.URL)
		return v.refs[id.key()]
	}
	_, answered := v.answers[c.Waits]
	waiting := c.Waits != "" && !answered && v.tapped == ""
	entities := make([]slack.WorkObjectEntity, 0, len(c.PRs)+len(c.Rows))
	for _, p := range c.PRs {
		entities = append(entities, p.entity(live(p), waiting))
	}
	for _, r := range c.Rows {
		if _, rowAnswered := v.answers[r.ID]; r.PR != nil {
			entities = append(entities, r.PR.entity(live(*r.PR), !rowAnswered && v.tapped == ""))
		}
	}
	return entities
}

func (c statusCard) workObjects() []slack.WorkObjectEntity {
	entities := make([]slack.WorkObjectEntity, len(c.Artifacts))
	for i, a := range c.Artifacts {
		entities[i] = a.entity()
	}
	return entities
}

func (c statusCard) entities(prs ...slack.WorkObjectEntity) []slack.MsgOption {
	seen := map[string]bool{}
	prs = slices.DeleteFunc(prs, func(e slack.WorkObjectEntity) bool {
		drawn := seen[e.URL]
		seen[e.URL] = true
		return drawn
	})
	entities := slices.Concat(prs[:min(len(prs), cardMaxPRObjects)], c.workObjects())
	for len(entities) > 0 && len(mustJSON(entities)) > metadataLimit {
		entities = entities[:len(entities)-1]
	}
	if len(entities) == 0 {
		return nil
	}
	return []slack.MsgOption{slack.MsgOptionWorkObjectMetadata(slack.WorkObjectMetadata{Entities: entities})}
}

func (c statusCard) hostsFiles() bool {
	return len(c.Images) > 0
}
