package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yasyf/cc-interact/channel"
)

func TestInstructionsArmTheWatchOnlyAfterASubscription(t *testing.T) {
	got := channelInstructions(testSession)
	if strings.Contains(got, "Immediately, before doing anything else") {
		t.Fatalf("instructions arm the watch at session start:\n%s", got)
	}
	for _, want := range []string{
		"Arm no Monitor at session start",
		"goes from no subscriptions to one, through a successful slack_watch without an owner, a slack_send or slack_reply without no_watch, or a slack_dm_status",
		"read the result's channel field. On active, channel tags carry the session: arm no Monitor",
		"cc-slack watch --until-proven --session " + testSession,
		"cc-slack channel-ack --session " + testSession,
		"On every channel.probe tag",
		"slack_unwatch reports remaining 0",
		"Shortcut hand-offs never arrive here",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("instructions lack %q:\n%s", want, got)
		}
	}
}

func TestAwaitHonorsTheCallersWindowAndDefaultsToOneMinute(t *testing.T) {
	shrinkAwaitReconnect(t, time.Minute)
	for _, tc := range []struct {
		in   string
		want int
	}{
		{`{"agent_id":"a1","timeout_seconds":3600}`, 3600},
		{`{"agent_id":"a1"}`, 60},
	} {
		var resumed struct {
			TimeoutSeconds int `json:"timeout_seconds"`
		}
		calls := 0
		tool := reconnectAwait(channel.Tool{
			Handler: func(_ context.Context, args json.RawMessage, _ func(string)) (string, bool) {
				calls++
				if calls == 1 {
					return "await: resolve subject: no daemon is listening", true
				}
				if err := json.Unmarshal(args, &resumed); err != nil {
					t.Fatal(err)
				}
				return "no directive", false
			},
		})
		if _, isErr := tool.Handler(context.Background(), json.RawMessage(tc.in), func(string) {}); isErr {
			t.Fatalf("await(%s) errored", tc.in)
		}
		if resumed.TimeoutSeconds > tc.want || resumed.TimeoutSeconds < tc.want-5 {
			t.Fatalf("await(%s) resumed with %ds, want the remainder of a %ds window", tc.in, resumed.TimeoutSeconds, tc.want)
		}
	}
	if awaitTimeout != time.Minute {
		t.Fatalf("default await window = %s, want one minute", awaitTimeout)
	}
}

func shrinkAwaitReconnect(t *testing.T, window time.Duration) {
	t.Helper()
	prevWindow, prevBackoff := awaitReconnectWindow, awaitBackoff
	awaitReconnectWindow, awaitBackoff = window, 10*time.Millisecond
	t.Cleanup(func() { awaitReconnectWindow, awaitBackoff = prevWindow, prevBackoff })
}

func TestAwaitResumesTheSubscriptionAcrossADaemonRestart(t *testing.T) {
	shrinkAwaitReconnect(t, time.Minute)
	var polls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("subject") != "subj" || r.URL.Query().Get("agent") != "a1" {
			t.Errorf("poll %s, want subject subj and agent a1", r.URL.RawQuery)
		}
		if polls.Add(1) == 1 {
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				t.Error("the test server's writer cannot hijack")
				return
			}
			conn, _, err := hijacker.Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = conn.Close()
			return
		}
		_, _ = w.Write([]byte(`{"directives":[{"ID":7,"Origin":"system","Text":"hello"}]}`))
	}))
	defer srv.Close()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}
	var resolves atomic.Int32
	tool := reconnectAwait(channel.NewAwaitTool(channel.AwaitSpec{
		Resolve: func(context.Context) (string, int, error) {
			if resolves.Add(1) == 1 {
				return "", 0, errors.New("no daemon is listening")
			}
			return "subj", port, nil
		},
		Timeout: time.Minute,
	}))
	var progress []string
	out, isErr := tool.Handler(context.Background(), json.RawMessage(`{"agent_id":"a1","timeout_seconds":30}`), func(p string) { progress = append(progress, p) })
	if isErr || !strings.Contains(out, "[system #7] hello") {
		t.Fatalf("await = %q, %v, want the directive delivered after the restart", out, isErr)
	}
	if got := polls.Load(); got != 2 {
		t.Fatalf("polls = %d, want the dropped poll and its resumption", got)
	}
	if len(progress) != 2 || progress[0] != awaitReconnecting || progress[1] != awaitReconnecting {
		t.Fatalf("progress = %q, want one reconnect notice per failure", progress)
	}
}

func TestAwaitErrorsOnceTheDaemonStaysDownPastTheReconnectWindow(t *testing.T) {
	shrinkAwaitReconnect(t, 50*time.Millisecond)
	var resolves atomic.Int32
	tool := reconnectAwait(channel.NewAwaitTool(channel.AwaitSpec{
		Resolve: func(context.Context) (string, int, error) {
			resolves.Add(1)
			return "", 0, errors.New("no daemon is listening")
		},
		Timeout: time.Minute,
	}))
	out, isErr := tool.Handler(context.Background(), json.RawMessage(`{"agent_id":"a1"}`), func(string) {})
	if !isErr || !strings.Contains(out, "no daemon is listening") {
		t.Fatalf("await = %q, %v, want the daemon error", out, isErr)
	}
	if resolves.Load() < 2 {
		t.Fatalf("resolves = %d, want retries before giving up", resolves.Load())
	}
}

func TestAwaitPassesAnArgumentErrorThroughWithoutRetrying(t *testing.T) {
	shrinkAwaitReconnect(t, time.Minute)
	calls := 0
	tool := reconnectAwait(channel.Tool{
		Handler: func(context.Context, json.RawMessage, func(string)) (string, bool) {
			calls++
			return "await: agent_id is required", true
		},
	})
	if out, isErr := tool.Handler(context.Background(), json.RawMessage(`{}`), func(string) {}); !isErr || calls != 1 {
		t.Fatalf("await = %q, %v after %d calls, want one failed call", out, isErr, calls)
	}
}
