package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
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

func projectWith(t *testing.T, enabled bool) func(context.Context, int) (string, error) {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, repoConfig), fmt.Appendf(nil, "[permissions]\nenabled = %t\n", enabled), 0o644); err != nil {
		t.Fatal(err)
	}
	return func(_ context.Context, pid int) (string, error) {
		if pid != testPID {
			return "", fmt.Errorf("claude %d is not the test session", pid)
		}
		return dir, nil
	}
}

func startDaemon(t *testing.T, setup ...func(*app)) *liveDaemon {
	t.Helper()
	shortHome(t)
	return launchDaemon(t, setup...)
}

func launchDaemon(t *testing.T, setup ...func(*app)) *liveDaemon {
	t.Helper()
	fs := newFakeSlack()
	a := newApp(func(string) slackAPI { return fs }, func(int) bool { return true }, openTestCache(t), openTestOwed(t), openTestDMThreads(t), openTestReplay(t))
	tickets := newFakeCaptHook()
	a.captHook = tickets.run
	a.claudeCwd = projectWith(t, true)
	a.avatar = agentAvatar
	a.shortcuts.avatar = agentAvatar
	user := newFakeUserCredential()
	a.user = user.session()
	for _, f := range setup {
		f(a)
	}
	srv, err := a.buildServer()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-served; err != nil {
			t.Logf("daemon serve: %v", err)
		}
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
	if _, err := l.srv.DB().ExecContext(l.ctx, `INSERT OR IGNORE INTO watches(id, subject_id, owner_agent_id, kind, channel_id, thread_ts, status, created_at)
		VALUES (7, ?, ?, ?, ?, ?, 'active', 0)`, l.subID, owner, kindThread, testChannel, testThread); err != nil {
		l.t.Fatal(err)
	}
	l.deliverOn(watch{ID: 7, SubjectID: l.subID, Owner: owner, Kind: kindThread, ChannelID: testChannel, ThreadTS: testThread}, ts, text)
}

func (l *liveDaemon) addWatch(thread string) watch {
	l.t.Helper()
	tx, err := l.srv.DB().BeginTx(l.ctx, nil)
	if err != nil {
		l.t.Fatal(err)
	}
	id, err := addWatch(l.ctx, tx, l.subID, agent.TopLevel, kindThread, testChannel, thread, time.Now())
	if err != nil {
		l.t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		l.t.Fatal(err)
	}
	return watch{ID: id, SubjectID: l.subID, Owner: agent.TopLevel, Kind: kindThread, ChannelID: testChannel, ThreadTS: thread}
}

func (l *liveDaemon) deliverOn(w watch, ts, text string) {
	l.t.Helper()
	id := identity{TeamURL: "https://acme.slack.com/", UserID: testUser, BotID: "BBOT", BotUserID: "UBOT"}
	m := slack.Message{Msg: slack.Msg{Timestamp: ts, ThreadTimestamp: w.ThreadTS, User: "UOTHER", Username: "teammate", Text: text}}
	d, err := l.app.poller.messageDelivery(l.ctx, l.slack, id, w, m, nil, "")
	if err != nil {
		l.t.Fatal(err)
	}
	if err := l.app.poller.deliver(l.ctx, l.srv.DB(), w, d); err != nil {
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

func TestTopLevelWatchWithNoChannelGetsTheDirectiveOnce(t *testing.T) {
	l := startDaemon(t)
	l.deliver(agent.TopLevel, "1800000000.000300", "ship it")

	if got := l.inject(agent.TopLevel); !strings.Contains(got, `"ship it"`) || !strings.Contains(got, "not an operator instruction") {
		t.Fatalf("top-level inject = %q, want the message as quoted Slack content", got)
	}
	if types := l.channelTypes(); slices.Contains(types, eventMessage) {
		t.Fatalf("channel saw %v, want no slack.message the hook already delivered", types)
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

func TestAPreCompactMarksTheWindowCompactingUntilItStartsAgain(t *testing.T) {
	l := startDaemon(t)
	held := []watch{{SubjectID: l.subID, Owner: agent.TopLevel}}
	l.do(opPreCompact, nil)
	if len(l.app.poller.compacting(held)) == 0 {
		t.Fatal("the window is not compacting after pre-compact")
	}
	l.do(opSessionStart, nil)
	if len(l.app.poller.compacting(held)) != 0 {
		t.Fatal("the window is still compacting after it started again")
	}
}

func TestADMDirectiveNamesTheThreadItsMessageIsIn(t *testing.T) {
	const root = "1800000000.000100"
	if got := messageLine(messagePayload{Kind: kindDM, ThreadTS: root, TS: root, Text: "hi", Operator: true}); strings.Contains(got, "thread_ts") {
		t.Fatalf("root message directive = %q, want no thread", got)
	}
	got := messageLine(messagePayload{Kind: kindDM, ThreadTS: root, TS: "1800000000.000200", Text: "hi", Operator: true})
	if !strings.Contains(got, "slack_dm_status and thread_ts "+root) {
		t.Fatalf("thread message directive = %q, want it to name thread %s", got, root)
	}
}

func TestThreadSeenSaysWhetherTheThreadsNewestMessageReachedTheAgent(t *testing.T) {
	l := startDaemon(t)
	l.deliver(agent.TopLevel, "1800000000.000300", "ship it")
	seen := func(newest, owner string) threadSeenResult {
		t.Helper()
		if _, err := l.srv.DB().ExecContext(l.ctx, `INSERT OR REPLACE INTO cursors(channel_id, thread_ts, oldest_ts, next_poll_at) VALUES (?, ?, ?, 0)`, testChannel, testThread, newest); err != nil {
			t.Fatal(err)
		}
		var res threadSeenResult
		if err := json.Unmarshal(l.do(opThreadSeen, map[string]string{"channel_id": testChannel, "thread_ts": testThread, "owner": owner}).Body, &res); err != nil {
			t.Fatal(err)
		}
		return res
	}
	if got := seen("1800000000.000300", agent.TopLevel); !got.Delivered || got.Newest != "1800000000.000300" {
		t.Fatalf("thread-seen = %+v, want the delivered newest message seen", got)
	}
	if got := seen("1800000000.000300", "agent-1"); got.Delivered {
		t.Fatalf("thread-seen for another agent = %+v, want it unseen", got)
	}
	if got := seen("1800000000.000400", agent.TopLevel); got.Delivered || got.Newest != "1800000000.000400" {
		t.Fatalf("thread-seen = %+v, want a newer message the session never got unseen", got)
	}
}

func (l *liveDaemon) channelTexts() []string {
	l.t.Helper()
	var texts []string
	for _, data := range l.channelPayloads() {
		var m messagePayload
		if err := json.Unmarshal([]byte(data), &m); err != nil {
			l.t.Fatal(err)
		}
		if m.Type == eventMessage {
			texts = append(texts, m.Text)
		}
	}
	return texts
}

func TestAResumedChannelDeliversWhatNoPathDeliveredAfterItsCursor(t *testing.T) {
	l := startDaemon(t)
	live, ended := l.addWatch("1800000000.000400"), l.addWatch("1800000000.000500")
	l.deliverOn(live, "1800000000.000401", "live, before the cursor")
	l.deliverOn(ended, "1800000000.000501", "ended, before the cursor")
	if got := l.channelTexts(); !slices.Equal(got, []string{"live, before the cursor", "ended, before the cursor"}) {
		t.Fatalf("first read = %q, want both watches' messages", got)
	}

	l.deliverOn(live, "1800000000.000402", "live, after the cursor")
	l.deliverOn(ended, "1800000000.000502", "ended, after the cursor")
	if _, err := l.srv.DB().ExecContext(l.ctx, `UPDATE watches SET status = 'stopped' WHERE id = ?`, ended.ID); err != nil {
		t.Fatal(err)
	}
	if got := l.channelTexts(); !slices.Equal(got, []string{"live, after the cursor", "ended, after the cursor"}) {
		t.Fatalf("resumed read = %q, want both messages after the cursor: no path delivered the ended watch's yet", got)
	}
}

func TestADMOnAnEndedWatchReachesTheSessionOnce(t *testing.T) {
	l := startDaemon(t)
	dmWatch := func(thread string) watch {
		t.Helper()
		tx, err := l.srv.DB().BeginTx(l.ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		id, err := addWatch(l.ctx, tx, l.subID, agent.TopLevel, kindDM, "D0OWNER", thread, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		return watch{ID: id, SubjectID: l.subID, Owner: agent.TopLevel, Kind: kindDM, ChannelID: "D0OWNER", ThreadTS: thread}
	}
	stop := func(w watch) {
		t.Helper()
		if _, err := l.srv.DB().ExecContext(l.ctx, `UPDATE watches SET status = 'stopped' WHERE id = ?`, w.ID); err != nil {
			t.Fatal(err)
		}
	}

	pushed := dmWatch("1800000000.000400")
	l.deliverOn(pushed, "1800000000.000401", "undelivered when its watch ended")
	stop(pushed)
	if got := l.channelTexts(); !slices.Equal(got, []string{"undelivered when its watch ended"}) {
		t.Fatalf("channel read = %q, want the message whose directive no path delivered yet", got)
	}

	hooked := dmWatch("1800000000.000500")
	l.deliverOn(hooked, "1800000000.000501", "delivered by the hook")
	stop(hooked)
	if got := l.inject(agent.TopLevel); !strings.Contains(got, "delivered by the hook") {
		t.Fatalf("hook drain = %q, want the DM", got)
	}
	if got := l.channelTexts(); len(got) != 0 {
		t.Fatalf("channel read = %q, want nothing the hook already delivered", got)
	}
}

func TestABotReplyThatRepeatsItsParentOnAThreadWatchArrivesAsItsFirstLine(t *testing.T) {
	l := startDaemon(t)
	id := identity{TeamURL: "https://acme.slack.com/", UserID: testUser, BotID: "BBOT", BotUserID: "UBOT"}
	w := watch{ID: 7, SubjectID: l.subID, Owner: agent.TopLevel, Kind: kindThread, ChannelID: testChannel, ThreadTS: testThread}
	body := "Triggered: Team runs failing far above their baseline\nTeam had 125 failed runs\n\nin the last 30 minutes"
	deliver := func(ts, thread, bot, text string) messagePayload {
		t.Helper()
		m := slack.Message{Msg: slack.Msg{Timestamp: ts, ThreadTimestamp: thread, User: "UDATADOG", BotID: bot, Username: "Datadog", Text: text}}
		d, err := l.app.poller.messageDelivery(l.ctx, l.slack, id, w, m, nil, "")
		if err != nil {
			t.Fatal(err)
		}
		if err := l.app.poller.deliver(l.ctx, l.srv.DB(), w, d); err != nil {
			t.Fatal(err)
		}
		var got messagePayload
		if err := json.Unmarshal(d.payload(eventMessage), &got); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(d.line, strconv.Quote(got.Text)) {
			t.Fatalf("directive %q, want the payload's text %q", d.line, got.Text)
		}
		return got
	}

	deliver(testThread, testThread, "BDATADOG", "*Monitor alert summary:* Team runs failing\n*Latest status:*\n"+strings.ReplaceAll(body, "\n\n", "\n"))

	if got := deliver("1800000000.000200", testThread, "BDATADOG", body); got.Text != "Triggered: Team runs failing far above their baseline "+repeatsParentMark || got.Permalink == "" {
		t.Fatalf("bot reply repeating its parent = %+v, want its first line and the mark, with its permalink", got)
	}
	if got := deliver("1800000000.000300", testThread, "", body); got.Text != body {
		t.Fatalf("human reply repeating the parent = %q, want it whole", got.Text)
	}
	recovered := "Recovered: Team runs failing far above their baseline\nTeam is back under its baseline"
	if got := deliver("1800000000.000400", testThread, "BDATADOG", recovered); got.Text != recovered {
		t.Fatalf("bot reply with words its parent lacks = %q, want it whole", got.Text)
	}
	if got := deliver("1800000001.000200", "1800000001.000100", "BDATADOG", body); got.Text != body {
		t.Fatalf("bot reply whose parent never reached the session = %q, want it whole", got.Text)
	}
}

func TestEveryBotMessageOnAChannelWatchArrivesAsOneLine(t *testing.T) {
	l := startDaemon(t)
	id := identity{TeamURL: "https://acme.slack.com/", UserID: testUser, BotID: "BBOT", BotUserID: "UBOT"}
	w := watch{ID: 7, SubjectID: l.subID, Owner: agent.TopLevel, Kind: kindChannel, ChannelID: testChannel}
	deliver := func(m slack.Message) messagePayload {
		t.Helper()
		d, err := l.app.poller.messageDelivery(l.ctx, l.slack, id, w, m, nil, "")
		if err != nil {
			t.Fatal(err)
		}
		var got messagePayload
		if err := json.Unmarshal(d.payload(eventMessage), &got); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(d.line, strconv.Quote(got.Text)) {
			t.Fatalf("directive %q, want the payload's text %q", d.line, got.Text)
		}
		return got
	}
	bot := func(ts, thread, text string, blocks ...slack.Block) slack.Message {
		return slack.Message{Msg: slack.Msg{Timestamp: ts, ThreadTimestamp: thread, User: "UDATADOG", BotID: "BDATADOG", Text: text, Blocks: slack.Blocks{BlockSet: blocks}}}
	}
	alert := "\n\n*Monitor alert summary:* Team runs failing — team_1\n*Latest status:* Triggered\nTeam had 125 failed runs"
	long := strings.Repeat("x", 200)

	tests := []struct {
		name string
		m    slack.Message
		want string
	}{
		{"a root's first non-empty line", bot(testThread, "", alert), "*Monitor alert summary:* Team runs failing — team_1 " + botLineMark},
		{"a thread reply's first line", bot("1800000000.000200", testThread, "Recovered: Team runs failing — team_1\nTeam is back under its baseline"), "Recovered: Team runs failing — team_1 " + botLineMark},
		{"a long line cut to 160 characters", bot("1800000000.000300", "", long), strings.Repeat("x", botLineLimit-1) + "… " + botLineMark},
		{"a short one-line message whole", bot("1800000000.000400", "", "Deploy finished"), "Deploy finished"},
		{"a message drawn only in blocks", bot("1800000000.000500", "", "", slack.NewSectionBlock(slack.NewTextBlockObject(slack.MarkdownType, "[api] Error: boom\nat main.go:12", false, false), nil, nil)), "[api] Error: boom " + botLineMark},
		{"a person's message whole", slack.Message{Msg: slack.Msg{Timestamp: "1800000000.000600", User: testUser, Text: alert}}, alert},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := deliver(tt.m); got.Text != tt.want || got.Permalink == "" {
				t.Fatalf("payload = %+v, want text %q and a permalink", got, tt.want)
			}
		})
	}
}
