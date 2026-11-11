package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type renderOutput struct {
	Text   string            `json:"text"`
	Blocks []json.RawMessage `json:"blocks"`
	Fold   []json.RawMessage `json:"fold"`
}

func renderCardFile(t *testing.T, card string, args ...string) (renderOutput, error) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(configEnv, "")
	t.Setenv("HOME", dir)
	path := filepath.Join(dir, "card.json")
	if err := os.WriteFile(path, []byte(card), 0o600); err != nil {
		t.Fatal(err)
	}
	c := cardCmd()
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&out)
	c.SetArgs(append([]string{"render", "--card", path, "--cwd", dir}, args...))
	if err := c.ExecuteContext(context.Background()); err != nil {
		return renderOutput{}, err
	}
	var got renderOutput
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("output %q is not JSON: %v", out.String(), err)
	}
	return got, nil
}

func renderedBlocks(blocks []json.RawMessage) string {
	var b strings.Builder
	for _, block := range blocks {
		b.Write(block)
	}
	return b.String()
}

func TestRenderPrintsEachOptionsWordsBesideItsPick(t *testing.T) {
	got, err := renderCardFile(t, `{"title":"How do refused calls recover?","waits":"recover","recommended":"Wait durably","options":["Wait durably","Raise the limit"],"hints":{"Wait durably":"A refused call waits and resumes."}}`)
	if err != nil {
		t.Fatal(err)
	}
	blocks := renderedBlocks(got.Blocks)
	for _, want := range []string{"*1. Wait durably*", "A refused call waits and resumes.", "*2. Raise the limit*", cardPickLabel, cardExplainLabel} {
		if !strings.Contains(blocks, want) {
			t.Errorf("blocks lack %q: %s", want, blocks)
		}
	}
	if got.Text != "🙋 How do refused calls recover?" {
		t.Errorf("text = %q", got.Text)
	}
}

func TestRenderDrawsAnOptionWithNoWordsAsAButton(t *testing.T) {
	got, err := renderCardFile(t, `{"title":"Merge it?","waits":"merge","recommended":"Merge","options":["Merge","Hold"]}`)
	if err != nil {
		t.Fatal(err)
	}
	blocks := renderedBlocks(got.Blocks)
	if strings.Contains(blocks, "1. Merge") || strings.Contains(blocks, cardPickLabel) || !strings.Contains(blocks, `"Hold"`) {
		t.Errorf("want Merge and Hold as plain buttons with no rows: %s", blocks)
	}
}

func TestRenderKeepsEveryWordOfALongOptionAcrossSections(t *testing.T) {
	sentence := "Each stopped run starts again from the top. "
	words := strings.Repeat(sentence, 100)
	raw, err := json.Marshal(statusCard{Title: "Which?", Waits: "pick", Recommended: "Long", Options: []string{"Long", "Short"}, Hints: map[string]string{"Long": words + "Last words."}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := renderCardFile(t, string(raw))
	if err != nil {
		t.Fatal(err)
	}
	blocks := renderedBlocks(got.Blocks)
	if n := strings.Count(blocks, "starts again from the top"); n != 100 || !strings.Contains(blocks, "Last words.") {
		t.Errorf("%d of 100 sentences and last words %v in %s", n, strings.Contains(blocks, "Last words."), blocks)
	}
	for _, block := range got.Blocks {
		var section struct {
			Text struct {
				Text string `json:"text"`
			} `json:"text"`
		}
		if err := json.Unmarshal(block, &section); err != nil {
			t.Fatal(err)
		}
		if n := runes(section.Text.Text); n > slackMaxSection {
			t.Errorf("a section holds %d characters, over Slack's %d", n, slackMaxSection)
		}
	}
}

func TestRenderDrawsABodyPictureWhereItsLineSits(t *testing.T) {
	shot := writeFile(t, "queue.png", pngBytes)
	card := statusCard{Title: "Two queues", Body: "Two layouts.\n*Show me*\n![The queue in layout A](" + shot + ")\nAfter the picture."}
	got, err := renderCardFile(t, string(mustJSON(card)))
	if err != nil {
		t.Fatal(err)
	}
	blocks := renderedBlocks(got.Blocks)
	at := strings.Index(blocks, shot)
	if at < 0 || strings.Index(blocks, "*Show me*") > at || strings.Index(blocks, "After the picture.") < at || !strings.Contains(blocks, "The queue in layout A") {
		t.Fatalf("blocks %s, want the picture drawn from %s under its Show me line, captioned", blocks, shot)
	}
}

func TestRenderRefusesABodyPictureItCannotDraw(t *testing.T) {
	words := writeFile(t, "notes.png", "plain words")
	for line, want := range map[string]string{
		"![The queue](shots/queue.png)":                                "absolute path",
		"![](" + writeFile(t, "a.png", pngBytes) + ")":                 "needs a caption",
		"![The queue](" + filepath.Join(t.TempDir(), "gone.png") + ")": "read the card's picture",
		"![The notes](" + words + ")":                                  "no PNG, JPEG, GIF, or WebP image",
	} {
		card := statusCard{Title: "Two queues", Body: "Two layouts.\n" + line}
		if _, err := renderCardFile(t, string(mustJSON(card))); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", line, err, want)
		}
	}
}

func TestRenderRefusesWhatPostingRefuses(t *testing.T) {
	card := `{"title":"Which?","waits":"pick","options":["A","B"]}`
	if _, err := renderCardFile(t, card); err == nil || !strings.Contains(err.Error(), "recommends one of its options") {
		t.Errorf("err = %v, want the waiting card's refusal", err)
	}
	if _, err := renderCardFile(t, card, "--mirrored"); err != nil {
		t.Errorf("a card cc-present sends may leave recommended out: %v", err)
	}
	if _, err := renderCardFile(t, `{"title":"Which?","options":["A","B","C","D","E"]}`); err == nil || !strings.Contains(err.Error(), "at most") {
		t.Errorf("err = %v, want the option count refused", err)
	}
}
