package main

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/slack-go/slack"
)

const (
	cardMaxArtifacts    = 5
	artifactRefType     = "link"
	artifactProductName = "Drive dashboard"
	artifactOpenLabel   = "Open"
)

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

func (c statusCard) workObjects() []slack.WorkObjectEntity {
	entities := make([]slack.WorkObjectEntity, len(c.Artifacts))
	for i, a := range c.Artifacts {
		entities[i] = a.entity()
	}
	return entities
}

func (c statusCard) entities() []slack.MsgOption {
	if len(c.Artifacts) == 0 {
		return nil
	}
	return []slack.MsgOption{slack.MsgOptionWorkObjectMetadata(slack.WorkObjectMetadata{Entities: c.workObjects()})}
}

func (c statusCard) hostsFiles() bool {
	return len(c.Images) > 0
}
