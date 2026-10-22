package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/yasyf/cc-interact/agent"
	"github.com/yasyf/cc-interact/daemon"
)

func TestWatchHandlerPrintsOnlySlackEvents(t *testing.T) {
	for _, tc := range []struct {
		name      string
		seq       int64
		typ       string
		lifecycle bool
		printed   bool
	}{
		{"slack message passes", 11, eventMessage, false, true},
		{"watch error passes", 11, eventWatchError, false, true},
		{"relay passes", 11, agent.EventRelay, false, true},
		{"lifecycle event is filtered", 11, agent.EventStarted, false, false},
		{"owned message is filtered", 11, ownedType(eventMessage, "agent-1"), false, false},
		{"history is filtered", 10, eventMessage, false, false},
		{"lifecycle flag prints lifecycle events", 11, agent.EventStarted, true, true},
		{"lifecycle flag still skips history", 10, agent.EventStarted, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			data := `{"type":"` + tc.typ + `"}`
			handle := watchHandler(&out, 10, tc.lifecycle, false, func(string) bool { return false })
			if _, err := handle(tc.seq, data); err != nil {
				t.Fatal(err)
			}
			want := ""
			if tc.printed {
				want = data + "\n"
			}
			if got := out.String(); got != want {
				t.Fatalf("printed %q, want %q", got, want)
			}
		})
	}
}

func TestWatchStartsAtTheLogHead(t *testing.T) {
	l := startDaemon(t)
	l.startAgent("agent-before")
	l.deliver(agent.TopLevel, "1800000000.000100", "before the watch")
	l.deps.WindowAlive = func(int) bool { return true }

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var out bytes.Buffer
	c := watchCmd(l.deps)
	c.SetArgs([]string{"--session", testSession, "--cwd", "/any/cwd", "--once"})
	c.SetOut(&out)
	done := make(chan error, 1)
	go func() { done <- c.ExecuteContext(ctx) }()

	for i := 0; ; i++ {
		l.startAgent(fmt.Sprintf("agent-%d", i))
		l.deliver(agent.TopLevel, fmt.Sprintf("1800000001.%06d", i), "after the watch")
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimSpace(out.String()), "\n")
			var m messagePayload
			if err := json.Unmarshal([]byte(lines[0]), &m); err != nil {
				t.Fatal(err)
			}
			if len(lines) != 1 || m.Type != eventMessage || m.Text != "after the watch" {
				t.Fatalf("watch printed %q, want one slack.message sent after it started", out.String())
			}
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func TestWatchUntilProvenExitsOnChannelAck(t *testing.T) {
	l := startDaemon(t)
	l.deps.WindowAlive = func(int) bool { return true }

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var out bytes.Buffer
	c := watchCmd(l.deps)
	c.SetArgs([]string{"--session", testSession, "--cwd", "/any/cwd", "--until-proven"})
	c.SetOut(&out)
	done := make(chan error, 1)
	go func() { done <- c.ExecuteContext(ctx) }()

	select {
	case err := <-done:
		t.Fatalf("watch exited before channel-ack: %v, printed %q", err, out.String())
	case <-time.After(3 * provenPoll):
	}
	l.do(daemon.OpChannelAck, nil)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("watch --until-proven never exited after channel-ack")
	}
	if got := strings.TrimSpace(out.String()); got != provenEvent {
		t.Fatalf("watch printed %q, want %s", got, provenEvent)
	}
}

func TestChannelEventAdmitsOnlyWhatTheSessionActsOn(t *testing.T) {
	for typ, want := range map[string]bool{
		eventMessage:                          true,
		eventAction:                           true,
		eventWatchError:                       true,
		agent.EventRelay:                      true,
		daemon.ProbeEventType:                 true,
		agent.EventStarted:                    false,
		agent.EventDirected:                   false,
		agent.EventDelivered:                  false,
		agent.EventStopped:                    false,
		ownedType(eventMessage, "agent-1"):    false,
		ownedType(eventWatchError, "agent-1"): false,
	} {
		if got := channelEvent(typ); got != want {
			t.Errorf("channelEvent(%q) = %v, want %v", typ, got, want)
		}
	}
}
