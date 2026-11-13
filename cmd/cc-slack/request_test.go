package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/slack-go/slack"

	"github.com/yasyf/cc-interact/daemon"
)

type heldMessage struct {
	Channel  string          `json:"-"`
	TS       string          `json:"ts"`
	ThreadTS string          `json:"thread_ts,omitempty"`
	Text     string          `json:"text"`
	Metadata json.RawMessage `json:"metadata,omitempty"`
}

type wireSlack struct {
	t        *testing.T
	srv      *httptest.Server
	mu       sync.Mutex
	clock    int64
	held     []heldMessage
	posts    int
	dropNext bool
}

func newWireSlack(t *testing.T) *wireSlack {
	w := &wireSlack{t: t, clock: time.Now().Unix()}
	w.srv = httptest.NewServer(http.HandlerFunc(w.serve))
	t.Cleanup(w.srv.Close)
	return w
}

func (w *wireSlack) api() slackAPI {
	return newSlackClient("xoxb-test", w.srv.URL+"/", w.srv.Client())
}

func (w *wireSlack) messages() []heldMessage {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.held)
}

func (w *wireSlack) postCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.posts
}

func (w *wireSlack) dropNextAnswer() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.dropNext = true
}

func (w *wireSlack) serve(rw http.ResponseWriter, r *http.Request) {
	switch path.Base(r.URL.Path) {
	case "chat.postMessage":
		w.post(rw, r)
	case methodHistory, methodReplies:
		w.read(rw, r)
	default:
		w.t.Errorf("Slack got an unexpected %s", r.URL.Path)
		http.NotFound(rw, r)
	}
}

func (w *wireSlack) post(rw http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		w.t.Error(err)
		return
	}
	var sent struct {
		Channel  string          `json:"channel"`
		ThreadTS string          `json:"thread_ts"`
		Text     string          `json:"text"`
		Metadata json.RawMessage `json:"metadata"`
	}
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		err = json.Unmarshal(body, &sent)
	} else {
		var form url.Values
		form, err = url.ParseQuery(string(body))
		sent.Channel, sent.ThreadTS, sent.Text = form.Get("channel"), form.Get("thread_ts"), form.Get("text")
		if raw := form.Get("metadata"); raw != "" {
			sent.Metadata = json.RawMessage(raw)
		}
	}
	if err != nil {
		w.t.Error(err)
		return
	}
	w.mu.Lock()
	w.clock++
	m := heldMessage{Channel: sent.Channel, TS: fmt.Sprintf("%d.000000", w.clock), ThreadTS: sent.ThreadTS, Text: sent.Text, Metadata: sent.Metadata}
	w.held, w.posts = append(w.held, m), w.posts+1
	drop := w.dropNext
	w.dropNext = false
	w.mu.Unlock()
	if drop {
		conn, _, err := http.NewResponseController(rw).Hijack()
		if err != nil {
			w.t.Error(err)
			return
		}
		_ = conn.Close()
		return
	}
	_ = json.NewEncoder(rw).Encode(map[string]any{"ok": true, "channel": sent.Channel, "ts": m.TS})
}

func (w *wireSlack) read(rw http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	thread := q.Get("ts")
	var found []heldMessage
	for _, m := range w.messages() {
		inThread := m.TS == thread || m.ThreadTS == thread
		if m.Channel != q.Get("channel") || !tsLess(q.Get("oldest"), m.TS) || thread != "" && !inThread || thread == "" && m.ThreadTS != "" {
			continue
		}
		if q.Get("include_all_metadata") != "true" {
			m.Metadata = nil
		}
		found = append(found, m)
	}
	at, _ := strconv.Atoi(q.Get("cursor"))
	next := ""
	if at+1 < len(found) {
		next = strconv.Itoa(at + 1)
	}
	page := found[min(at, len(found)):min(at+1, len(found))]
	_ = json.NewEncoder(rw).Encode(map[string]any{"ok": true, "messages": page, "response_metadata": map[string]string{"next_cursor": next}})
}

func stampOf(t *testing.T, m heldMessage) (string, map[string]any, bool) {
	t.Helper()
	var metadata struct {
		EventType string            `json:"event_type"`
		Payload   map[string]any    `json:"event_payload"`
		Entities  []json.RawMessage `json:"entities"`
	}
	if err := json.Unmarshal(m.Metadata, &metadata); err != nil {
		t.Fatalf("metadata of %s = %s: %v", m.TS, m.Metadata, err)
	}
	return metadata.EventType, metadata.Payload, len(metadata.Entities) > 0
}

func TestAResentRequestAnswersEachStepSlackHoldsAndPostsOnlyTheRest(t *testing.T) {
	wire := newWireSlack(t)
	api := wire.api()
	key := newRequestKey(time.Now())
	id := identity{UserID: testUser}
	card := slack.MsgOptionWorkObjectMetadata(slack.WorkObjectMetadata{Entities: []slack.WorkObjectEntity{{
		URL: "https://github.com/acme/app/pull/41", ExternalRef: slack.WorkObjectExternalRef{ID: "acme/app#41"},
		EntityType: slack.EntityTypeItem, EntityPayload: map[string]any{"attributes": map[string]any{"title": map[string]any{"text": "#41"}}},
	}}})
	post := func(resend bool, step, thread string, opts ...slack.MsgOption) string {
		t.Helper()
		ctx, err := withRequest(context.Background(), opBody{RequestKey: key, RequestResend: resend})
		if err != nil {
			t.Fatal(err)
		}
		opts = append(opts, slack.MsgOptionText(step, false))
		if thread != "" {
			opts = append(opts, slack.MsgOptionTS(thread))
		}
		_, ts, err := api.PostMessageContext(atStep(ctx, step), testChannel, opts...)
		if err != nil {
			t.Fatalf("post %s: %v", step, err)
		}
		return ts
	}

	root := post(false, "status", "", id.authorMetadata())
	page := post(false, "page:1", root)
	tap := post(false, "tap:acme/app#41:", "", card)
	held := wire.messages()
	for i, step := range []string{"status", "page:1", "tap:acme/app#41:"} {
		eventType, payload, carded := stampOf(t, held[i])
		if eventType != postEventType || payload[requestKeyField] != key || payload[requestStepField] != step {
			t.Fatalf("post %s carries %s, want event %s with request %s at step %s", step, held[i].Metadata, postEventType, key, step)
		}
		if carded != (step == "tap:acme/app#41:") {
			t.Fatalf("post %s carries %s, want its card entities kept beside the stamp", step, held[i].Metadata)
		}
	}
	if _, payload, _ := stampOf(t, held[0]); payload["user_id"] != testUser {
		t.Fatalf("root post carries %s, want its author kept beside the stamp", held[0].Metadata)
	}

	if again := post(true, "status", "", id.authorMetadata()); again != root {
		t.Fatalf("resent root post = %s, want the one Slack holds, %s", again, root)
	}
	if again := post(true, "page:1", root); again != page {
		t.Fatalf("resent thread reply = %s, want the one Slack holds, %s", again, page)
	}
	if again := post(true, "tap:acme/app#41:", "", card); again != tap {
		t.Fatalf("resent card = %s, want the one Slack holds, %s", again, tap)
	}
	if next := post(true, "page:2", root); slices.Contains([]string{root, page, tap}, next) {
		t.Fatalf("a step the cut call never reached answered %s, want a fresh post", next)
	}
	if n := wire.postCount(); n != 4 {
		t.Fatalf("chat.postMessage reached Slack %d times, want 4: three before the cut and the one step after it", n)
	}

	other, err := withRequest(context.Background(), opBody{RequestKey: newRequestKey(time.Now()), RequestResend: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, ts, err := api.PostMessageContext(atStep(other, "status"), testChannel, slack.MsgOptionText("status", false)); err != nil || ts == root {
		t.Fatalf("another request's resend = %s, %v; want its own post, never this request's", ts, err)
	}
	if _, _, err := api.PostMessageContext(context.Background(), testChannel, slack.MsgOptionText("plain", false)); err != nil {
		t.Fatal(err)
	}
	if plain := wire.messages()[5]; plain.Metadata != nil {
		t.Fatalf("a post outside any request carries %s, want it sent as written", plain.Metadata)
	}
}

func TestKeyedBodiesKeepOneKeyAcrossTheResend(t *testing.T) {
	t.Parallel()
	read := func(raw json.RawMessage) opBody {
		t.Helper()
		var b opBody
		if err := json.Unmarshal(raw, &b); err != nil {
			t.Fatal(err)
		}
		return b
	}
	first, again, err := keyedBodies(json.RawMessage(`{"channel_id":"C1","text":"deploy is out"}`), time.UnixMilli(1_800_000_000_123))
	if err != nil {
		t.Fatal(err)
	}
	sent, resent := read(first), read(again)
	if !strings.HasPrefix(sent.RequestKey, "1800000000123-") || sent.RequestResend || resent.RequestKey != sent.RequestKey || !resent.RequestResend || resent.Text != "deploy is out" {
		t.Fatalf("first = %+v, resend = %+v; want one key minted at the call, and only the resend marked", sent, resent)
	}
	handed, _, err := keyedBodies(again, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if b := read(handed); b.RequestKey != sent.RequestKey || !b.RequestResend {
		t.Fatalf("a body handed to a newer build = %+v, want its key and resend kept", b)
	}
	if _, err := withRequest(context.Background(), opBody{RequestKey: "not-a-key"}); err == nil {
		t.Fatal("a request key with no mint time was taken, want it refused")
	}
}

type postsOnTheWire struct {
	slackAPI
	wire slackAPI
}

func (p postsOnTheWire) PostMessageContext(ctx context.Context, channelID string, options ...slack.MsgOption) (string, string, error) {
	return p.wire.PostMessageContext(ctx, channelID, options...)
}

func TestASendWhoseAnswerWasLostIsResentAndPostsOnce(t *testing.T) {
	wire := newWireSlack(t)
	l := startDaemon(t, func(a *app) {
		fake := a.newAPI
		a.newAPI = func(token string) slackAPI { return postsOnTheWire{fake(token), wire.api()} }
	})
	loggedIn(t, l)
	body := l.hooked(opSend, map[string]any{"channel_id": testChannel, "text": "deploy is out"})
	body["request_key"] = newRequestKey(time.Now())

	wire.dropNextAnswer()
	l.refused(opSend, body, "can't post here")
	body["request_resend"] = true
	var res postResult
	if err := json.Unmarshal(l.do(opSend, body).Body, &res); err != nil {
		t.Fatal(err)
	}
	held := wire.messages()
	if len(held) != 1 || !res.Posted || res.Replayed || res.TS != held[0].TS || !res.Watched {
		t.Fatalf("resend = %+v with Slack holding %+v, want the one post the lost answer left, watched", res, held)
	}
	stored, found, err := loadReceipt(l.ctx, l.srv.DB(), field(body, "ticket_id"))
	if err != nil || !found || stored.TS != held[0].TS {
		t.Fatalf("receipt = %+v (%t, %v), want it to name the post", stored, found, err)
	}
	delete(body, "request_key")
	delete(body, "request_resend")
	var later postResult
	if err := json.Unmarshal(l.do(opSend, retried(body)).Body, &later); err != nil {
		t.Fatal(err)
	}
	if !later.Replayed || later.TS != held[0].TS || wire.postCount() != 1 {
		t.Fatalf("a later call under the same ticket = %+v, want the receipt replayed and no second post", later)
	}
}

func TestEveryPostOfAKeyedWriteNamesAStepNoOtherPostOfItNames(t *testing.T) {
	cmd := fakeCardCommand(t, []string{"acme/app#41"}, mergeRowCard())
	l, _ := cardingDaemon(t, cmd.argv, "acme/app#41")
	cards := openCardLinks(t, l, 7)
	text := strings.Join(slices.Concat([]string{"Tonight's cards:"}, cards[:3], []string{""}, cards[3:]), "\n")
	for _, write := range []struct {
		op   daemon.Op
		body map[string]any
		taps bool
	}{
		{opDMStatus, map[string]any{"text": text}, false},
		{opDMStatus, map[string]any{"text": waitsOn41}, true},
		{opSend, l.hooked(opSend, map[string]any{"channel_id": testChannel, "text": "deploy is out"}), false},
	} {
		from := len(l.slack.sent())
		l.do(write.op, write.body)
		if write.taps {
			awaitPRCards(t, l, testDM)
		}
		sent := l.slack.sent()[from:]
		if len(sent) < 2 && write.op == opDMStatus {
			t.Fatalf("%s %q posted %+v, want more than one post to tell apart", write.op, write.body["text"], sent)
		}
		steps := map[string]bool{}
		for _, p := range sent {
			if p.request == "" || p.step == "" || steps[p.step] {
				t.Fatalf("%s %q posted %+v, want every post under the call's request at a step of its own", write.op, write.body["text"], sent)
			}
			steps[p.step] = true
		}
	}
}
