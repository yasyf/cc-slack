package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/slack-go/slack"
)

const testBucket = "cc-slack-pictures"

type fakeBucket struct {
	mu   sync.Mutex
	puts map[string]string
	n    int
}

func (b *fakeBucket) put() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.n
}

func (b *fakeBucket) body(t *testing.T, link string) string {
	t.Helper()
	u, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.puts[u.Path]
}

func withBucket(t *testing.T, l *liveDaemon) *fakeBucket {
	t.Helper()
	b, srv := serveBucket(t)
	id, err := loadIdentity(l.ctx, l.srv.DB())
	if err != nil {
		t.Fatal(err)
	}
	id.Config.Pictures = pictureConfig{Endpoint: srv.URL, Bucket: testBucket, KeyID: "pictures-key", Secret: "pictures-secret"}
	if err := saveIdentity(l.ctx, l.srv.DB(), id); err != nil {
		t.Fatal(err)
	}
	l.app.cards.usePictures(id.Config.Pictures)
	return b
}

func imageURLs(t *testing.T, raw string) []string {
	t.Helper()
	var blocks []struct {
		Type     string `json:"type"`
		ImageURL string `json:"image_url"`
	}
	if err := json.Unmarshal([]byte(raw), &blocks); err != nil {
		t.Fatalf("blocks %s: %v", raw, err)
	}
	var urls []string
	for _, b := range blocks {
		if b.Type == "image" {
			urls = append(urls, b.ImageURL)
		}
	}
	return urls
}

func serveBucket(t *testing.T) (*fakeBucket, *httptest.Server) {
	t.Helper()
	b := &fakeBucket{puts: map[string]string{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Method != http.MethodPut || !strings.Contains(r.Header.Get("Authorization"), "Credential=pictures-key/") {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		b.mu.Lock()
		b.puts[r.URL.Path] = string(body)
		b.n++
		b.mu.Unlock()
	}))
	t.Cleanup(srv.Close)
	return b, srv
}

func loggedInWithPictures(t *testing.T, l *liveDaemon, endpoint string) {
	t.Helper()
	l.slack.setUser(&slack.User{ID: testUser, Name: "ym", RealName: "Yasyf Mohamedali", Profile: slack.UserProfile{
		Email: "ym@example.com", Image512: "https://avatars/ym.png", DisplayName: "yasyf",
	}})
	l.do(opLogin, map[string]any{
		"token": "xoxb-test", "email": "ym@example.com", "socket_url": "ws://127.0.0.1:1/live", "repo_root": "/repo", "capt_hook_path": testCaptHook,
		"avatar_key_id": "r2-key", "avatar_secret": "r2-secret",
		"config": map[string]any{"pictures": map[string]string{"endpoint": endpoint, "bucket": testBucket, "key_id": "pictures-key", "secret": "pictures-secret"}},
	})
}

func TestACardWithAPictureMakesNoPostButTheCard(t *testing.T) {
	l := startDaemon(t)
	bucket, srv := serveBucket(t)
	loggedInWithPictures(t, l, srv.URL)
	board := cardArtifact{Title: "The board", Kind: "image", URL: testDiagramURL, Image: writeFile(t, "board.png", pngBytes)}
	l.do(opDMStatus, map[string]any{"card": statusCard{Status: "info", Title: "Two layouts", Body: "Two layouts.", Artifacts: []cardArtifact{board}}})

	sent := l.slack.sent()
	if len(sent) != 1 {
		t.Fatalf("posts = %+v, want the card alone: no picture thread, no share", sent)
	}
	if len(l.slack.uploads) != 0 || len(l.slack.complete) != 0 {
		t.Fatalf("slack uploads %v, completes %v, want the picture kept out of Slack's files", l.slack.uploads, l.slack.complete)
	}
	if len(bucket.puts) != 1 {
		t.Fatalf("bucket puts = %v, want the one picture", bucket.puts)
	}
	for path, body := range bucket.puts {
		if !strings.HasPrefix(path, "/cc-slack-pictures/"+picturePrefix) || body != pngBytes {
			t.Fatalf("put %s = %q, want the picture's bytes under %s", path, body, picturePrefix)
		}
		key := strings.TrimPrefix(path, "/cc-slack-pictures/")
		card := sent[0].blocks
		if !strings.Contains(card, `"image_url":"`+srv.URL+"/cc-slack-pictures/"+key+"?") || !strings.Contains(card, "X-Amz-Expires=604800") || strings.Contains(card, "slack_file") {
			t.Fatalf("card blocks = %s, want the picture drawn from a 7-day signed link to %s and no Slack file", card, key)
		}
	}
}

func TestACardWithPicturesAndNoBucketIsRefusedBeforeItPosts(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	shot := cardArtifact{Title: "The board", Kind: "image", URL: testDiagramURL, Image: writeFile(t, "a.png", pngBytes)}
	body := mustJSON(map[string]any{"card": statusCard{Status: "info", Title: "Status", Artifacts: []cardArtifact{shot}}})
	if _, err := invoke(l.ctx, l.deps, testSession, "/any", opDMStatus, body); err == nil || !strings.Contains(err.Error(), "[pictures]") {
		t.Fatalf("dm-status = %v, want a refusal naming the [pictures] bucket", err)
	}
	if sent := l.slack.sent(); len(sent) != 0 {
		t.Fatalf("posts = %+v, want none", sent)
	}
}
