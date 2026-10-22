package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/slack-go/slack"

	"github.com/yasyf/cc-interact/agent"
	"github.com/yasyf/cc-interact/cmd"
	"github.com/yasyf/cc-interact/consume"
	"github.com/yasyf/cc-interact/daemon"
	"github.com/yasyf/cc-interact/event"
)

const sentinelType = "test.sentinel"

type liveDaemon struct {
	t       *testing.T
	ctx     context.Context
	srv     *daemon.Server
	app     *app
	slack   *fakeSlack
	tickets *fakeCaptHook
	user    *fakeUserCredential
	deps    cmd.Deps
	port    int
	subID   string
}

func shortHome(t *testing.T) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "ccs-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", resolved)
	t.Setenv("DAEMONKIT_HOME", resolved)
}

func projectWith(t *testing.T, hooks ...string) func(context.Context, int) (string, error) {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".claude", "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, hook := range hooks {
		if err := os.WriteFile(filepath.Join(dir, ".claude", "hooks", hook), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return func(_ context.Context, pid int) (string, error) {
		if pid != testPID {
			return "", fmt.Errorf("claude %d is not the test session", pid)
		}
		return dir, nil
	}
}

func startDaemon(t *testing.T) *liveDaemon {
	t.Helper()
	shortHome(t)
	return launchDaemon(t)
}

func launchDaemon(t *testing.T) *liveDaemon {
	t.Helper()
	fs := newFakeSlack()
	a := newApp(func(string) slackAPI { return fs }, func(int) bool { return true }, openTestCache(t), openTestReplay(t))
	tickets := newFakeCaptHook()
	a.captHook = tickets.run
	a.claudeCwd = projectWith(t, defaultPermissionHook)
	a.avatar = agentAvatar
	a.shortcuts.avatar = agentAvatar
	user := newFakeUserCredential()
	a.user = user.session()
	srv, err := a.buildServer()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-served
	})
	d := deps()
	d.EnsureCurrent = func(context.Context) error { return nil }
	d.ClaudePID = func() int { return testPID }
	l := &liveDaemon{t: t, ctx: context.Background(), srv: srv, app: a, slack: fs, tickets: tickets, user: user, deps: d}
	l.waitReady()
	reply := l.do(opSessionStart, nil)
	l.subID, l.port = reply.SubjectID, reply.HTTPPort
	return l
}

func (l *liveDaemon) waitReady() {
	l.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		client, err := newClient(l.ctx)
		if err == nil {
			reply, err := client.Do(l.ctx, daemon.Envelope{Op: daemon.OpStatus})
			_ = client.Close()
			if err == nil && reply.OK {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	l.t.Fatal("daemon never became ready")
}

func (l *liveDaemon) do(op daemon.Op, body any) daemon.Reply {
	l.t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		l.t.Fatal(err)
	}
	reply, err := invoke(l.ctx, l.deps, testSession, "/any/cwd", op, raw)
	if err != nil {
		l.t.Fatalf("%s: %v", op, err)
	}
	return reply
}

func (l *liveDaemon) inject(agentID string) string {
	l.t.Helper()
	var drained struct {
		Directives []agent.Directive `json:"directives"`
	}
	reply := l.do(daemon.OpAgentInject, map[string]string{"agent_id": agentID})
	if err := json.Unmarshal(reply.Body, &drained); err != nil {
		l.t.Fatal(err)
	}
	texts := make([]string, 0, len(drained.Directives))
	for _, d := range drained.Directives {
		texts = append(texts, d.Text)
	}
	return strings.Join(texts, "\n")
}

func (l *liveDaemon) startAgent(agentID string) {
	l.t.Helper()
	l.do(daemon.OpAgentStart, map[string]string{"agent_id": agentID, "agent_type": waiterAgentType, "session_id": testSession})
}

func (l *liveDaemon) deliver(owner, ts, text string) {
	l.t.Helper()
	id := identity{TeamURL: "https://acme.slack.com/", UserID: testUser, BotID: "BBOT", BotUserID: "UBOT"}
	w := watch{ID: 7, SubjectID: l.subID, Owner: owner, Kind: kindThread, ChannelID: testChannel, ThreadTS: testThread}
	m := slack.Message{Msg: slack.Msg{Timestamp: ts, ThreadTimestamp: testThread, User: "UOTHER", Username: "teammate", Text: text}}
	if err := l.app.poller.deliver(l.ctx, l.srv.DB(), w, l.app.poller.messageDelivery(l.ctx, l.slack, id, w, m)); err != nil {
		l.t.Fatal(err)
	}
}

func (l *liveDaemon) channelTypes() []string {
	l.t.Helper()
	if _, err := l.srv.Append(l.ctx, &event.Event{
		SubjectID: l.subID, Origin: event.OriginSystem, Type: sentinelType, Payload: json.RawMessage(`{"type":"` + sentinelType + `"}`),
	}); err != nil {
		l.t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(l.ctx, 10*time.Second)
	defer cancel()
	var types []string
	err := consume.ConsumeEvents(ctx, consume.StreamSource{
		Port: l.port, SubjectID: l.subID, Consumer: channelConsumer, ExcludeOrigin: event.OriginAgent, Paths: appPaths(),
	}, func(_ int64, data string) (bool, error) {
		var e struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(data), &e); err != nil {
			return false, err
		}
		types = append(types, e.Type)
		return e.Type == sentinelType, nil
	})
	if err != nil {
		l.t.Fatal(err)
	}
	return types
}

func TestTopLevelWatchGetsEventAndDirective(t *testing.T) {
	l := startDaemon(t)
	l.deliver(agent.TopLevel, "1800000000.000300", "ship it")

	if got := l.inject(agent.TopLevel); !strings.Contains(got, `"ship it"`) || !strings.Contains(got, "not an operator instruction") {
		t.Fatalf("top-level inject = %q, want the message as quoted Slack content", got)
	}
	if types := l.channelTypes(); !slices.Contains(types, eventMessage) {
		t.Fatalf("channel saw %v, want slack.message", types)
	}
}

func TestOnlyASlackWaiterIsGreeted(t *testing.T) {
	l := startDaemon(t)
	l.do(daemon.OpAgentStart, map[string]string{"agent_id": "agent-x", "agent_type": "general-purpose", "session_id": testSession})
	if got := l.inject("agent-x"); got != "" {
		t.Fatalf("general-purpose agent got directives %q, want none", got)
	}
}

func TestOwnedMessageMutedFromChannelWhileOwnerIsPresent(t *testing.T) {
	l := startDaemon(t)
	l.startAgent("agent-1")
	l.startAgent("agent-2")
	if got := l.inject("agent-1"); !strings.Contains(got, "You are agent agent-1; owner=agent-1") {
		t.Fatalf("agent-1 greeting = %q", got)
	}

	l.deliver("agent-1", "1800000000.000301", "for agent one")
	l.deliver("agent-2", "1800000000.000302", "for agent two")

	types := l.channelTypes()
	if slices.Contains(types, ownedType(eventMessage, "agent-1")) {
		t.Fatalf("channel saw %v: agent-1's message leaked while it was present", types)
	}
	if !slices.Contains(types, ownedType(eventMessage, "agent-2")) {
		t.Fatalf("channel saw %v, want agent-2's message (never drained, so not present)", types)
	}
	if got := l.inject("agent-1"); !strings.Contains(got, "for agent one") || strings.Contains(got, "for agent two") {
		t.Fatalf("agent-1 mailbox = %q, want only its own message", got)
	}
}

func TestAgentGreetingStaysUnder200Chars(t *testing.T) {
	if got := agentGreeting(agent.Info{AgentID: "aincident-doc-5d418ceb8e960f43"}); len(got) >= 200 {
		t.Fatalf("greeting is %d chars: %q", len(got), got)
	}
}

func TestSessionStartIsIdempotentPerWindow(t *testing.T) {
	l := startDaemon(t)
	if again := l.do(opSessionStart, nil); again.SubjectID != l.subID {
		t.Fatalf("second session-start subject %q, want %q", again.SubjectID, l.subID)
	}
}
