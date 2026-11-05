package main

import (
	"encoding/json"
	"strings"
	"testing"
)

const testDiagramURL = "https://brook.tail1234.ts.net:61211/diagram/r9"

type drawnArtifact struct {
	URL         string `json:"url"`
	EntityType  string `json:"entity_type"`
	ExternalRef struct {
		ID   string `json:"id"`
		Type string `json:"type"`
	} `json:"external_ref"`
	EntityPayload struct {
		Attributes struct {
			Title struct {
				Text string `json:"text"`
			} `json:"title"`
			DisplayType  string `json:"display_type"`
			LastModified int64  `json:"metadata_last_modified"`
		} `json:"attributes"`
		Fields struct {
			Preview *struct {
				Type      string `json:"type"`
				SlackFile struct {
					ID string `json:"id"`
				} `json:"slack_file"`
				AltText string `json:"alt_text"`
			} `json:"preview"`
		} `json:"fields"`
		DisplayOrder []string `json:"display_order"`
		Actions      struct {
			Primary []struct {
				URL string `json:"url"`
			} `json:"primary_actions"`
		} `json:"actions"`
	} `json:"entity_payload"`
}

func drawnArtifacts(t *testing.T, metadata string) []drawnArtifact {
	t.Helper()
	var drawn struct {
		Entities []drawnArtifact `json:"entities"`
	}
	if err := json.Unmarshal([]byte(metadata), &drawn); err != nil {
		t.Fatalf("metadata %q: %v", metadata, err)
	}
	return drawn.Entities
}

func TestADiagramArtifactPostsAsAWorkObjectWhosePreviewIsItsUnsharedPicture(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	diagram := cardArtifact{Title: "How a reply reaches Slack", Kind: "diagram", URL: testDiagramURL, Image: writeFile(t, "diagram.png", pngBytes)}
	body := "The new path.\n\n[How a reply reaches Slack](" + testDiagramURL + ")"
	l.do(opDMStatus, map[string]any{"card": statusCard{Status: "info", Title: "Status", Body: body, Artifacts: []cardArtifact{diagram}}})

	post := l.slack.sent()[0]
	if !strings.Contains(post.blocks, testDiagramURL+"|How a reply reaches Slack") {
		t.Fatalf("blocks = %s, want the link kept for a client that draws no card", post.blocks)
	}
	if strings.Contains(post.blocks, `"type":"image"`) {
		t.Fatalf("blocks = %s, want the picture in the Work Object only", post.blocks)
	}
	entities := drawnArtifacts(t, post.metadata)
	if len(entities) != 1 {
		t.Fatalf("metadata = %s, want one Work Object", post.metadata)
	}
	got := entities[0]
	payload := got.EntityPayload
	if got.URL != testDiagramURL || got.EntityType != "slack#/entities/content_item" || got.ExternalRef.ID != "/diagram/r9" || got.ExternalRef.Type != "link" {
		t.Fatalf("entity = %+v", got)
	}
	if payload.Attributes.Title.Text != diagram.Title || payload.Attributes.DisplayType != "Diagram" || payload.Attributes.LastModified == 0 {
		t.Fatalf("attributes = %+v", payload.Attributes)
	}
	preview := payload.Fields.Preview
	if preview == nil || preview.Type != "slack#/types/image" || preview.SlackFile.ID == "" || preview.AltText != diagram.Title {
		t.Fatalf("preview = %+v, want the uploaded picture as a Slack-hosted image", preview)
	}
	if string(l.slack.uploads[preview.SlackFile.ID]) != pngBytes {
		t.Fatalf("preview names file %s, which is not the diagram's upload", preview.SlackFile.ID)
	}
	if len(l.slack.complete) != 1 {
		t.Fatalf("completed uploads = %v, want the one picture completed unshared", l.slack.complete)
	}
	if strings.Join(payload.DisplayOrder, ",") != "preview" || len(payload.Actions.Primary) != 1 || payload.Actions.Primary[0].URL != testDiagramURL {
		t.Fatalf("order %v, actions %+v", payload.DisplayOrder, payload.Actions)
	}
}

func TestAnArtifactWithNoImageDrawsNoPreview(t *testing.T) {
	raw, err := json.Marshal(cardArtifact{Title: "The session", Kind: "transcript", URL: testDiagramURL}.entity())
	if err != nil {
		t.Fatal(err)
	}
	drawn := drawnArtifacts(t, `{"entities":[`+string(raw)+`]}`)[0]
	if drawn.EntityPayload.Fields.Preview != nil || len(drawn.EntityPayload.DisplayOrder) != 0 {
		t.Fatalf("entity = %s, want no preview field", raw)
	}
}

func TestAnArtifactWithoutItsPartsIsRefusedBeforeItPosts(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	cases := map[string]cardArtifact{
		"no title":            {Kind: "diagram", URL: testDiagramURL},
		"no kind":             {Title: "t", URL: testDiagramURL},
		"http off tailnet":    {Title: "t", Kind: "diagram", URL: "http://100.1.2.3:61210/diagram/r9"},
		"long title":          {Title: strings.Repeat("t", cardMaxTitleLen+1), Kind: "diagram", URL: testDiagramURL},
		"image not a picture": {Title: "t", Kind: "diagram", URL: testDiagramURL, Image: writeFile(t, "a.txt", "plain words")},
	}
	for name, artifact := range cases {
		t.Run(name, func(t *testing.T) {
			body := mustJSON(map[string]any{"card": statusCard{Status: "info", Title: "Status", Artifacts: []cardArtifact{artifact}}})
			if _, err := invoke(l.ctx, l.deps, testSession, "/any", opDMStatus, body); err == nil {
				t.Fatal("dm-status took it")
			}
		})
	}
	six := make([]cardArtifact, cardMaxArtifacts+1)
	for i := range six {
		six[i] = cardArtifact{Title: "t", Kind: "diagram", URL: testDiagramURL}
	}
	if err := (statusCard{Artifacts: six}).checkArtifacts(""); err == nil {
		t.Fatal("six artifacts were taken")
	}
	if len(l.slack.sent()) != 0 {
		t.Fatalf("posts = %+v, want none", l.slack.sent())
	}
}
