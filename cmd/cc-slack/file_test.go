package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"text/tabwriter"

	"github.com/slack-go/slack"
)

const shotURL = "https://files.slack.com/files-pri/T1-F1/download/shot.png"

var shot = slack.File{ID: "F1", Name: "shot.png", Mimetype: "image/png", URLPrivateDownload: shotURL, Size: 4}

func TestFileDownloadsAnAttachment(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	l.slack.serveFile(shot, []byte("\x89PNG"))

	var res fileResult
	if err := json.Unmarshal(l.do(opFile, map[string]any{"url": "https://acme.slack.com/files/U1/F1/shot.png"}).Body, &res); err != nil {
		t.Fatal(err)
	}
	want := fileResult{FileID: "F1", Name: "shot.png", Mimetype: "image/png", Size: 4, Path: filepath.Join(filesDir(), "F1.png")}
	if res != want {
		t.Fatalf("file = %+v, want %+v", res, want)
	}
	if got, err := os.ReadFile(res.Path); err != nil || string(got) != "\x89PNG" {
		t.Fatalf("downloaded %q, %v", got, err)
	}

	out := filepath.Join(t.TempDir(), "nested", "josh.png")
	if err := json.Unmarshal(l.do(opFile, map[string]any{"file_id": "F1", "out": out}).Body, &res); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(out); err != nil || string(got) != "\x89PNG" || res.Path != out {
		t.Fatalf("downloaded %q to %s, %v", got, res.Path, err)
	}

	var buf strings.Builder
	w := tabwriter.NewWriter(&buf, 0, 0, 2, ' ', 0)
	renderFile(w, res)
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if line := strings.TrimSpace(buf.String()); line != out+"  image/png  4" {
		t.Fatalf("rendered file = %q", line)
	}
}

func TestFileRefusesWhatIsNotADownloadableFile(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	out := filepath.Join(t.TempDir(), "shot.png")
	l.slack.serveFile(shot, []byte("<!DOCTYPE html><html>sign in</html>"))
	l.slack.fail("files.info", slack.SlackErrorResponse{Err: "file_not_found"})
	for _, c := range []struct {
		name string
		body map[string]any
		want string
	}{
		{"message permalink", map[string]any{"url": "https://acme.slack.com/archives/C1/p1800000000000300"}, "Slack file id"},
		{"channel id", map[string]any{"file_id": "C1"}, "Slack file id"},
		{"relative out", map[string]any{"file_id": "F1", "out": "shot.png"}, "absolute out path"},
		{"unknown file", map[string]any{"file_id": "F404", "out": out}, "can't download this file (file_not_found)"},
		{"login page served", map[string]any{"file_id": "F1", "out": out}, "cannot read it"},
	} {
		raw, _ := json.Marshal(c.body)
		if _, err := invoke(l.ctx, l.deps, testSession, "/any", opFile, raw); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: %v, want %q", c.name, err, c.want)
		}
	}
	if _, err := os.Stat(out); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused download left %s: %v", out, err)
	}
}
