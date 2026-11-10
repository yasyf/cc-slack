package main

import (
	"cmp"
	"encoding/json"
	"slices"
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
		Fields       json.RawMessage `json:"fields"`
		CustomFields json.RawMessage `json:"custom_fields"`
		DisplayOrder []string        `json:"display_order"`
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

func TestADiagramArtifactPostsItsPictureOnTheCardBesideItsWorkObject(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	bucket := withBucket(t, l)
	diagram := cardArtifact{Title: "How a reply reaches Slack", Kind: "diagram", URL: testDiagramURL, Image: writeFile(t, "diagram.png", pngBytes)}
	body := "The new path.\n\n[How a reply reaches Slack](" + testDiagramURL + ")"
	l.do(opDMStatus, map[string]any{"card": statusCard{Status: "info", Title: "Status", Body: body, Artifacts: []cardArtifact{diagram}}})

	post := l.slack.sent()[0]
	if !strings.Contains(post.blocks, testDiagramURL+"|How a reply reaches Slack") {
		t.Fatalf("blocks = %s, want the link kept for a client that draws no card", post.blocks)
	}
	if sent := l.slack.sent(); len(sent) != 1 || len(l.slack.complete) != 0 {
		t.Fatalf("posts = %+v, completes = %v, want the card alone", sent, l.slack.complete)
	}
	if urls := imageURLs(t, post.blocks); len(urls) != 1 || bucket.body(t, urls[0]) != pngBytes {
		t.Fatalf("image urls %v, want the diagram drawn on the card from the bucket", urls)
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
	if len(payload.Fields) != 0 || len(payload.DisplayOrder) != 0 || len(payload.Actions.Primary) != 1 || payload.Actions.Primary[0].URL != testDiagramURL {
		t.Fatalf("payload = %+v, want a link with no picture inside the Work Object", payload)
	}
}

func TestACodeArtifactsWorkObjectShowsItsLanguageAndSizeAndNoBody(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	code := cardArtifact{Title: "The entry point", Kind: "code", URL: "https://brook.tail1234.ts.net:61211/artifact/r9/0", Language: "go", Size: "3 lines"}
	l.do(opDMStatus, map[string]any{"card": statusCard{Status: "info", Title: "Status", Body: "The new path.", Artifacts: []cardArtifact{code}}})

	entities := drawnArtifacts(t, l.slack.sent()[0].metadata)
	if len(entities) != 1 {
		t.Fatalf("entities = %+v, want the one Work Object", entities)
	}
	payload := entities[0].EntityPayload
	if payload.Attributes.DisplayType != "Code" || !slices.Equal(payload.DisplayOrder, []string{"language", "size"}) {
		t.Fatalf("payload = %+v", payload)
	}
	var facts []struct {
		Key   string `json:"key"`
		Label string `json:"label"`
		Value string `json:"value"`
	}
	if err := json.Unmarshal(payload.CustomFields, &facts); err != nil {
		t.Fatal(err)
	}
	if len(facts) != 2 || facts[0].Value != "go" || facts[1].Label != "Size" || facts[1].Value != "3 lines" {
		t.Fatalf("custom fields = %+v, want the language and the size", facts)
	}
}

func TestADiagramSlackCannotOpenStillPostsItsPictureAndSaysWhyItHasNoWorkObject(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	bucket := withBucket(t, l)
	diagram := cardArtifact{Title: "How a reply reaches Slack", Kind: "diagram", URL: "http://100.1.2.3:61210/diagram/r9", Image: writeFile(t, "diagram.png", pngBytes)}
	var res map[string]string
	if err := json.Unmarshal(l.do(opDMStatus, map[string]any{"card": statusCard{Status: "info", Title: "Status", Body: "The new path.", Artifacts: []cardArtifact{diagram}}}).Body, &res); err != nil {
		t.Fatal(err)
	}

	if res["ts"] == "" || !strings.Contains(res["warning"], "no Work Object") || !strings.Contains(res["warning"], diagram.URL) {
		t.Fatalf("dm-status = %v, want the card posted with a warning naming the link Slack cannot open", res)
	}
	shown := l.slack.sent()[0]
	if urls := imageURLs(t, shown.blocks); len(urls) != 1 || bucket.body(t, urls[0]) != pngBytes {
		t.Fatalf("image urls %v, want the diagram drawn on the card", urls)
	}
	if len(drawnArtifacts(t, cmp.Or(shown.metadata, "{}"))) != 0 {
		t.Fatalf("metadata = %s, want no Work Object", shown.metadata)
	}
}

func TestAnArtifactWithoutItsPartsIsRefusedBeforeItPosts(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	cases := map[string]cardArtifact{
		"no title":            {Kind: "diagram", URL: testDiagramURL},
		"no kind":             {Title: "t", URL: testDiagramURL},
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
	if err := (statusCard{Artifacts: six}).checkArtifacts(); err == nil {
		t.Fatal("six artifacts were taken")
	}
	if len(l.slack.sent()) != 0 {
		t.Fatalf("posts = %+v, want none", l.slack.sent())
	}
}

func TestAWebPImageArtifactIsDrawnOnTheCardAsAPNG(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	bucket := withBucket(t, l)
	shot := cardArtifact{Title: "The board after the fix", Kind: "image", URL: testDiagramURL, Image: writeFile(t, "board.webp", webpBytes)}
	l.do(opDMStatus, map[string]any{"card": statusCard{Status: "info", Title: "Status", Body: "The new bar.", Artifacts: []cardArtifact{shot}}})

	urls := imageURLs(t, l.slack.sent()[0].blocks)
	if len(urls) != 1 || !strings.Contains(urls[0], ".png?") || !strings.HasPrefix(bucket.body(t, urls[0]), "\x89PNG") {
		t.Fatalf("image urls %v, want the WebP drawn from a PNG in the bucket, since Slack draws only PNG, JPEG and GIF", urls)
	}
}
