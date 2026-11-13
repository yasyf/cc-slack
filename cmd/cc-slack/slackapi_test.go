package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/slack-go/slack"
)

func pausesInstantly(t *testing.T) *[]time.Duration {
	t.Helper()
	var waits []time.Duration
	prev := pause
	pause = func(_ context.Context, wait time.Duration) error {
		waits = append(waits, wait)
		return nil
	}
	t.Cleanup(func() { pause = prev })
	return &waits
}

func TestSlackCallsRetryWhatSlackRefusedAndNeverRepostWhatMayHaveLanded(t *testing.T) {
	update := func(api slackAPI) error {
		_, _, _, err := api.UpdateMessageContext(context.Background(), testChannel, testThread, slack.MsgOptionText("hi", false))
		return err
	}
	post := func(api slackAPI) error {
		_, _, err := api.PostMessageContext(context.Background(), testChannel, slack.MsgOptionText("hi", false))
		return err
	}
	sweep := func(api slackAPI) error {
		_, _, _, err := api.UpdateMessageContext(withoutRateWait(context.Background()), testChannel, testThread, slack.MsgOptionText("hi", false))
		return err
	}
	const dropped = 0
	backoffs := []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second}
	for _, tc := range []struct {
		name       string
		call       func(slackAPI) error
		answers    []int
		retryAfter string
		hits       int32
		waits      []time.Duration
		err        string
	}{
		{"chat.update answered 503 twice succeeds on the third try", update, []int{503, 503, 200}, "", 3, backoffs[:2], ""},
		{"chat.postMessage answered 503 is not retried, since the post may have landed", post, []int{503}, "", 1, nil, "slack server error: 503 Service Unavailable"},
		{"chat.postMessage answered 429 with Retry-After 0 is retried at once and succeeds", post, []int{429, 200}, "0", 2, []time.Duration{0}, ""},
		{"chat.postMessage answered 429 asking for more than 30s is not retried", post, []int{429}, "31", 1, nil, "slack rate limit exceeded, retry after 31s"},
		{"a card sweep's chat.update answered 429 hands the limit back at once instead of waiting it out", sweep, []int{429, 200}, "5", 1, nil, "slack rate limit exceeded, retry after 5s"},
		{"chat.update answered 404 is not retried", update, []int{404}, "", 1, nil, "slack server error: 404 Not Found"},
		{"chat.update answered 503 every time stops at 4 tries and says so", update, []int{503}, "", 4, backoffs, "slack server error: 503 Service Unavailable after 4 tries"},
		{"chat.update whose connection drops every time stops at 4 tries and says so", update, []int{dropped}, "", 4, backoffs, " after 4 tries"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			waits := pausesInstantly(t)
			var hits atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch status := tc.answers[min(int(hits.Add(1)), len(tc.answers))-1]; status {
				case dropped:
					conn, _, err := http.NewResponseController(w).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					_ = conn.Close()
				case http.StatusOK:
					_, _ = w.Write([]byte(`{"ok":true,"channel":"C1","ts":"1800000000.000100"}`))
				default:
					if tc.retryAfter != "" {
						w.Header().Set("Retry-After", tc.retryAfter)
					}
					w.WriteHeader(status)
				}
			}))
			t.Cleanup(srv.Close)
			err := tc.call(newSlackClient("xoxb-test", srv.URL+"/", srv.Client()))
			if tc.err == "" && err != nil || tc.err != "" && (err == nil || !strings.HasSuffix(err.Error(), tc.err)) {
				t.Fatalf("err = %v, want %q", err, tc.err)
			}
			if got := hits.Load(); got != tc.hits {
				t.Fatalf("hits = %d, want %d", got, tc.hits)
			}
			if !slices.Equal(*waits, tc.waits) {
				t.Fatalf("waits = %v, want %v", *waits, tc.waits)
			}
		})
	}
}

func TestRawMessagesFollowSlacksCursorFromTheOldestMessage(t *testing.T) {
	var queries []url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.Query())
		if r.URL.Query().Get("cursor") == "" {
			_, _ = w.Write([]byte(`{"ok":true,"messages":[{"ts":"1800000000.000100"}],"response_metadata":{"next_cursor":"page2"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"messages":[{"ts":"1800000000.000200"}]}`))
	}))
	defer srv.Close()
	raw, err := newSlackClient("xoxb-test", srv.URL+"/", srv.Client()).RawMessages(context.Background(), methodReplies, testChannel, testThread, "1800000000.000100")
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != 2 || raw["1800000000.000200"] == nil {
		t.Fatalf("raw = %v, want both pages' messages", raw)
	}
	if len(queries) != 2 || queries[0].Get("oldest") != "1800000000.000100" || queries[0].Get("inclusive") != "true" || queries[1].Get("cursor") != "page2" {
		t.Fatalf("queries = %v, want two pages from the oldest message on, the second at Slack's cursor", queries)
	}
}
