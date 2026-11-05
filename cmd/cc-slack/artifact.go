package main

import (
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
	workImageType       = "slack#/types/image"
)

type cardArtifact struct {
	Title   string             `json:"title"`
	Kind    string             `json:"kind"`
	URL     string             `json:"url"`
	Image   string             `json:"image,omitempty"`
	Picture *slack.FileSummary `json:"picture,omitempty"`
}

func (c statusCard) checkArtifacts(tailnet string) error {
	if len(c.Artifacts) > cardMaxArtifacts {
		return fmt.Errorf("card carries %d artifacts, at most %d", len(c.Artifacts), cardMaxArtifacts)
	}
	for _, a := range c.Artifacts {
		if a.Title == "" || runes(a.Title) > cardMaxTitleLen || a.Kind == "" || !checkURL(a.URL, tailnet) {
			return fmt.Errorf("card artifact %q needs a title of up to %d characters, a kind, and a url of up to %d", a.Title, cardMaxTitleLen, cardMaxURLLen)
		}
	}
	return nil
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
	if a.Picture != nil {
		payload["fields"] = map[string]any{"preview": map[string]any{"type": workImageType, "slack_file": map[string]any{"id": a.Picture.ID}, "alt_text": a.Title}}
		payload["display_order"] = []string{"preview"}
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
	return len(c.Images) > 0 || slices.ContainsFunc(c.Artifacts, func(a cardArtifact) bool { return a.Picture != nil })
}
