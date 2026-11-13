package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	requestKeyField  = "request_key"
	requestStepField = "request_step"
	requestClockSkew = time.Minute
)

type request struct {
	key    string
	resend bool
	minted time.Time
}

type (
	requestCtxKey struct{}
	stepCtxKey    struct{}
)

func newRequestKey(now time.Time) string {
	return strconv.FormatInt(now.UnixMilli(), 10) + "-" + rand.Text()
}

func keyedBodies(body json.RawMessage, now time.Time) (json.RawMessage, json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, nil, err
	}
	_, keyed := fields[requestKeyField]
	if !keyed {
		fields[requestKeyField] = mustJSON(newRequestKey(now))
	}
	first, err := json.Marshal(fields)
	if err != nil {
		return nil, nil, err
	}
	fields["request_resend"] = json.RawMessage("true")
	again, err := json.Marshal(fields)
	if keyed {
		return again, again, err
	}
	return first, again, err
}

type requestTurns struct {
	mu    sync.Mutex
	turns map[string]*requestTurn
}

type requestTurn struct {
	mu      sync.Mutex
	callers int
}

func (t *requestTurns) take(key string) func() {
	t.mu.Lock()
	if t.turns == nil {
		t.turns = map[string]*requestTurn{}
	}
	turn := t.turns[key]
	if turn == nil {
		turn = &requestTurn{}
		t.turns[key] = turn
	}
	turn.callers++
	t.mu.Unlock()
	turn.mu.Lock()
	return func() {
		turn.mu.Unlock()
		t.mu.Lock()
		defer t.mu.Unlock()
		turn.callers--
		if turn.callers == 0 {
			delete(t.turns, key)
		}
	}
}

func withRequest(ctx context.Context, b opBody) (context.Context, error) {
	if b.RequestKey == "" {
		return ctx, nil
	}
	ms, _, found := strings.Cut(b.RequestKey, "-")
	minted, err := strconv.ParseInt(ms, 10, 64)
	if !found || err != nil {
		return ctx, fmt.Errorf("request_key %q does not open with the unix milliseconds it was minted at", b.RequestKey)
	}
	return context.WithValue(ctx, requestCtxKey{}, request{key: b.RequestKey, resend: b.RequestResend, minted: time.UnixMilli(minted)}), nil
}

func resent(ctx context.Context) bool {
	r, ok := ctx.Value(requestCtxKey{}).(request)
	return ok && r.resend
}

func atStep(ctx context.Context, step string) context.Context {
	return context.WithValue(ctx, stepCtxKey{}, step)
}

func substep(ctx context.Context, step string) context.Context {
	parent, ok := ctx.Value(stepCtxKey{}).(string)
	if !ok {
		return ctx
	}
	return atStep(ctx, parent+"/"+step)
}

type requestPoster struct {
	token    string
	endpoint string
	next     doer
}

func (p requestPoster) Do(req *http.Request) (*http.Response, error) {
	r, keyed := req.Context().Value(requestCtxKey{}).(request)
	step, stepped := req.Context().Value(stepCtxKey{}).(string)
	if !keyed || !stepped || path.Base(req.URL.Path) != "chat.postMessage" {
		return p.next.Do(req) //nolint:gosec // G704: the caller built the request for Slack or the doc-cards URL the config names
	}
	form, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	values, err := url.ParseQuery(string(form))
	if err != nil {
		return nil, err
	}
	if r.resend {
		held, err := p.held(req, values, r, step)
		if err != nil || held != nil {
			return held, err
		}
	}
	metadata, err := stampMetadata(values.Get("metadata"), r.key, step)
	if err != nil {
		return nil, err
	}
	values.Set("metadata", metadata)
	stamped, err := http.NewRequestWithContext(req.Context(), req.Method, req.URL.String(), strings.NewReader(values.Encode())) //nolint:gosec // G704: the request goes to the Slack API endpoint
	if err != nil {
		return nil, err
	}
	stamped.Header = req.Header.Clone()
	return p.next.Do(stamped) //nolint:gosec // G704: the request goes to the Slack API endpoint
}

func stampMetadata(raw, key, step string) (string, error) {
	metadata := map[string]any{}
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &metadata); err != nil {
			return "", fmt.Errorf("read the metadata to stamp request %s on: %w", key, err)
		}
	}
	payload, _ := metadata["event_payload"].(map[string]any)
	if payload == nil {
		payload = map[string]any{}
	}
	payload[requestKeyField], payload[requestStepField] = key, step
	metadata["event_payload"] = payload
	if _, typed := metadata["event_type"]; !typed {
		metadata["event_type"] = postEventType
	}
	out, err := json.Marshal(metadata)
	return string(out), err
}

func (p requestPoster) held(req *http.Request, form url.Values, r request, step string) (*http.Response, error) {
	channel := form.Get("channel")
	method := methodHistory
	query := url.Values{
		"channel": {channel}, "include_all_metadata": {"true"}, "limit": {strconv.Itoa(pageLimit)},
		"oldest": {strconv.FormatInt(r.minted.Add(-requestClockSkew).Unix(), 10) + ".000000"},
	}
	if thread := form.Get("thread_ts"); thread != "" {
		method = methodReplies
		query.Set("ts", thread)
	}
	for {
		messages, next, err := slackMessages(req.Context(), p.next, p.token, p.endpoint+method, query)
		if err != nil {
			return nil, fmt.Errorf("read whether Slack already holds step %s of resent request %s: %w", step, r.key, err)
		}
		for _, raw := range messages {
			var m struct {
				TS       string `json:"ts"`
				Metadata struct {
					Payload map[string]any `json:"event_payload"`
				} `json:"metadata"`
			}
			if err := json.Unmarshal(raw, &m); err != nil {
				return nil, err
			}
			if m.Metadata.Payload[requestKeyField] != r.key || m.Metadata.Payload[requestStepField] != step {
				continue
			}
			log.Printf("request %s step %s: Slack already holds it at %s/%s, so it is not posted again", r.key, step, channel, m.TS) //nolint:gosec // G706: the line carries ids cc-slack minted and Slack's own ts, for the daemon log only
			answer, err := json.Marshal(map[string]any{"ok": true, "channel": channel, "ts": m.TS, "message": raw})
			if err != nil {
				return nil, err
			}
			return &http.Response{
				StatusCode: http.StatusOK, Status: "200 OK", Header: http.Header{"Content-Type": {"application/json"}},
				Body: io.NopCloser(bytes.NewReader(answer)), Request: req,
			}, nil
		}
		if next == "" {
			return nil, nil
		}
		query.Set("cursor", next)
	}
}
