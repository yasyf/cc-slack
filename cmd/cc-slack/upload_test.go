package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/slack-go/slack"

	"github.com/yasyf/cc-interact/agent"
)

const (
	pngBytes = "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"
	gifBytes = "GIF89a\x01\x00\x01\x00"
)

func writeFile(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

type uploadRequest struct{ filename, length string }

type slackServer struct {
	mu        sync.Mutex
	url       string
	requested []uploadRequest
	uploaded  map[string]string
	completed map[string]string
	infoCalls map[string]int
	posts     []map[string]string
	auth      string
}

func (s *slackServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case r.URL.Path == "/files.getUploadURLExternal":
		_ = r.ParseForm()
		s.requested = append(s.requested, uploadRequest{r.Form.Get("filename"), r.Form.Get("length")})
		id := "F" + r.Form.Get("filename")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "file_id": id, "upload_url": s.url + "/upload/" + id})
	case strings.HasPrefix(r.URL.Path, "/upload/"):
		file, _, err := r.FormFile("file")
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		body, _ := io.ReadAll(file)
		s.uploaded[strings.TrimPrefix(r.URL.Path, "/upload/")] = string(body)
		_, _ = w.Write([]byte("OK"))
	case r.URL.Path == "/files.completeUploadExternal":
		_ = r.ParseForm()
		for _, key := range []string{"files", "channel_id", "channels", "thread_ts", "initial_comment", "username", "icon_url"} {
			s.completed[key] = r.Form.Get(key)
		}
		s.auth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "files": json.RawMessage(r.Form.Get("files"))})
	case r.URL.Path == "/files.info":
		_ = r.ParseForm()
		id := r.Form.Get("file")
		s.infoCalls[id]++
		file := map[string]any{"id": id}
		if s.infoCalls[id] > 1 {
			file["original_w"] = 640
			if channel := s.completed["channel_id"]; channel != "" {
				file["shares"] = map[string]any{"public": map[string]any{channel: []map[string]string{{"ts": "1800000000.000700", "thread_ts": s.completed["thread_ts"]}}}}
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "file": file})
	case r.URL.Path == "/chat.postMessage":
		_ = r.ParseForm()
		post := map[string]string{}
		for key := range r.Form {
			post[key] = r.Form.Get(key)
		}
		s.posts = append(s.posts, post)
		if len(s.posts) == 1 {
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "invalid_blocks"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": post["channel"], "ts": "1800000000.000900"})
	default:
		http.NotFound(w, r)
	}
}

func TestPostImagesUploadsPrivatelyThenPostsImageBlocksAsTheUser(t *testing.T) {
	fake := &slackServer{uploaded: map[string]string{}, completed: map[string]string{}, infoCalls: map[string]int{}}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	fake.url = srv.URL
	api := newSlackClient("xoxb-test", srv.URL+"/", srv.Client())
	id := identity{UserID: testUser, DisplayName: "yasyf", AvatarURL: "https://avatars/ym.png"}
	png, gif := writeFile(t, "a.png", pngBytes), writeFile(t, "b.gif", gifBytes)

	ch, ts, err := postImages(context.Background(), api, id, testChannel, testThread, "the map", nil, []string{png, gif})
	if err != nil {
		t.Fatal(err)
	}
	if ch != testChannel || ts != "1800000000.000900" {
		t.Fatalf("postImages = %s/%s", ch, ts)
	}
	want := []uploadRequest{{"a.png", "16"}, {"b.gif", "10"}}
	if len(fake.requested) != 2 || fake.requested[0] != want[0] || fake.requested[1] != want[1] {
		t.Fatalf("getUploadURLExternal = %+v, want %+v", fake.requested, want)
	}
	if fake.uploaded["Fa.png"] != pngBytes || fake.uploaded["Fb.gif"] != gifBytes {
		t.Fatalf("uploaded = %q", fake.uploaded)
	}
	if fake.completed["channel_id"] != "" || fake.completed["channels"] != "" || fake.completed["thread_ts"] != "" || fake.completed["initial_comment"] != "" {
		t.Fatalf("completeUploadExternal = %+v, want a private upload shared nowhere", fake.completed)
	}
	if fake.infoCalls["Fa.png"] < 2 || fake.infoCalls["Fb.gif"] < 2 {
		t.Fatalf("files.info calls = %v, want each file polled until processed", fake.infoCalls)
	}
	if len(fake.posts) != 2 {
		t.Fatalf("chat.postMessage calls = %d, want a retry after invalid_blocks", len(fake.posts))
	}
	post := fake.posts[1]
	if post["username"] != "yasyf (agent)" || post["icon_url"] != "https://avatars/ym.png" || post["thread_ts"] != testThread || post["text"] != "the map" {
		t.Fatalf("post = %+v, want yasyf with the user's avatar in the thread", post)
	}
	var blocks []map[string]any
	if err := json.Unmarshal([]byte(post["blocks"]), &blocks); err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 3 || blocks[0]["type"] != "section" || blocks[1]["type"] != "image" || blocks[2]["type"] != "image" {
		t.Fatalf("blocks = %s", post["blocks"])
	}
	if ids := imageIDs(t, post["blocks"]); strings.Join(ids, ",") != "Fa.png,Fb.gif" || blocks[1]["alt_text"] != "a.png" {
		t.Fatalf("image blocks = %+v", blocks[1:])
	}
}

func imageIDs(t *testing.T, raw string) []string {
	t.Helper()
	var blocks []struct {
		Type      string `json:"type"`
		SlackFile struct {
			ID string `json:"id"`
		} `json:"slack_file"`
	}
	if err := json.Unmarshal([]byte(raw), &blocks); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, b := range blocks {
		if b.Type == "image" {
			ids = append(ids, b.SlackFile.ID)
		}
	}
	return ids
}

func TestReplyAttachesImagesAsTheUser(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	l.slack.pending = 2
	png, gif := writeFile(t, "a.png", pngBytes), writeFile(t, "b.gif", gifBytes)

	var res postResult
	if err := json.Unmarshal(l.post(opReply, map[string]any{
		"channel_id": testChannel, "thread_ts": testThread, "text": "the map", "files": []string{png, gif},
	}).Body, &res); err != nil {
		t.Fatal(err)
	}
	sent := l.slack.sent()
	if len(sent) != 1 || sent[0].ts != res.TS || sent[0].username != "yasyf (agent)" || sent[0].threadTS != testThread {
		t.Fatalf("reply = %+v, sent = %+v, want one post as yasyf in the thread", res, sent)
	}
	if done := l.slack.completed(); len(done) != 1 || len(done[0]) != 2 {
		t.Fatalf("completeUploadExternal = %+v, want one call for both files", done)
	}
}

func TestPostRetriesInvalidBlocksWhileSlackProcesses(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	l.slack.fail("chat.postMessage", slack.SlackErrorResponse{Err: "invalid_blocks"})
	l.post(opSend, map[string]any{"channel_id": testChannel, "files": []string{writeFile(t, "a.png", pngBytes)}})
	if n := l.slack.callCount("chat.postMessage"); n != 2 {
		t.Fatalf("chat.postMessage calls = %d, want one retry", n)
	}
	if sent := l.slack.sent(); len(sent) != 1 || sent[0].text != "a.png" {
		t.Fatalf("sent = %+v, want the file names as the notification text", sent)
	}
}

func TestDMStatusAttachesImagesWithoutText(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	var status map[string]string
	if err := json.Unmarshal(l.do(opDMStatus, map[string]any{"files": []string{writeFile(t, "a.png", pngBytes)}}).Body, &status); err != nil {
		t.Fatal(err)
	}
	sent := l.slack.sent()
	if len(sent) != 1 || status["thread_ts"] != sent[0].ts || status["channel_id"] != "D"+testUser {
		t.Fatalf("dm-status = %v, sent = %+v, want the image post to open the session thread", status, sent)
	}
}

func TestShareFilesSharesAsTheUserAndFindsTheMessage(t *testing.T) {
	fake := &slackServer{uploaded: map[string]string{}, completed: map[string]string{}, infoCalls: map[string]int{}}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	fake.url = srv.URL
	api := newSlackClient("xoxb-test", srv.URL+"/", srv.Client())
	id := identity{UserID: testUser, DisplayName: "yasyf", AvatarURL: "https://avatars/ym.png"}
	notes := writeFile(t, "notes.txt", "edge reference 18.2f3a")

	ch, ts, err := shareFiles(context.Background(), api, id, testChannel, testThread, "the <https://example.com|reference>", []string{notes})
	if err != nil {
		t.Fatal(err)
	}
	if ch != testChannel || ts != "1800000000.000700" {
		t.Fatalf("shareFiles = %s/%s, want the share's message ts", ch, ts)
	}
	want := map[string]string{
		"channel_id": testChannel, "thread_ts": testThread, "initial_comment": "the <https://example.com|reference>",
		"username": "yasyf (agent)", "icon_url": "https://avatars/ym.png", "files": `[{"id":"Fnotes.txt","title":"notes.txt"}]`,
	}
	for key, value := range want {
		if fake.completed[key] != value {
			t.Fatalf("completeUploadExternal %s = %q, want %q", key, fake.completed[key], value)
		}
	}
	if fake.auth != "Bearer xoxb-test" || fake.uploaded["Fnotes.txt"] != "edge reference 18.2f3a" || len(fake.posts) != 0 {
		t.Fatalf("auth = %q, uploaded = %q, posts = %d, want one authorized share and no chat.postMessage", fake.auth, fake.uploaded, len(fake.posts))
	}
}

func TestReplyAttachesAnyFileAsTheUser(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	notes, png := writeFile(t, "notes.md", "# reference\n"), writeFile(t, "a.png", pngBytes)

	var res postResult
	if err := json.Unmarshal(l.post(opReply, map[string]any{
		"channel_id": testChannel, "thread_ts": testThread, "text": "the reference", "files": []string{notes, png},
	}).Body, &res); err != nil {
		t.Fatal(err)
	}
	sent := l.slack.sent()
	if len(sent) != 1 || sent[0].ts != res.TS || sent[0].username != "yasyf (agent)" || sent[0].threadTS != testThread || sent[0].text != "the reference" {
		t.Fatalf("reply = %+v, sent = %+v, want one share as yasyf in the thread", res, sent)
	}
	if done := l.slack.completed(); len(done) != 1 || len(done[0]) != 2 || l.slack.callCount("chat.postMessage") != 0 {
		t.Fatalf("completeUploadExternal = %+v, want one share of both files and no chat.postMessage", done)
	}
}

func TestBroadcastRefusesFilesThatAreNotImages(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	raw, _ := json.Marshal(l.hooked(opReply, map[string]any{
		"channel_id": testChannel, "thread_ts": testThread, "text": "landed", "broadcast": true, "files": []string{writeFile(t, "notes.txt", "log")},
	}))
	if _, err := invoke(l.ctx, l.deps, testSession, "/any", opReply, raw); err == nil || !strings.Contains(err.Error(), "nothing was sent") {
		t.Fatalf("broadcast reply with a text file: %v, want a refusal that hands the ticket back", err)
	}
	if n := l.slack.callCount("files.getUploadURLExternal"); n != 0 {
		t.Fatalf("getUploadURLExternal calls = %d after a refused broadcast", n)
	}
}

func TestShareFilesReportsAnUnknownTSAsMaybeSent(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	l.slack.fail("files.info", slack.SlackErrorResponse{Err: "ratelimited"})
	raw, _ := json.Marshal(l.hooked(opReply, map[string]any{
		"channel_id": testChannel, "thread_ts": testThread, "text": "the log", "files": []string{writeFile(t, "run.log", "boom")},
	}))
	_, err := invoke(l.ctx, l.deps, testSession, "/any", opReply, raw)
	if err == nil || !strings.Contains(err.Error(), "may be up") || strings.Contains(err.Error(), "nothing was sent") || l.slack.callCount("files.completeUploadExternal") != 1 {
		t.Fatalf("reply after a failed files.info: %v, want one share and no hand-back", err)
	}
}

func TestShareFilesSurfacesRateLimits(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)
	err := newSlackClient("xoxb-test", srv.URL+"/", srv.Client()).ShareFilesContext(context.Background(), fileShare{channelID: testChannel})
	var rate *slack.RateLimitedError
	if !errors.As(err, &rate) || rate.RetryAfter != 7*time.Second {
		t.Fatalf("ShareFilesContext on a 429 = %v, want a rate limit of 7s", err)
	}
}

func TestPostsRefuseUnreadableAttachments(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	huge := writeFile(t, "huge.log", "x")
	if err := os.Truncate(huge, maxFileBytes+1); err != nil {
		t.Fatal(err)
	}
	for _, files := range [][]string{
		{"a.png"},
		{writeFile(t, "empty.txt", "")},
		{t.TempDir()},
		{huge},
		{filepath.Join(t.TempDir(), "missing.png")},
	} {
		raw, _ := json.Marshal(l.hooked(opSend, map[string]any{"channel_id": testChannel, "text": "hi", "files": files}))
		if _, err := invoke(l.ctx, l.deps, testSession, "/any", opSend, raw); err == nil || !strings.Contains(err.Error(), "attachment") {
			t.Fatalf("send with %v: %v", files, err)
		}
	}
	if n := l.slack.callCount("files.getUploadURLExternal"); n != 0 {
		t.Fatalf("getUploadURLExternal calls = %d after refused attachments", n)
	}
}

func TestStageAttachmentsHandsTheDaemonCopies(t *testing.T) {
	report := writeFile(t, "report.pdf", "%PDF-1.7 first")
	again := writeFile(t, "report.pdf", "%PDF-1.7 second")
	locked := writeFile(t, "locked.pdf", "%PDF-1.7 locked")
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	untouched := []string{"relative.pdf", t.TempDir(), filepath.Join(t.TempDir(), "missing.pdf"), writeFile(t, "empty.txt", ""), locked}
	body, err := json.Marshal(map[string]any{"channel_id": testChannel, "text": "the deck", "files": append([]string{report, again}, untouched...)})
	if err != nil {
		t.Fatal(err)
	}
	staged, cleanup, err := stageAttachments(body)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		ChannelID string   `json:"channel_id"`
		Text      string   `json:"text"`
		Files     []string `json:"files"`
	}
	if err := json.Unmarshal(staged, &got); err != nil {
		t.Fatal(err)
	}
	if got.ChannelID != testChannel || got.Text != "the deck" {
		t.Fatalf("staged body = %s, want the other fields kept", staged)
	}
	for i, original := range []string{report, again} {
		copied := got.Files[i]
		if copied == original || filepath.Base(copied) != "report.pdf" {
			t.Fatalf("files[%d] = %q, want a copy of %q under its own name", i, copied, original)
		}
		want, _ := os.ReadFile(original)
		if have, err := os.ReadFile(copied); err != nil || !bytes.Equal(have, want) {
			t.Fatalf("copy of %s = %q, %v; want %q", original, have, err, want)
		}
	}
	if strings.Join(got.Files[2:], ",") != strings.Join(untouched, ",") {
		t.Fatalf("files = %v, want %v passed through for the daemon to refuse", got.Files[2:], untouched)
	}
	cleanup()
	if _, err := os.Stat(filepath.Dir(filepath.Dir(got.Files[0]))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staging dir after cleanup: %v, want it gone", err)
	}
	if _, err := os.Stat(report); err != nil {
		t.Fatalf("original after cleanup: %v", err)
	}
}

func TestStageAttachmentsLeavesABodyWithoutFiles(t *testing.T) {
	body := json.RawMessage(`{"channel_id":"C1","text":"hi","files":[]}`)
	staged, cleanup, err := stageAttachments(body)
	if err != nil {
		t.Fatal(err)
	}
	cleanup()
	if string(staged) != string(body) {
		t.Fatalf("staged = %s, want %s", staged, body)
	}
}

func TestPollerSkipsItsOwnImagePost(t *testing.T) {
	f := newFixture(t)
	f.watch(agent.TopLevel, testThread)
	res, err := f.app.post(f.ctx, f.db(), f.id, testChannel, testThread, "the map", nil, []string{writeFile(t, "a.png", pngBytes)})
	if err != nil {
		t.Fatal(err)
	}

	f.tick()
	if len(f.plane.events) != 0 {
		t.Fatalf("own post routed: %+v", f.plane.events)
	}
	if c := f.cursor(); c.oldest != res.TS {
		t.Fatalf("cursor = %q, want it past the own post %q", c.oldest, res.TS)
	}
}

func TestTextBlocksSplitLongTextAtLineBreaks(t *testing.T) {
	line := strings.Repeat("x", 1999) + "\n"
	blocks := textBlocks(line + line + "tail")
	if len(blocks) != 2 {
		t.Fatalf("blocks = %d, want 2", len(blocks))
	}
	texts := make([]string, 0, len(blocks))
	for _, b := range blocks {
		section, ok := b.(*slack.SectionBlock)
		if !ok {
			t.Fatalf("block = %T, want a section", b)
		}
		texts = append(texts, section.Text.Text)
	}
	if texts[0] != line || len([]rune(texts[1])) != 2004 {
		t.Fatalf("blocks = %d and %d runes, want the first line whole and the rest after it", len(texts[0]), len(texts[1]))
	}
	if err := checkAttachments(strings.Repeat("x", maxBlockChars*maxBlocks), nil, []string{writeFile(t, "a.png", pngBytes)}, false); err == nil {
		t.Fatal("checkAttachments accepted more than 50 blocks")
	}
}

func TestAbsPathsResolvesAgainstTheCallersDirectory(t *testing.T) {
	got := absPaths("/work", []string{"shot.png", "/tmp/map.gif"})
	if strings.Join(got, ",") != "/work/shot.png,/tmp/map.gif" {
		t.Fatalf("absPaths = %v", got)
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if got := absPaths(".", []string{"shot.png"}); got[0] != filepath.Join(wd, "shot.png") {
		t.Fatalf("absPaths with a relative cwd = %v", got)
	}
}

func TestUpdateMessageSendsTheTextWithItsLineBreaksAndClearsBlocks(t *testing.T) {
	var form url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		form = r.Form
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": testChannel, "ts": "1800000000.000100"})
	}))
	t.Cleanup(srv.Close)
	api := newSlackClient("xoxb-test", srv.URL+"/", srv.Client())

	text := "Fixed in <https://github.com/acme/app/pull/28352|#28352>.\nEdit: the missing piece was on our side."
	if err := updateMessage(context.Background(), api, testChannel, "1800000000.000100", text, nil); err != nil {
		t.Fatal(err)
	}
	if form.Get("text") != text || form.Get("blocks") != "[]" {
		t.Fatalf("chat.update text = %q, blocks = %q, want the text with its line break and blocks cleared", form.Get("text"), form.Get("blocks"))
	}
}

func TestUpdateMessageRendersTextPastTheUpdateLimitAsSections(t *testing.T) {
	var form url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		form = r.Form
		if len([]rune(form.Get("text"))) > 4000 {
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "msg_too_long"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": testChannel, "ts": "1800000000.000100"})
	}))
	t.Cleanup(srv.Close)
	api := newSlackClient("xoxb-test", srv.URL+"/", srv.Client())

	text := strings.Repeat("batch post line with a correction\n", 140)
	if err := updateMessage(context.Background(), api, testChannel, "1800000000.000100", text, nil); err != nil {
		t.Fatalf("edit of a %d-character text: %v", len(text), err)
	}
	var sections []struct {
		Type string `json:"type"`
		Text struct {
			Text string `json:"text"`
		} `json:"text"`
	}
	if err := json.Unmarshal([]byte(form.Get("blocks")), &sections); err != nil {
		t.Fatal(err)
	}
	var rendered strings.Builder
	for _, s := range sections {
		if s.Type != "section" {
			t.Fatalf("block type = %q, want section", s.Type)
		}
		rendered.WriteString(s.Text.Text)
	}
	if rendered.String() != text {
		t.Fatalf("sections render %d characters, want the whole %d-character text", rendered.Len(), len(text))
	}
}

func TestBlocksCombineWithImagesOnly(t *testing.T) {
	blocks, err := parseBlocks([]json.RawMessage{json.RawMessage(`{"type":"section","text":{"type":"mrkdwn","text":"the map"}}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err := checkAttachments("the map", blocks, []string{writeFile(t, "notes.txt", "plain notes")}, false); err == nil || !strings.Contains(err.Error(), "drop blocks") {
		t.Fatalf("blocks with a text file: %v, want a refusal", err)
	}
	png := writeFile(t, "a.png", pngBytes)
	if err := checkAttachments("the map", blocks, []string{png}, false); err != nil {
		t.Fatalf("blocks with an image: %v", err)
	}
	message := imageMessage("the map", blocks, []slack.FileSummary{{ID: "Fa.png", Title: "a.png"}})
	if len(message) != 2 || message[0].BlockType() != slack.MBTSection || message[1].BlockType() != slack.MBTImage {
		t.Fatalf("image message = %+v, want the given blocks then the image", message)
	}
}
