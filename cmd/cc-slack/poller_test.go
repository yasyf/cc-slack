package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/slack-go/slack"

	"github.com/yasyf/cc-interact/agent"
	"github.com/yasyf/cc-interact/event"
)

func TestPollerPollsOnlyTheDMWithoutWatches(t *testing.T) {
	f := newFixture(t)
	if next := f.tick(); !next.IsZero() {
		t.Fatalf("next poll = %v, want none until a nudge", next)
	}
	if r, h := f.slack.callCount(methodReplies), f.slack.callCount(methodHistory); r != 0 || h != 1 {
		t.Fatalf("slack calls = %d replies, %d history; want one DM history poll", r, h)
	}
	f.now = f.now.Add(time.Hour)
	f.tick()
	if h := f.slack.callCount(methodHistory); h != 1 {
		t.Fatalf("history calls an hour later with no nudge = %d, want 1", h)
	}
}

func TestPollerPollsAWatchedThreadOnlyWhenNudged(t *testing.T) {
	f := newFixture(t)
	f.watch(agent.TopLevel, testThread)
	f.tick()
	calls := f.slack.callCount(methodReplies)
	f.slack.add(testChannel, reply("1800000000.000300", "looks good"))
	f.now = f.now.Add(time.Hour)
	f.tick()
	if n := f.slack.callCount(methodReplies); n != calls || len(f.plane.events) != 0 {
		t.Fatalf("replies calls = %d (was %d), events = %d: the thread polled with no nudge", n, calls, len(f.plane.events))
	}

	f.push(mustJSON(map[string]any{"event_type": nudgeEventType, "event_payload": map[string]string{
		"channel": testChannel, "ts": "1800000000.000300", "thread_ts": testThread,
	}}))
	f.tick()
	if n := f.slack.callCount(methodReplies); n != calls+1 || len(f.plane.events) != 1 {
		t.Fatalf("replies calls = %d, events = %d after the nudge, want one poll delivering the reply", n, len(f.plane.events))
	}
}

func TestPollerIgnoresNudgesNoWatchOrRouteNeeds(t *testing.T) {
	f := newFixture(t)
	f.watch(agent.TopLevel, testThread)
	f.tick()
	calls := f.slack.callCount(methodReplies)
	history := f.slack.callCount(methodHistory)
	for _, n := range []messageNudge{
		{Channel: testChannel, TS: "1800000000.000300", ThreadTS: "1800000000.000900"},
		{Channel: "C0OTHER0001", TS: "1800000000.000300", ThreadTS: testThread},
		{Channel: testChannel, TS: "1800000000.000300", ThreadTS: testThread, Subtype: "message_changed"},
		{Channel: testChannel, TS: "1800000000.000300"},
	} {
		f.poller.nudged(n)
	}
	f.tick()
	if r, h := f.slack.callCount(methodReplies), f.slack.callCount(methodHistory); r != calls || h != history {
		t.Fatalf("slack calls = %d replies, %d history; want none for nudges no watch needs", r-calls, h-history)
	}
}

func TestPollerReconcilesEveryWatchOnReconnect(t *testing.T) {
	f := newFixture(t)
	f.watch(agent.TopLevel, testThread)
	f.tick()
	calls, history := f.slack.callCount(methodReplies), f.slack.callCount(methodHistory)
	f.slack.add(testChannel, reply("1800000000.000300", "sent while the socket was down"))

	f.poller.reconcileAll()
	f.tick()
	if r, h := f.slack.callCount(methodReplies), f.slack.callCount(methodHistory); r != calls+1 || h != history+1 {
		t.Fatalf("slack calls = %d replies, %d history after reconnect, want one poll of the thread and the DM", r-calls, h-history)
	}
	if len(f.plane.events) != 1 {
		t.Fatalf("events = %d, want the reply missed while disconnected", len(f.plane.events))
	}
}

func TestPollerDeliversEachReplyOnceAndAdvancesCursor(t *testing.T) {
	f := newFixture(t)
	f.slack.add(testChannel, slack.Message{Msg: slack.Msg{Timestamp: testThread, User: testUser, Text: "parent"}})
	f.slack.add(testChannel, reply("1800000000.000200", "before the watch"))
	f.watch(agent.TopLevel, "1800000000.000200")
	f.slack.add(testChannel, reply("1800000000.000300", "looks good"))

	f.tick()
	if len(f.plane.events) != 1 {
		t.Fatalf("events = %+v, want one slack.message", f.plane.events)
	}
	var got messagePayload
	if err := json.Unmarshal(f.plane.events[0].Payload, &got); err != nil {
		t.Fatal(err)
	}
	if got.Type != eventMessage || got.TS != "1800000000.000300" || got.UserName != "teammate" || got.FromClaude || got.Text != "looks good" {
		t.Fatalf("payload = %+v", got)
	}
	if want := "https://acme.slack.com/archives/C1/p1800000000000300?thread_ts=1800000000.000100&cid=C1"; got.Permalink != want {
		t.Fatalf("permalink = %q, want %q", got.Permalink, want)
	}
	if len(f.plane.directives) != 1 || f.plane.directives[0].agentID != agent.TopLevel || !strings.Contains(f.plane.directives[0].text, "looks good") {
		t.Fatalf("directives = %+v, want one top-level directive", f.plane.directives)
	}
	if c := f.cursor(); c.oldest != "1800000000.000300" {
		t.Fatalf("cursor = %q, want the delivered ts", c.oldest)
	}

	f.now = f.now.Add(time.Minute)
	f.nudgeThread()
	f.tick()
	if len(f.plane.events) != 1 || len(f.plane.directives) != 1 {
		t.Fatalf("second tick redelivered: %d events, %d directives", len(f.plane.events), len(f.plane.directives))
	}
}

func TestPollerAdvancesCursorOnlyAfterAppend(t *testing.T) {
	f := newFixture(t)
	f.watch(agent.TopLevel, testThread)
	f.slack.add(testChannel, reply("1800000000.000300", "hello"))

	f.plane.appendErr = errors.New("disk full")
	if _, err := f.poller.tick(f.ctx); err == nil {
		t.Fatal("tick succeeded with a failing append")
	}
	if c := f.cursor(); c.oldest != testThread {
		t.Fatalf("cursor advanced to %q past an undelivered message", c.oldest)
	}

	f.plane.appendErr = nil
	f.tick()
	if len(f.plane.events) != 1 {
		t.Fatalf("events after recovery = %d, want 1", len(f.plane.events))
	}
	if c := f.cursor(); c.oldest != "1800000000.000300" {
		t.Fatalf("cursor = %q after delivery", c.oldest)
	}
}

func TestPollerRecordsDeliveryBeforeDirecting(t *testing.T) {
	f := newFixture(t)
	f.watch(agent.TopLevel, testThread)
	f.slack.add(testChannel, reply("1800000000.000300", "hello"))

	f.plane.directErr = errors.New("daemon killed")
	if _, err := f.poller.tick(f.ctx); err == nil {
		t.Fatal("tick succeeded with a failing direct")
	}
	if len(f.plane.events) != 1 {
		t.Fatalf("events = %d, want the delivery recorded before the direct", len(f.plane.events))
	}

	f.plane.directErr = nil
	f.tick()
	if len(f.plane.events) != 1 || len(f.plane.directives) != 0 {
		t.Fatalf("retry redelivered: %d events, %d directives", len(f.plane.events), len(f.plane.directives))
	}
}

func TestPollerPausesMethodOnRateLimit(t *testing.T) {
	f := newFixture(t)
	f.watch(agent.TopLevel, testThread)
	f.slack.add(testChannel, reply("1800000000.000300", "hello"))
	f.slack.fail(methodReplies, &slack.RateLimitedError{RetryAfter: 30 * time.Second})

	next := f.tick()
	if want := f.now.Add(30 * time.Second); next.Before(want) {
		t.Fatalf("next poll %v, want at or after Retry-After %v", next, want)
	}
	if c := f.cursor(); c.backoffS != 10 {
		t.Fatalf("backoff = %ds, want 10", c.backoffS)
	}

	f.now = f.now.Add(15 * time.Second)
	f.tick()
	if n := f.slack.callCount(methodReplies); n != 1 {
		t.Fatalf("replies calls during the pause = %d, want 1", n)
	}

	f.now = f.now.Add(20 * time.Second)
	f.tick()
	if n := f.slack.callCount(methodReplies); n != 2 {
		t.Fatalf("replies calls after the pause = %d, want 2", n)
	}
	if len(f.plane.events) != 1 {
		t.Fatalf("events = %d after the pause lifted, want 1", len(f.plane.events))
	}
	if c := f.cursor(); c.backoffS != 0 {
		t.Fatalf("backoff = %ds after a success, want it cleared", c.backoffS)
	}
}

func TestPollerOrphansWatchesOfDeadWindows(t *testing.T) {
	f := newFixture(t)
	id := f.watch(agent.TopLevel, testThread)
	f.alive[testPID] = false

	if next := f.tick(); !next.IsZero() {
		t.Fatalf("next poll = %v, want none once orphaned", next)
	}
	if got := f.watchStatus(id); got != "orphaned" {
		t.Fatalf("status = %q, want orphaned", got)
	}
	if n := f.slack.callCount(methodReplies); n != 0 {
		t.Fatalf("replies calls = %d for an orphaned watch", n)
	}
}

func TestPollerSkipsItsOwnPosts(t *testing.T) {
	f := newFixture(t)
	f.watch(agent.TopLevel, testThread)
	res, err := f.app.post(f.ctx, f.db(), f.id, testChannel, testThread, "on it", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := f.slack.sent()[0]; got.username != "yasyf (agent)" || got.iconURL != "https://avatars/ym.png?agent-badge" || got.threadTS != testThread {
		t.Fatalf("post = %+v, want yasyf with the agent avatar in the thread", got)
	}

	f.tick()
	if len(f.plane.events) != 0 {
		t.Fatalf("own post routed: %+v", f.plane.events)
	}
	if c := f.cursor(); c.oldest != res.TS {
		t.Fatalf("cursor = %q, want it past the own post %q", c.oldest, res.TS)
	}
}

func TestPostSucceedsWhenRecordingItFails(t *testing.T) {
	f := newFixture(t)
	if _, err := f.db().ExecContext(f.ctx, `CREATE TRIGGER posts_locked BEFORE INSERT ON posts BEGIN SELECT RAISE(ABORT, 'database is locked'); END`); err != nil {
		t.Fatal(err)
	}
	res, err := f.app.post(f.ctx, f.db(), f.id, testChannel, testThread, "on it", nil, nil)
	if err != nil {
		t.Fatalf("post failed after Slack accepted it: %v", err)
	}
	if sent := f.slack.sent(); len(sent) != 1 || sent[0].ts != res.TS {
		t.Fatalf("sent = %+v, want one post with ts %q", sent, res.TS)
	}
}

func TestPollerFlagsAnotherSessionsPostAsFromClaude(t *testing.T) {
	f := newFixture(t)
	f.watch(agent.TopLevel, testThread)
	f.slack.add(testChannel, slack.Message{Msg: slack.Msg{
		Timestamp: "1800000000.000300", ThreadTimestamp: testThread, User: "UBOT", BotID: "BBOT", Username: "alice", Text: "done",
	}})

	f.tick()
	var got messagePayload
	if err := json.Unmarshal(f.plane.events[0].Payload, &got); err != nil {
		t.Fatal(err)
	}
	if !got.FromClaude || got.UserName != "alice" || f.plane.events[0].Origin != event.OriginSystem {
		t.Fatalf("payload = %+v origin %q, want from_claude by alice", got, f.plane.events[0].Origin)
	}
}

func TestPollerErrorsWatchOnFatalSlackError(t *testing.T) {
	f := newFixture(t)
	id := f.watch(agent.TopLevel, testThread)
	f.slack.fail(methodReplies, slack.SlackErrorResponse{Err: "not_in_channel"})

	f.tick()
	if got := f.watchStatus(id); got != "error" {
		t.Fatalf("status = %q, want error", got)
	}
	if len(f.plane.events) != 1 || f.plane.events[0].Type != eventWatchError {
		t.Fatalf("events = %+v, want one slack.watch_error", f.plane.events)
	}
	var got watchErrorPayload
	if err := json.Unmarshal(f.plane.events[0].Payload, &got); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.Error, "not_in_channel") || !strings.Contains(got.Error, mcpThread) {
		t.Fatalf("error = %q, want the code and the MCP fallback", got.Error)
	}
	f.nudgeThread()
	if n := f.tick(); !n.IsZero() {
		t.Fatalf("next poll = %v, want none once the watch errored", n)
	}
}

func TestPollerRoutesOwnedWatchToItsSubagent(t *testing.T) {
	f := newFixture(t)
	registerAgent(t, f, "agent-1")
	f.watch("agent-1", testThread)
	f.slack.add(testChannel, reply("1800000000.000300", "for the waiter"))

	f.tick()
	if len(f.plane.events) != 1 || f.plane.events[0].Type != ownedType(eventMessage, "agent-1") {
		t.Fatalf("events = %+v, want one slack.message@agent-1", f.plane.events)
	}
	var got messagePayload
	if err := json.Unmarshal(f.plane.events[0].Payload, &got); err != nil {
		t.Fatal(err)
	}
	if got.Type != "slack.message@agent-1" {
		t.Fatalf("payload type = %q, want the owned type", got.Type)
	}
	if len(f.plane.directives) != 0 {
		t.Fatalf("directives = %+v: a running owner receives through the tee, not a direct", f.plane.directives)
	}
}

func TestPollerRelaysToAFinishedOwner(t *testing.T) {
	f := newFixture(t)
	registerAgent(t, f, "agent-1")
	f.watch("agent-1", testThread)
	if _, err := f.plane.st.CloseAgent(f.ctx, testSubject, "agent-1", f.now); err != nil {
		t.Fatal(err)
	}
	f.slack.add(testChannel, reply("1800000000.000300", "late reply"))

	f.tick()
	if len(f.plane.directives) != 2 || f.plane.directives[0].agentID != "agent-1" || f.plane.directives[0].origin != event.OriginEvent {
		t.Fatalf("directives = %+v, want an event directive to the finished owner, then the parent's notice", f.plane.directives)
	}
}

func TestPollerTellsTheParentOncePerStopThatAnOwnerStoppedWithLiveWatches(t *testing.T) {
	f := newFixture(t)
	registerAgent(t, f, "agent-1")
	id := f.watch("agent-1", testThread)
	if _, err := f.plane.st.CloseAgent(f.ctx, testSubject, "agent-1", f.now); err != nil {
		t.Fatal(err)
	}
	notices := func() []directed {
		var out []directed
		for _, d := range f.plane.directives {
			if d.agentID == agent.TopLevel {
				out = append(out, d)
			}
		}
		return out
	}
	stopped := func() []ownerStoppedPayload {
		var out []ownerStoppedPayload
		for _, e := range f.plane.events {
			if e.Type != eventOwnerStopped {
				continue
			}
			var p ownerStoppedPayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				t.Fatal(err)
			}
			out = append(out, p)
		}
		return out
	}

	f.slack.add(testChannel, reply("1800000000.000300", "first after the stop"))
	f.tick()
	got := stopped()
	if len(got) != 1 || got[0].AgentID != "agent-1" || len(got[0].Watches) != 1 || got[0].Watches[0].WatchID != id ||
		got[0].Watches[0].ThreadTS != testThread {
		t.Fatalf("owner_stopped events = %+v, want one naming agent-1 and watch %d", got, id)
	}
	if n := notices(); len(n) != 1 || !strings.Contains(n[0].text, "agent-1") || !strings.Contains(n[0].text, "SendMessage") ||
		!strings.Contains(n[0].text, "slack_unwatch") {
		t.Fatalf("parent directives = %+v, want one notice naming the agent and both remedies", n)
	}

	f.now = f.now.Add(time.Minute)
	f.slack.add(testChannel, reply("1800000060.000300", "second after the stop"))
	f.nudgeThread()
	f.tick()
	if len(stopped()) != 1 || len(notices()) != 1 {
		t.Fatalf("a second message on the same stop renotified: %d events, %d notices", len(stopped()), len(notices()))
	}

	registerAgent(t, f, "agent-1")
	f.now = f.now.Add(time.Minute)
	if _, err := f.plane.st.CloseAgent(f.ctx, testSubject, "agent-1", f.now); err != nil {
		t.Fatal(err)
	}
	f.slack.add(testChannel, reply("1800000120.000300", "after the second stop"))
	f.nudgeThread()
	f.tick()
	if len(stopped()) != 2 || len(notices()) != 2 {
		t.Fatalf("a later stop did not renotify: %d events, %d notices", len(stopped()), len(notices()))
	}
}

func TestPollerRoutesDMsFromTheOperatorOnly(t *testing.T) {
	f := newFixture(t)
	dm := f.id.DMChannelID
	status, err := f.app.post(f.ctx, f.db(), f.id, dm, "", "working on the migration", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.app.openSessionThread(f.ctx, f.db(), subjectFor(t, f), dm, status.TS); err != nil {
		t.Fatal(err)
	}
	f.slack.add(dm, slack.Message{Msg: slack.Msg{Timestamp: "1800000002.000000", ThreadTimestamp: status.TS, User: testUser, Text: "also bump the version"}})
	f.slack.add(dm, slack.Message{Msg: slack.Msg{Timestamp: "1800000003.000000", User: testUser, Text: "what's the status?"}})
	f.slack.add(dm, slack.Message{Msg: slack.Msg{Timestamp: "1800000004.000000", User: "UOTHER", Text: "not the operator"}})

	f.tick()
	texts := make([]string, 0, len(f.plane.events))
	for _, e := range f.plane.events {
		var got messagePayload
		if err := json.Unmarshal(e.Payload, &got); err != nil {
			t.Fatal(err)
		}
		if got.Kind != kindDM {
			t.Fatalf("kind = %q, want dm", got.Kind)
		}
		texts = append(texts, got.Text)
	}
	if strings.Join(texts, "|") != "also bump the version|what's the status?" {
		t.Fatalf("routed = %q, want the operator's thread reply and top-level DM", texts)
	}
	if len(f.plane.directives) != 2 || !strings.Contains(f.plane.directives[0].text, "your operator") {
		t.Fatalf("directives = %+v", f.plane.directives)
	}
}

func TestPollerFramesTheOperatorsThreadRepliesAsSteering(t *testing.T) {
	f := newFixture(t)
	f.watch(agent.TopLevel, testThread)
	f.slack.add(testChannel, slack.Message{Msg: slack.Msg{Timestamp: "1800000000.000200", ThreadTimestamp: testThread, User: testUser, Text: "aim for 5 min releases"}})
	f.slack.add(testChannel, reply("1800000000.000300", "sounds good"))

	f.tick()
	if len(f.plane.directives) != 2 {
		t.Fatalf("directives = %+v, want both replies", f.plane.directives)
	}
	if got := f.plane.directives[0].text; !strings.Contains(got, "your operator") || !strings.Contains(got, "operator steering") {
		t.Fatalf("operator reply directive = %q, want operator steering", got)
	}
	if got := f.plane.directives[1].text; strings.Contains(got, "your operator") || !strings.Contains(got, "not an operator instruction") {
		t.Fatalf("teammate reply directive = %q, want Slack content", got)
	}
}

func TestPollerPollsTheDMRootOnceForAChannelWatchOnIt(t *testing.T) {
	f := newFixture(t)
	dm := f.id.DMChannelID
	status, err := f.app.post(f.ctx, f.db(), f.id, dm, "", "status", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.app.openSessionThread(f.ctx, f.db(), subjectFor(t, f), dm, status.TS); err != nil {
		t.Fatal(err)
	}
	if _, _, err := insertWatch(f.ctx, f.db(), testSubject, agent.TopLevel, kindChannel, dm, "", status.TS, f.now); err != nil {
		t.Fatal(err)
	}
	f.slack.add(dm, slack.Message{Msg: slack.Msg{Timestamp: "1800000009.000000", User: testUser, Text: "what's the status?"}})

	f.tick()
	if n := f.slack.callCount(methodHistory); n != 1 {
		t.Fatalf("history calls = %d, want one poll of the shared DM root", n)
	}
	kinds := make([]string, 0, len(f.plane.events))
	for _, e := range f.plane.events {
		var got messagePayload
		if err := json.Unmarshal(e.Payload, &got); err != nil {
			t.Fatal(err)
		}
		kinds = append(kinds, got.Kind)
	}
	if strings.Join(kinds, ",") != kindChannel+","+kindDM {
		t.Fatalf("routed kinds = %q, want the channel watch and the session", kinds)
	}
}

func watchChannel(t *testing.T, f *fixture, oldest string) int64 {
	t.Helper()
	id, _, err := insertWatch(f.ctx, f.db(), testSubject, agent.TopLevel, kindChannel, testChannel, "", oldest, f.now)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func payloads(t *testing.T, f *fixture) []messagePayload {
	t.Helper()
	out := make([]messagePayload, 0, len(f.plane.events))
	for _, e := range f.plane.events {
		var got messagePayload
		if err := json.Unmarshal(e.Payload, &got); err != nil {
			t.Fatal(err)
		}
		out = append(out, got)
	}
	return out
}

func TestPollerDeliversEachThreadReplyToAChannelWatch(t *testing.T) {
	f := newFixture(t)
	f.slack.add(testChannel, slack.Message{Msg: slack.Msg{Timestamp: testThread, User: "UOTHER", Text: "Datadog alert"}})
	f.slack.add(testChannel, reply("1800000000.000200", "before the watch"))
	watchChannel(t, f, testThread)
	f.tick()
	calls := f.slack.callCount(methodReplies)

	f.slack.add(testChannel, reply("1800000000.000300", "on it"))
	nudge := messageNudge{Channel: testChannel, TS: "1800000000.000300", ThreadTS: testThread}
	f.poller.nudged(nudge)
	f.tick()
	if n := f.slack.callCount(methodReplies); n != calls+1 {
		t.Fatalf("replies calls = %d, want one read for the nudged reply", n-calls)
	}
	if oldest := f.slack.repliesOldest(); oldest[len(oldest)-1] != "1800000000.000299" {
		t.Fatalf("read from %q, want just before the nudged reply", oldest[len(oldest)-1])
	}
	got := payloads(t, f)
	if len(got) != 1 || got[0].Kind != kindChannel || got[0].ThreadTS != testThread || got[0].TS != "1800000000.000300" || got[0].Text != "on it" {
		t.Fatalf("events = %+v, want the one new reply on the channel watch", got)
	}

	f.poller.nudged(nudge)
	f.tick()
	if n := len(f.plane.events); n != 1 {
		t.Fatalf("events = %d after a repeated nudge, want the reply delivered once", n)
	}
}

func TestPollerKeepsAChannelWatchWhenOneOfItsThreadsVanishes(t *testing.T) {
	f := newFixture(t)
	id := watchChannel(t, f, testThread)
	f.tick()
	f.slack.fail(methodReplies, slack.SlackErrorResponse{Err: "thread_not_found"})

	f.poller.nudged(messageNudge{Channel: testChannel, TS: "1800000000.000300", ThreadTS: testThread})
	f.tick()
	if got := f.watchStatus(id); got != "active" {
		t.Fatalf("channel watch status = %q, want it active after one thread failed", got)
	}
	if len(f.plane.events) != 0 {
		t.Fatalf("events = %+v, want no watch error", f.plane.events)
	}
	if n := f.tick(); !n.IsZero() {
		t.Fatalf("next poll = %v, want the vanished thread dropped", n)
	}
}

func TestPollerSendsAReplyOnceToASessionWatchingItsThreadAndChannel(t *testing.T) {
	f := newFixture(t)
	f.watch(agent.TopLevel, testThread)
	watchChannel(t, f, testThread)
	f.tick()

	f.slack.add(testChannel, reply("1800000000.000300", "looks good"))
	f.nudgeThread()
	f.tick()
	got := payloads(t, f)
	if len(got) != 1 || got[0].Kind != kindThread {
		t.Fatalf("events = %+v, want the reply once, on the thread watch", got)
	}
}

func TestPollerKeepsEachSessionsWatchToThatSession(t *testing.T) {
	f := newFixture(t)
	const (
		otherSubject = "fedcba9876543210fedcba9876543210"
		otherThread  = "1800000000.000900"
	)
	if _, err := storeSubjects(f).Create(f.ctx, otherSubject, "", "sess-2", scopeName, testPID, statusOpen); err != nil {
		t.Fatal(err)
	}
	f.watch(agent.TopLevel, testThread)
	if _, _, err := insertWatch(f.ctx, f.db(), otherSubject, agent.TopLevel, kindThread, testChannel, otherThread, otherThread, f.now); err != nil {
		t.Fatal(err)
	}
	f.slack.add(testChannel, reply("1800000000.000300", "for the first session"))
	f.slack.add(testChannel, slack.Message{Msg: slack.Msg{Timestamp: "1800000000.000950", ThreadTimestamp: otherThread, User: "UOTHER", Text: "for the second session"}})

	f.tick()
	got := map[string]string{}
	for _, e := range f.plane.events {
		var m messagePayload
		if err := json.Unmarshal(e.Payload, &m); err != nil {
			t.Fatal(err)
		}
		got[e.SubjectID] += m.Text
	}
	want := map[string]string{testSubject: "for the first session", otherSubject: "for the second session"}
	if len(got) != len(want) || got[testSubject] != want[testSubject] || got[otherSubject] != want[otherSubject] {
		t.Fatalf("events by session = %q, want %q", got, want)
	}
	for _, d := range f.plane.directives {
		if (d.subjectID == testSubject) != strings.Contains(d.text, "first session") {
			t.Fatalf("directive %+v reached a session that did not watch its thread", d)
		}
	}
	if len(f.plane.directives) != 2 {
		t.Fatalf("directives = %+v, want one per session", f.plane.directives)
	}
}

func TestPollerListsSessionsForAnAmbiguousDM(t *testing.T) {
	f := newFixture(t)
	dm := f.id.DMChannelID
	for i, subjectID := range []string{testSubject, "fedcba9876543210fedcba9876543210"} {
		if i > 0 {
			if _, err := storeSubjects(f).Create(f.ctx, subjectID, "", "sess-2", scopeName, testPID, statusOpen); err != nil {
				t.Fatal(err)
			}
		}
		status, err := f.app.post(f.ctx, f.db(), f.id, dm, "", "status", nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		sub, err := storeSubjects(f).Get(f.ctx, subjectID)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.app.openSessionThread(f.ctx, f.db(), sub, dm, status.TS); err != nil {
			t.Fatal(err)
		}
	}
	f.slack.add(dm, slack.Message{Msg: slack.Msg{Timestamp: "1800000009.000000", User: testUser, Text: "which one?"}})

	f.tick()
	if len(f.plane.events) != 0 {
		t.Fatalf("ambiguous DM routed to a session: %+v", f.plane.events)
	}
	last := f.slack.sent()[len(f.slack.sent())-1]
	if last.threadTS != "1800000009.000000" || strings.Count(last.text, "• ") != 2 {
		t.Fatalf("session list post = %+v", last)
	}
}

func TestPollerKeepsANudgeMarkedAfterItsSnapshot(t *testing.T) {
	f := newFixture(t)
	key := cursorKey{testChannel, "1800000000.000900"}
	f.poller.mark(key)
	seen, _ := f.poller.pendingMarks()
	f.poller.mark(key)
	f.poller.keepWatched(nil, seen, false)
	if !f.poller.isPending(key) {
		t.Fatal("a nudge marked after the snapshot was pruned with the snapshot")
	}
	seen, _ = f.poller.pendingMarks()
	f.poller.keepWatched(nil, seen, false)
	if f.poller.isPending(key) {
		t.Fatal("a key no watch covers survived a snapshot that held its latest mark")
	}
}

func TestPostsRefreshAStaleProfileByUserID(t *testing.T) {
	f := newFixture(t)
	f.slack.setUser(&slack.User{ID: testUser, Name: "ym", Profile: slack.UserProfile{DisplayName: "andrew", Image512: "https://avatars/new.png"}})
	post := func(text string) fakePost {
		t.Helper()
		id, err := loadIdentity(f.ctx, f.db())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.app.post(f.ctx, f.db(), id, testChannel, "", text, nil, nil); err != nil {
			t.Fatal(err)
		}
		sent := f.slack.sent()
		return sent[len(sent)-1]
	}

	if got := post("fresh"); got.username != "yasyf (agent)" || f.slack.callCount("users.info") != 0 {
		t.Fatalf("post = %+v after %d users.info calls, want the saved profile and no lookup", got, f.slack.callCount("users.info"))
	}
	f.now = f.now.Add(profileTTL)
	if got := post("stale"); got.username != "andrew (agent)" || got.iconURL != "https://avatars/new.png?agent-badge" {
		t.Fatalf("post = %+v, want the profile users.info returns", got)
	}
	if got := post("refreshed"); got.username != "andrew (agent)" || f.slack.callCount("users.info") != 1 {
		t.Fatalf("post = %+v after %d users.info calls, want the saved refresh reused", got, f.slack.callCount("users.info"))
	}
}

func TestAFailedProfileRefreshKeepsTheSavedProfileForADay(t *testing.T) {
	f := newFixture(t)
	f.now = f.now.Add(profileTTL)
	f.slack.fail("users.info", slack.SlackErrorResponse{Err: "ratelimited"})
	for _, text := range []string{"still posts", "no second lookup"} {
		id, err := loadIdentity(f.ctx, f.db())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.app.post(f.ctx, f.db(), id, testChannel, "", text, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	for _, got := range f.slack.sent() {
		if got.username != "yasyf (agent)" {
			t.Fatalf("post = %+v, want the saved profile", got)
		}
	}
	if n := f.slack.callCount("users.info"); n != 1 {
		t.Fatalf("users.info calls = %d, want 1 until the saved profile goes stale again", n)
	}
}
