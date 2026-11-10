package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/slack-go/slack"

	"github.com/yasyf/cc-interact/agent"
	"github.com/yasyf/cc-interact/event"
	"github.com/yasyf/cc-interact/subject"
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

var fallbackEchoes = []string{
	"_(Yasyf's Claude)_ *10:10 PM status*\n\n*Done since 8 PM*",
	"_(Yasyf's Claude)_ *11:33 PM, one tap when you can*\n\nThe new code stopped compiling on dev at 10:02 PM",
	"_(Yasyf's Claude)_ *11:54 PM, alert change live*\n\nYour 11:28 PM platform release passed and is healthy",
}

func TestPollerFlagsTheOperatorsFallbackPostsAsFromClaude(t *testing.T) {
	f := newFixture(t)
	f.watch(agent.TopLevel, testThread)
	for i, text := range fallbackEchoes {
		f.slack.add(testChannel, slack.Message{Msg: slack.Msg{Timestamp: fmt.Sprintf("180000000%d.000300", i), ThreadTimestamp: testThread, User: testUser, Text: text}})
	}
	f.slack.add(testChannel, slack.Message{Msg: slack.Msg{Timestamp: "1800000009.000300", ThreadTimestamp: testThread, User: testUser, Text: "_(not a marker)_ ship it"}})

	f.tick()
	if len(f.plane.events) != 4 || len(f.plane.directives) != 4 {
		t.Fatalf("events = %d, directives = %d, want 4 each", len(f.plane.events), len(f.plane.directives))
	}
	for i, e := range f.plane.events {
		var got messagePayload
		if err := json.Unmarshal(e.Payload, &got); err != nil {
			t.Fatal(err)
		}
		human := i == len(fallbackEchoes)
		if got.FromClaude == human || (e.Origin == event.OriginHuman) != human {
			t.Fatalf("message %d: from_claude = %v origin %q, want human %v", i, got.FromClaude, e.Origin, human)
		}
		if strings.Contains(f.plane.directives[i].text, "your operator") != human {
			t.Fatalf("message %d directive = %q, want operator steering only for the human", i, f.plane.directives[i].text)
		}
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
	registerAgent(t, f)
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
	registerAgent(t, f)
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
	registerAgent(t, f)
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

	registerAgent(t, f)
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
	for i, text := range fallbackEchoes {
		f.slack.add(dm, slack.Message{Msg: slack.Msg{Timestamp: fmt.Sprintf("180000001%d.000000", i), ThreadTimestamp: status.TS, User: testUser, Text: text}})
	}

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

func TestAHeldStreamAloneCarriesTheMessageWhileItRuns(t *testing.T) {
	f := newFixture(t)
	f.watch(agent.TopLevel, testThread)
	const stream = testPID + 1
	f.alive[stream] = true
	f.poller.holdDelivery(testSubject, stream)

	f.slack.add(testChannel, reply("1800000000.000200", "while it runs"))
	f.tick()
	if len(f.plane.directives) != 0 {
		t.Fatalf("directives = %+v, want none while the stream holds delivery", f.plane.directives)
	}
	last := f.plane.events[len(f.plane.events)-1]
	if last.Type != eventStreamed || !strings.Contains(string(last.Payload), "while it runs") {
		t.Fatalf("event = %s %s, want the streamed message", last.Type, last.Payload)
	}

	f.alive[stream] = false
	f.slack.add(testChannel, reply("1800000000.000300", "after it exits"))
	f.nudgeThread()
	f.tick()
	if len(f.plane.directives) != 1 {
		t.Fatalf("directives = %+v, want the message once the stream is gone", f.plane.directives)
	}
	if last := f.plane.events[len(f.plane.events)-1]; last.Type != eventMessage {
		t.Fatalf("event type = %s, want a channel message once the stream is gone", last.Type)
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
	f.tick()
	history := f.slack.callCount(methodHistory)
	f.slack.add(dm, slack.Message{Msg: slack.Msg{Timestamp: "1800000009.000000", User: testUser, Text: "what's the status?"}})
	f.poller.nudged(messageNudge{Channel: dm, TS: "1800000009.000000"})

	f.tick()
	if n := f.slack.callCount(methodHistory) - history; n != 1 {
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
	if strings.Join(kinds, ",") != kindChannel {
		t.Fatalf("routed kinds = %q, want only the channel watch, which holds the DM", kinds)
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
	if n := f.slack.callCount("reactions.add"); n != 0 {
		t.Fatalf("reactions.add calls = %d, want no eyes on a message no session received", n)
	}
	last := f.slack.sent()[len(f.slack.sent())-1]
	if last.threadTS != "1800000009.000000" || strings.Count(last.text, "• ") != 2 {
		t.Fatalf("session list post = %+v", last)
	}
}

func TestTheDaemonPutsEyesAndTheWorkingLineOnAnOperatorMessageAsItReachesASession(t *testing.T) {
	f := newFixture(t)
	dm := f.id.DMChannelID
	holdDM(t, f)
	thread := postDMThread(t, f)
	f.tick()

	ownerReplies(f, thread, "1800000001.500000", "ship it")
	f.slack.add(dm, slack.Message{Msg: slack.Msg{Timestamp: "1800000001.600000", ThreadTimestamp: thread, User: "UOTHER", Text: "not the operator"}})
	f.poller.nudged(messageNudge{Channel: dm, TS: "1800000001.600000", ThreadTS: thread})
	f.tick()

	if got := f.slack.reactedTo("1800000001.500000"); !slices.Equal(got, []string{pickupReaction}) {
		t.Fatalf("reactions on the operator's reply = %v, want eyes", got)
	}
	if got := f.slack.reactedTo("1800000001.600000"); len(got) != 0 {
		t.Fatalf("reactions on another user's message = %v, want none", got)
	}
	f.slack.mu.Lock()
	defer f.slack.mu.Unlock()
	if len(f.slack.statuses) != 1 || f.slack.statuses[0].ThreadTS != thread || f.slack.statuses[0].Status != workingStatus || !slices.Equal(f.slack.statuses[0].LoadingMessages, arrivalLoading) {
		t.Fatalf("working lines = %+v, want one %v line under thread %s", f.slack.statuses, arrivalLoading, thread)
	}
}

func TestWhileItsSessionCompactsAnOperatorMessageGetsOneAckThatAnswersNothing(t *testing.T) {
	f := newFixture(t)
	dm := f.id.DMChannelID
	holdDM(t, f)
	thread := postDMThread(t, f)
	f.tick()
	f.poller.startCompacting(testSubject)

	ownerReplies(f, thread, "1800000001.500000", "ship it")

	acks := func() []fakePost {
		return slices.DeleteFunc(f.slack.sent(), func(p fakePost) bool { return p.text != compactionAck })
	}
	posted := acks()
	if len(posted) != 1 || posted[0].threadTS != thread {
		t.Fatalf("acks = %+v, want one %q in thread %s", posted, compactionAck, thread)
	}
	delivered := payloads(t, f)
	if got := delivered[len(delivered)-1]; got.TS != "1800000001.500000" || got.Acked != posted[0].ts {
		t.Fatalf("delivery = %+v, want the reply carrying acked %s", got, posted[0].ts)
	}
	if got := f.plane.directives[len(f.plane.directives)-1].text; !strings.Contains(got, "do not ack it again") {
		t.Fatalf("directive = %q, want it to say the reply is already acked", got)
	}
	if got := f.slack.reactedTo("1800000001.500000"); !slices.Equal(got, []string{pickupReaction}) {
		t.Fatalf("reactions on the reply = %v, want eyes", got)
	}
	f.poller.nudged(messageNudge{Channel: dm, TS: posted[0].ts, ThreadTS: thread})
	f.tick()
	if got := owedStamps(t, f); !slices.Equal(got, []string{"1800000001.500000"}) {
		t.Fatalf("owed %v, want the acked reply still owed its answer", got)
	}

	if !f.poller.endCompacting(testSubject) {
		t.Fatal("the session was not marked compacting")
	}
	ownerReplies(f, thread, "1800000009.500000", "and this")
	if got := acks(); len(got) != 1 {
		t.Fatalf("acks = %+v, want none once the session is back", got)
	}
	if got := payloads(t, f); got[len(got)-1].Acked != "" {
		t.Fatalf("delivery = %+v, want no ack once the session is back", got[len(got)-1])
	}
}

func TestPollerKeepsANudgeMarkedAfterItsSnapshot(t *testing.T) {
	f := newFixture(t)
	key := cursorKey{testChannel, "1800000000.000900"}
	f.poller.mark(key)
	seen, _ := f.poller.pendingMarks()
	f.poller.mark(key)
	f.poller.keepWatched(nil, seen)
	if !f.poller.isPending(key) {
		t.Fatal("a nudge marked after the snapshot was pruned with the snapshot")
	}
	seen, _ = f.poller.pendingMarks()
	f.poller.keepWatched(nil, seen)
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

func secondSession(t *testing.T, f *fixture) subject.Subject {
	t.Helper()
	const id = "fedcba9876543210fedcba9876543210"
	if _, err := storeSubjects(f).Create(f.ctx, id, "", "sess-2", scopeName, testPID, statusOpen); err != nil {
		t.Fatal(err)
	}
	sub, err := storeSubjects(f).Get(f.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return sub
}

func dmMessageOf(t *testing.T, f *fixture, sub subject.Subject) string {
	t.Helper()
	dm := f.id.DMChannelID
	status, err := f.app.post(f.ctx, f.db(), f.id, dm, "", "an update", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.app.openSessionThread(f.ctx, f.db(), sub, dm, status.TS); err != nil {
		t.Fatal(err)
	}
	return status.TS
}

func TestAThreadReplyRoutesToTheSessionThatPostedTheMessage(t *testing.T) {
	f := newFixture(t)
	dm := f.id.DMChannelID
	first, other := subjectFor(t, f), secondSession(t, f)
	older := dmMessageOf(t, f, first)
	dmMessageOf(t, f, other)
	dmMessageOf(t, f, first)
	f.tick()

	f.slack.add(dm, slack.Message{Msg: slack.Msg{Timestamp: "1800000090.000000", ThreadTimestamp: older, User: testUser, Text: "hold this one"}})
	f.poller.nudged(messageNudge{Channel: dm, TS: "1800000090.000000", ThreadTS: older})
	f.tick()
	if len(f.plane.events) != 1 || f.plane.events[0].SubjectID != first.ID {
		t.Fatalf("events = %+v, want the reply delivered once to the session that posted %s", f.plane.events, older)
	}
}

func TestADMRootMessageListsEachSessionOnceByItsNewestMessage(t *testing.T) {
	f := newFixture(t)
	dm := f.id.DMChannelID
	first, other := subjectFor(t, f), secondSession(t, f)
	older := dmMessageOf(t, f, first)
	newest := dmMessageOf(t, f, first)
	dmMessageOf(t, f, other)
	f.tick()

	f.slack.add(dm, slack.Message{Msg: slack.Msg{Timestamp: "1800000090.000000", User: testUser, Text: "which one?"}})
	f.poller.nudged(messageNudge{Channel: dm, TS: "1800000090.000000"})
	f.tick()
	last := f.slack.sent()[len(f.slack.sent())-1]
	if strings.Count(last.text, "• ") != 2 || !strings.Contains(last.text, f.id.permalink(dm, newest, "")) || strings.Contains(last.text, f.id.permalink(dm, older, "")) {
		t.Fatalf("session list = %q, want each session once by its newest message", last.text)
	}
}

func TestASessionKeepsItsNewestDMMessageWatches(t *testing.T) {
	f := newFixture(t)
	sub := subjectFor(t, f)
	dm := f.id.DMChannelID
	for i := range sessionMessageWatches + 1 {
		if err := f.app.openSessionThread(f.ctx, f.db(), sub, dm, fmt.Sprintf("1800000000.%06d", i)); err != nil {
			t.Fatal(err)
		}
	}
	threads, err := sessionThreads(f.ctx, f.db(), sub.ID, dm)
	if err != nil {
		t.Fatal(err)
	}
	if len(threads) != sessionMessageWatches || threads[0] != fmt.Sprintf("1800000000.%06d", sessionMessageWatches) || slices.Contains(threads, "1800000000.000000") {
		t.Fatalf("%d active watches, newest %s: want the newest %d and the oldest released", len(threads), threads[0], sessionMessageWatches)
	}
	if err := f.app.openSessionThread(f.ctx, f.db(), sub, dm, threads[0]); err != nil {
		t.Fatal(err)
	}
	again, err := sessionThreads(f.ctx, f.db(), sub.ID, dm)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(again, threads) {
		t.Fatal("opening a watched message again changed the session's watches")
	}
}

func TestReconnectCallsStayFlatAtAnyMessageCount(t *testing.T) {
	for _, n := range []int{1, sessionMessageWatches} {
		f := newFixture(t)
		sub := subjectFor(t, f)
		dm := f.id.DMChannelID
		newest := ""
		for range n {
			newest = dmMessageOf(t, f, sub)
		}
		f.tick()
		f.tick()
		replies, history := f.slack.callCount(methodReplies), f.slack.callCount(methodHistory)

		f.poller.reconcileAll()
		f.tick()
		if r, h := f.slack.callCount(methodReplies)-replies, f.slack.callCount(methodHistory)-history; r != 0 || h != 2 {
			t.Fatalf("N=%d: a quiet reconnect made %d replies and %d history calls, want 0 and 2", n, r, h)
		}

		f.slack.add(dm, slack.Message{Msg: slack.Msg{Timestamp: "1900000000.000000", ThreadTimestamp: newest, User: testUser, Text: "sent while the socket was down"}})
		replies = f.slack.callCount(methodReplies)
		f.poller.reconcileAll()
		f.tick()
		if r := f.slack.callCount(methodReplies) - replies; r != 1 || len(f.plane.events) != 1 {
			t.Fatalf("N=%d: reconnect after one missed reply made %d replies calls and %d events, want 1 and 1", n, r, len(f.plane.events))
		}
	}
}

func operatorReplies(f *fixture, channel, thread string, n int) {
	for i := range n {
		f.slack.add(channel, slack.Message{Msg: slack.Msg{Timestamp: fmt.Sprintf("18000002%02d.000000", i), ThreadTimestamp: thread, User: testUser, Text: fmt.Sprintf("order %d", i)}})
	}
}

func orphanWatches(t *testing.T, f *fixture, subjectID string) {
	t.Helper()
	if _, err := f.db().ExecContext(f.ctx, `UPDATE watches SET status = 'orphaned' WHERE subject_id = ?`, subjectID); err != nil {
		t.Fatal(err)
	}
}

func TestAResumedSessionReopeningAThreadGetsNoDirectiveAgain(t *testing.T) {
	f := newFixture(t)
	dm := f.id.DMChannelID
	sub := subjectFor(t, f)
	thread := dmMessageOf(t, f, sub)
	operatorReplies(f, dm, thread, 10)
	f.poller.nudged(messageNudge{Channel: dm, TS: "1800000209.000000", ThreadTS: thread})
	f.tick()
	if len(f.plane.events) != 10 || len(f.plane.directives) != 10 {
		t.Fatalf("first delivery: %d events, %d directives, want 10 each", len(f.plane.events), len(f.plane.directives))
	}

	orphanWatches(t, f, sub.ID)
	if err := f.app.bindSessionThread(f.ctx, f.db(), sub, f.id, thread); err != nil {
		t.Fatal(err)
	}
	f.tick()
	if len(f.plane.events) != 10 || len(f.plane.directives) != 10 {
		t.Fatalf("after the resume: %d events, %d directives, want the same 10", len(f.plane.events), len(f.plane.directives))
	}
	c, err := loadCursor(f.ctx, f.db(), cursorKey{dm, thread})
	if err != nil {
		t.Fatal(err)
	}
	if c.oldest != "1800000209.000000" {
		t.Fatalf("cursor = %q, want it left on the last delivered reply", c.oldest)
	}
}

func TestAResumedSessionStillGetsWhatArrivedWhileItWasDown(t *testing.T) {
	f := newFixture(t)
	dm := f.id.DMChannelID
	sub := subjectFor(t, f)
	thread := dmMessageOf(t, f, sub)
	f.tick()

	orphanWatches(t, f, sub.ID)
	f.slack.add(dm, slack.Message{Msg: slack.Msg{Timestamp: "1800000300.000000", ThreadTimestamp: thread, User: testUser, Text: "sent during the restart"}})
	if err := f.app.bindSessionThread(f.ctx, f.db(), sub, f.id, thread); err != nil {
		t.Fatal(err)
	}
	f.tick()
	if len(f.plane.directives) != 1 || !strings.Contains(f.plane.directives[0].text, "sent during the restart") {
		t.Fatalf("directives = %+v, want the one message sent during the restart", f.plane.directives)
	}
}

func TestOneMessageReachesASessionOnceAcrossItsWatches(t *testing.T) {
	f := newFixture(t)
	dm := f.id.DMChannelID
	sub := subjectFor(t, f)
	thread := dmMessageOf(t, f, sub)
	operatorReplies(f, dm, thread, 1)
	f.poller.nudged(messageNudge{Channel: dm, TS: "1800000200.000000", ThreadTS: thread})
	f.tick()

	orphanWatches(t, f, sub.ID)
	if _, err := f.db().ExecContext(f.ctx, `UPDATE cursors SET oldest_ts = ? WHERE channel_id = ? AND thread_ts = ?`, thread, dm, thread); err != nil {
		t.Fatal(err)
	}
	if err := f.app.bindSessionThread(f.ctx, f.db(), sub, f.id, thread); err != nil {
		t.Fatal(err)
	}
	f.tick()
	if len(f.plane.directives) != 1 {
		t.Fatalf("directives = %d, want the message delivered once under its own ts", len(f.plane.directives))
	}
	if n := f.slack.callCount("reactions.add"); n != 1 {
		t.Fatalf("reactions.add calls = %d, want eyes only when the message first reached the session", n)
	}
}

func TestSessionStartBringsBackTheWatchesARestartOrphaned(t *testing.T) {
	f := newFixture(t)
	dm := f.id.DMChannelID
	sub := subjectFor(t, f)
	thread := dmMessageOf(t, f, sub)
	stopped := dmMessageOf(t, f, sub)
	if _, err := f.db().ExecContext(f.ctx, `UPDATE watches SET status = 'stopped' WHERE subject_id = ? AND thread_ts = ?`, sub.ID, stopped); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db().ExecContext(f.ctx, `UPDATE watches SET status = 'orphaned' WHERE subject_id = ? AND thread_ts = ?`, sub.ID, thread); err != nil {
		t.Fatal(err)
	}

	back, err := reviveWatches(f.ctx, f.db(), sub.ID, dm)
	if err != nil {
		t.Fatal(err)
	}
	if back != 1 {
		t.Fatalf("back = %d, want the one orphaned watch", back)
	}
	threads, err := sessionThreads(f.ctx, f.db(), sub.ID, dm)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(threads, thread) || slices.Contains(threads, stopped) {
		t.Fatalf("active threads = %v, want %s back and %s still stopped", threads, thread, stopped)
	}
	if again, err := reviveWatches(f.ctx, f.db(), sub.ID, dm); err != nil || again != 0 {
		t.Fatalf("second pass = %d err=%v, want nothing", again, err)
	}
}

func TestARestartedSessionDoesNotTakeBackTheDMFromAnother(t *testing.T) {
	f := newFixture(t)
	dm := f.id.DMChannelID
	sub, other := subjectFor(t, f), secondSession(t, f)
	for _, subjectID := range []string{sub.ID, other.ID} {
		if _, _, err := insertWatch(f.ctx, f.db(), subjectID, agent.TopLevel, kindChannel, dm, "", "1800000000.000000", f.now); err != nil {
			t.Fatal(err)
		}
	}
	orphanWatches(t, f, sub.ID)

	back, err := reviveWatches(f.ctx, f.db(), sub.ID, dm)
	if err != nil {
		t.Fatal(err)
	}
	if back != 0 {
		t.Fatalf("back = %d, want the DM left with the session that holds it", back)
	}
}
