package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/slack-go/slack"
)

func TestWritesWithoutACoveringTicketNeverReachSlack(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)

	l.refused(opSend, map[string]any{"channel_id": testChannel, "text": "PR is up"}, "no ticket covers this post")
	l.refused(opReply, map[string]any{"channel_id": testChannel, "thread_ts": testThread, "text": "CI is green"}, "no ticket covers this post")
	l.refused(opEdit, map[string]any{"channel_id": testChannel, "ts": testThread, "text": "edited"}, "no ticket covers this edit")
	l.refused(opSend, l.spending("0123456789ab", map[string]any{"channel_id": testChannel, "text": "PR is up"}), "ticket 0123456789ab does not cover this post")
	spoofed := l.hooked(opSend, map[string]any{"channel_id": testChannel, "text": "PR is up"})
	spoofed["ticket_tree"] = "another-root"
	l.refused(opSend, spoofed, "does not include this action")
	if posts, edits := l.slack.callCount("chat.postMessage"), l.slack.callCount("chat.update"); posts != 0 || edits != 0 {
		t.Fatalf("chat.postMessage %d, chat.update %d, want the fake Slack API untouched", posts, edits)
	}
}

func TestSessionsWithoutThePermissionHookWriteWithoutATicket(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	l.app.claudeCwd = projectWith(t)

	var res postResult
	if err := json.Unmarshal(l.do(opSend, map[string]any{"channel_id": testChannel, "text": "PR is up"}).Body, &res); err != nil {
		t.Fatal(err)
	}
	if !res.Posted || res.TicketID != "" {
		t.Fatalf("send = %+v, want it posted with no ticket", res)
	}
	if n := l.tickets.callCount(); n != 0 {
		t.Fatalf("capt-hook ran %d times, want never for a session no hook mints tickets in", n)
	}
	l.refused(opSend, l.spending("0123456789ab", map[string]any{"channel_id": testChannel, "text": "PR is up"}), "ticket 0123456789ab does not cover this post")
}

func TestSpendCarriesTheHookIdentityToCaptHook(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)

	body := l.hooked(opReply, map[string]any{"url": "https://acme.slack.com/archives/C1/p1800000000000100", "text": "CI is green"})
	var res postResult
	if err := json.Unmarshal(l.do(opReply, body).Body, &res); err != nil {
		t.Fatal(err)
	}
	want := []string{
		testCaptHook, "ticket", "spend", field(body, "ticket_id"),
		"--place", "channel=" + testChannel, "--place", "thread=" + testThread,
		"--tree", testTree, "--session", testSession, "--agent", "main", "--call", field(body, "ticket_call"),
		"--fingerprint", field(body, "ticket_fingerprint"), "--summary", "a reply in " + testChannel + "/" + testThread,
	}
	if got := l.tickets.lastCall(); !slices.Equal(got, want) {
		t.Fatalf("capt-hook argv = %q\nwant %q", got, want)
	}
	if !res.Posted || res.TicketID != body["ticket_id"] {
		t.Fatalf("reply = %+v, want it posted under %v", res, body["ticket_id"])
	}
}

func TestOnceTicketCoversExactlyOneWrite(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	for i := range 100 {
		l.slack.add(testChannel, slack.Message{Msg: slack.Msg{Timestamp: fmt.Sprintf("1800000000.%06d", i+1), ThreadTimestamp: testThread, User: "UOTHER", Text: "chatter"}})
	}

	body := l.hooked(opReply, map[string]any{"channel_id": testChannel, "thread_ts": testThread, "text": "CI is green"})
	ticketID := field(body, "ticket_id")
	var first, again postResult
	if err := json.Unmarshal(l.do(opReply, body).Body, &first); err != nil {
		t.Fatal(err)
	}
	if !first.Posted || !first.Watched || first.Replayed || first.TicketID != ticketID || first.WatchID == 0 {
		t.Fatalf("first reply = %+v, want posted and watched under ticket %s", first, ticketID)
	}
	if err := json.Unmarshal(l.do(opReply, body).Body, &again); err != nil {
		t.Fatal(err)
	}
	if !again.Replayed || again.TS != first.TS || again.WatchID != first.WatchID || !again.Posted {
		t.Fatalf("second reply = %+v, want the first's receipt replayed", again)
	}
	if spends := l.tickets.spent(ticketID); len(spends) != 1 {
		t.Fatalf("spends = %+v, want the retry to spend nothing more", spends)
	}
	var later postResult
	if err := json.Unmarshal(l.do(opReply, retried(body)).Body, &later); err != nil {
		t.Fatal(err)
	}
	if !later.Replayed || later.TS != first.TS {
		t.Fatalf("reply in a later call = %+v, want the first's receipt replayed", later)
	}

	l.refused(opReply, l.spending(ticketID, map[string]any{"channel_id": testChannel, "thread_ts": testThread, "text": "CI is red"}), "does not include this action")
	l.refused(opReply, l.spending(ticketID, map[string]any{"channel_id": testChannel, "thread_ts": "1800000000.000999", "text": "CI is green"}), "does not include this action")
	l.refused(opSend, l.spending(ticketID, map[string]any{"channel_id": "C2", "thread_ts": testThread, "text": "CI is green"}), "does not include this action")

	revoked := l.hooked(opSend, map[string]any{"channel_id": testChannel, "text": "stand down"})
	l.do(opRevoke, map[string]any{"ticket_id": revoked["ticket_id"]})
	l.refused(opSend, revoked, "revoked")
	if n := l.slack.callCount("chat.postMessage"); n != 1 {
		t.Fatalf("chat.postMessage calls = %d, want one post for one ticket", n)
	}
}

func TestPostedThenWatchFailureIsReceiptedNotResent(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)

	body := l.hooked(opSend, map[string]any{"channel_id": testChannel, "text": "deploy is out"})
	if _, err := l.srv.DB().ExecContext(l.ctx, `ALTER TABLE watches RENAME TO watches_offline`); err != nil {
		t.Fatal(err)
	}
	var res postResult
	if err := json.Unmarshal(l.do(opSend, body).Body, &res); err != nil {
		t.Fatal(err)
	}
	if !res.Posted || res.Watched || res.WatchError == "" || res.WatchID != 0 {
		t.Fatalf("send = %+v, want posted with the watch failure reported", res)
	}
	if _, err := l.srv.DB().ExecContext(l.ctx, `ALTER TABLE watches_offline RENAME TO watches`); err != nil {
		t.Fatal(err)
	}
	stored, found, err := loadReceipt(l.ctx, l.srv.DB(), field(body, "ticket_id"))
	if err != nil || !found || stored.TS != res.TS || stored.WatchError != res.WatchError {
		t.Fatalf("receipt = %+v (%t, %v), want the post and its watch failure recorded", stored, found, err)
	}
	var again postResult
	if err := json.Unmarshal(l.do(opSend, body).Body, &again); err != nil {
		t.Fatal(err)
	}
	if !again.Replayed || again.TS != res.TS || again.Watched {
		t.Fatalf("resend = %+v, want the receipt replayed, not a second post", again)
	}
	if n := l.slack.callCount("chat.postMessage"); n != 1 {
		t.Fatalf("chat.postMessage calls = %d, want no resend after a watch failure", n)
	}
}

func TestPostFailureLeavesTheTicketUncertainUntilAFreshOne(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)

	text := map[string]any{"channel_id": testChannel, "text": "deploy is out"}
	body := l.hooked(opSend, text)
	l.slack.fail("chat.postMessage", slack.SlackErrorResponse{Err: "fatal_error"})
	l.refused(opSend, body, "fatal_error")
	l.refused(opSend, body, "left no receipt")
	if n := l.slack.callCount("chat.postMessage"); n != 1 {
		t.Fatalf("chat.postMessage calls = %d, want no retry while the first outcome is unknown", n)
	}
	fresh := l.hooked(opSend, text)
	var res postResult
	if err := json.Unmarshal(l.do(opSend, fresh).Body, &res); err != nil {
		t.Fatal(err)
	}
	if !res.Posted || res.TicketID != fresh["ticket_id"] || l.slack.callCount("chat.postMessage") != 2 {
		t.Fatalf("send under a fresh ticket = %+v, want it posted under %v", res, fresh["ticket_id"])
	}
}

func TestSlackRefusalHandsTheTicketBack(t *testing.T) {
	notIn := slack.SlackErrorResponse{Err: "not_in_channel"}
	for name, refusals := range map[string][]struct {
		method string
		err    error
	}{
		"refused":      {{"chat.postMessage", slack.SlackErrorResponse{Err: "is_archived"}}},
		"rate limited": {{"chat.postMessage", &slack.RateLimitedError{RetryAfter: time.Second}}},
		"private":      {{"chat.postMessage", notIn}, {"conversations.join", slack.SlackErrorResponse{Err: "channel_not_found"}}},
		"after a join": {{"chat.postMessage", notIn}, {"chat.postMessage", notIn}},
	} {
		t.Run(name, func(t *testing.T) {
			l := startDaemon(t)
			loggedIn(t, l)
			body := l.hooked(opSend, map[string]any{"channel_id": testChannel, "text": "deploy is out"})
			ticketID := field(body, "ticket_id")
			for _, r := range refusals {
				l.slack.fail(r.method, r.err)
			}
			l.refused(opSend, body, "nothing was sent, so ticket "+ticketID+" still covers it")
			if got, want := l.tickets.lastCall(), []string{testCaptHook, "ticket", "release", ticketID, "--call", field(body, "ticket_call")}; !slices.Equal(got, want) {
				t.Fatalf("capt-hook argv = %q, want %q", got, want)
			}
			if spends := l.tickets.committed(ticketID); len(spends) != 0 {
				t.Fatalf("committed spends = %+v, want the refused post's use handed back", spends)
			}
			if _, found, err := loadReceipt(l.ctx, l.srv.DB(), ticketID); err != nil || found {
				t.Fatalf("receipt found %t (%v), want the refused post's claim cleared", found, err)
			}
			var res postResult
			if err := json.Unmarshal(l.do(opSend, retried(body)).Body, &res); err != nil {
				t.Fatal(err)
			}
			if !res.Posted || res.TicketID != ticketID || len(l.tickets.committed(ticketID)) != 1 || len(l.slack.sentTo(testChannel)) != 1 {
				t.Fatalf("resend = %+v, want one post under the handed-back ticket %s", res, ticketID)
			}
		})
	}
}

func TestFailedHandBackReportsAHookDefectInsteadOfAReAsk(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	body := l.hooked(opSend, map[string]any{"channel_id": testChannel, "text": "deploy is out"})
	l.slack.fail("chat.postMessage", slack.SlackErrorResponse{Err: "is_archived"})
	l.tickets.mu.Lock()
	l.tickets.releaseErr = errors.New("capt-hook host unreachable")
	l.tickets.mu.Unlock()
	raw, _ := json.Marshal(body)
	_, err := invoke(l.ctx, l.deps, testSession, "/any", opSend, raw)
	if err == nil {
		t.Fatal("send succeeded, want the Slack refusal")
	}
	for _, want := range []string{"releasing ticket " + field(body, "ticket_id") + " failed: capt-hook host unreachable", "hook defect", "spawns a hook-fix lane", "retry the same write"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal = %q, want it to name %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "approve it again") {
		t.Errorf("refusal = %q, want no re-ask of the user", err)
	}
	if _, found, err := loadReceipt(l.ctx, l.srv.DB(), field(body, "ticket_id")); err != nil || found {
		t.Fatalf("receipt found %t (%v), want the claim cleared even when the release fails", found, err)
	}
}

func TestOutcomeUnknownKeepsTheTicketSpent(t *testing.T) {
	for name, err := range map[string]error{
		"fatal":   slack.SlackErrorResponse{Err: "fatal_error"},
		"network": errors.New("read tcp: connection reset by peer"),
	} {
		t.Run(name, func(t *testing.T) {
			l := startDaemon(t)
			loggedIn(t, l)
			body := l.hooked(opSend, map[string]any{"channel_id": testChannel, "text": "deploy is out"})
			l.slack.fail("chat.postMessage", err)
			raw, _ := json.Marshal(body)
			if _, err := invoke(l.ctx, l.deps, testSession, "/any", opSend, raw); err == nil || strings.Contains(err.Error(), "still covers it") {
				t.Fatalf("send = %v, want a failure that keeps the ticket spent", err)
			}
			if spends := l.tickets.committed(field(body, "ticket_id")); len(spends) != 1 {
				t.Fatalf("committed spends = %+v, want the use kept while the outcome is unknown", spends)
			}
		})
	}
}

func TestDaemonRefusalsHandTheTicketBack(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)

	missing := l.hooked(opSend, map[string]any{"channel_id": testChannel, "text": "the map", "files": []string{"/nonexistent/map.png"}})
	l.refused(opSend, missing, "still covers it")
	edit := l.hooked(opEdit, map[string]any{"channel_id": testChannel, "ts": testThread, "text": "edited"})
	l.slack.fail("chat.update", slack.SlackErrorResponse{Err: "cant_update_message"})
	l.refused(opEdit, edit, "still covers it")
	create := l.hooked(opChannelCreate, map[string]any{"name": "deploys"})
	l.slack.fail("conversations.create", slack.SlackErrorResponse{Err: "name_taken"})
	l.refused(opChannelCreate, create, "still covers it")
	for _, body := range []map[string]any{missing, edit, create} {
		if spends := l.tickets.committed(field(body, "ticket_id")); len(spends) != 0 {
			t.Fatalf("ticket %s committed spends = %+v, want every refused write's use handed back", field(body, "ticket_id"), spends)
		}
	}
}

func TestReplayAnswersBeforeAttachmentsAreChecked(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	png := writeFile(t, "a.png", pngBytes)
	body := l.hooked(opSend, map[string]any{"channel_id": testChannel, "text": "the map", "files": []string{png}})
	var first, again postResult
	if err := json.Unmarshal(l.do(opSend, body).Body, &first); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(png); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(l.do(opSend, body).Body, &again); err != nil {
		t.Fatal(err)
	}
	if !again.Replayed || again.TS != first.TS {
		t.Fatalf("retry after the file vanished = %+v, want the receipt replayed", again)
	}

	pending := l.hooked(opSend, map[string]any{"channel_id": testChannel, "text": "the map", "files": []string{png}})
	l.refused(opSend, pending, "attachment")
	if err := os.WriteFile(png, []byte(pngBytes), 0o600); err != nil {
		t.Fatal(err)
	}
	var later postResult
	if err := json.Unmarshal(l.do(opSend, pending).Body, &later); err != nil {
		t.Fatalf("retry once the attachment is back: %v", err)
	}
	if !later.Posted || later.Replayed || l.slack.callCount("chat.postMessage") != 2 {
		t.Fatalf("retry = %+v, want a refused attachment to leave its ticket usable for the same write", later)
	}
}

func TestEditNeedsItsOwnTicket(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)

	var sent postResult
	if err := json.Unmarshal(l.post(opSend, map[string]any{"channel_id": testChannel, "text": "PR is up"}).Body, &sent); err != nil {
		t.Fatal(err)
	}
	edit := map[string]any{"channel_id": testChannel, "ts": sent.TS, "text": "PR is up and green"}
	l.refused(opEdit, edit, "no ticket covers this edit")
	postTicket := l.hooked(opSend, map[string]any{"channel_id": testChannel, "text": "PR is up and green"})
	l.refused(opEdit, l.spending(field(postTicket, "ticket_id"), edit), "does not include this action")

	body := l.hooked(opEdit, edit)
	var edited, again postResult
	if err := json.Unmarshal(l.do(opEdit, body).Body, &edited); err != nil {
		t.Fatal(err)
	}
	if !edited.Posted || edited.TicketID != body["ticket_id"] || edited.TS != sent.TS {
		t.Fatalf("edit = %+v, want it receipted under %v", edited, body["ticket_id"])
	}
	if err := json.Unmarshal(l.do(opEdit, body).Body, &again); err != nil {
		t.Fatal(err)
	}
	if !again.Replayed || again.TS != sent.TS {
		t.Fatalf("second edit = %+v, want the receipt replayed", again)
	}
	if edits := l.slack.edited(); len(edits) != 1 || edits[0].text != "PR is up and green" {
		t.Fatalf("edits = %+v, want exactly one chat.update", edits)
	}
}

func TestDeleteNeedsItsOwnTicket(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)

	var sent postResult
	if err := json.Unmarshal(l.post(opSend, map[string]any{"channel_id": testChannel, "text": "PR is up"}).Body, &sent); err != nil {
		t.Fatal(err)
	}
	del := map[string]any{"channel_id": testChannel, "ts": sent.TS}
	l.refused(opDelete, del, "no ticket covers this delete")
	editTicket := l.hooked(opEdit, map[string]any{"channel_id": testChannel, "ts": sent.TS, "text": "PR is up and green"})
	l.refused(opDelete, l.spending(field(editTicket, "ticket_id"), del), "does not include this action")

	body := l.hooked(opDelete, del)
	var deleted, again postResult
	if err := json.Unmarshal(l.do(opDelete, body).Body, &deleted); err != nil {
		t.Fatal(err)
	}
	if deleted.TicketID != body["ticket_id"] || deleted.TS != sent.TS {
		t.Fatalf("delete = %+v, want it receipted under %v", deleted, body["ticket_id"])
	}
	if err := json.Unmarshal(l.do(opDelete, body).Body, &again); err != nil {
		t.Fatal(err)
	}
	if !again.Replayed || again.TS != sent.TS {
		t.Fatalf("second delete = %+v, want the receipt replayed", again)
	}
	if deletes := l.slack.deleted(); len(deletes) != 1 || deletes[0].ts != sent.TS {
		t.Fatalf("deletes = %+v, want exactly one chat.delete", deletes)
	}
}

func TestChannelPlaceTicketCoversTopLevelWritesInItsChannelOnly(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)

	ticketID := l.standing("")
	var first, second postResult
	if err := json.Unmarshal(l.do(opSend, l.spending(ticketID, map[string]any{"channel_id": testChannel, "text": "deploy started", "no_watch": true})).Body, &first); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(l.do(opSend, l.spending(ticketID, map[string]any{"channel_id": testChannel, "text": "deploy finished", "no_watch": true})).Body, &second); err != nil {
		t.Fatal(err)
	}
	if posts := l.slack.sent(); len(posts) != 2 || posts[0].threadTS != "" || posts[1].threadTS != "" || !first.Posted || !second.Posted || first.TicketID != ticketID || second.TicketID != ticketID {
		t.Fatalf("posts = %+v, first = %+v, want two unreceipted top-level posts naming the channel ticket", posts, first)
	}
	if n := l.slack.callCount(methodReplies); n != 0 {
		t.Fatalf("conversations.replies calls = %d, want a top-level post to read no thread", n)
	}

	l.refused(opReply, l.spending(ticketID, map[string]any{"channel_id": testChannel, "thread_ts": first.TS, "text": "rollout at 50%"}), "does not include this action")
	l.refused(opSend, l.spending(ticketID, map[string]any{"channel_id": "C2", "text": "x"}), "does not include this action")
	l.do(opEdit, l.spending(ticketID, map[string]any{"url": "https://acme.slack.com/archives/" + testChannel + "/p" + strings.Replace(first.TS, ".", "", 1), "text": "deploy started at 4:10"}))
	if edits := l.slack.edited(); len(edits) != 1 || edits[0].ts != first.TS {
		t.Fatalf("edits = %+v, want the channel ticket to cover editing a top-level post", edits)
	}
	if posts := l.slack.sent(); len(posts) != 2 {
		t.Fatalf("refused posts reached Slack: %+v", posts)
	}

	l.do(opRevoke, map[string]any{"ticket_id": ticketID})
	l.refused(opSend, l.spending(ticketID, map[string]any{"channel_id": testChannel, "text": "late"}), "revoked")
}

func TestTicketIsSpentOnceAtTheWritesOwnPlace(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)

	approved := l.hooked(opSend, map[string]any{"channel_id": testChannel, "text": "CI is green"})
	reply := map[string]any{"channel_id": testChannel, "thread_ts": testThread, "text": "CI is green", "no_watch": true}
	l.refused(opReply, l.spending(field(approved, "ticket_id"), reply), "does not include this action")
	if calls := l.tickets.callCount(); calls != 1 || !slices.Contains(l.tickets.lastCall(), "thread="+testThread) {
		t.Fatalf("capt-hook calls = %q, want one spend at the reply's thread and no channel-wide retry", l.tickets.calls)
	}
	if posts := l.slack.sent(); len(posts) != 0 {
		t.Fatalf("posts = %+v, want a top-level ticket never to reach a thread", posts)
	}
}

func TestOwedUpdateTicketAnswersAfterThisDaemonsOwnPostButNotAnotherAgents(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)

	var sent postResult
	if err := json.Unmarshal(l.post(opSend, map[string]any{"channel_id": testChannel, "text": "PR is up"}).Body, &sent); err != nil {
		t.Fatal(err)
	}
	ticketID := l.owedUpdate(sent.TS)
	l.do(opReply, l.spending(ticketID, map[string]any{"channel_id": testChannel, "thread_ts": sent.TS, "text": "CI started"}))
	l.do(opReply, l.spending(ticketID, map[string]any{"channel_id": testChannel, "thread_ts": sent.TS, "text": "CI started"}))
	if posts := l.slack.sent(); len(posts) != 3 || posts[2].threadTS != sent.TS {
		t.Fatalf("posts = %+v, want consecutive ticketed replies after this daemon's own posts", posts)
	}

	l.slack.add(testChannel, slack.Message{Msg: slack.Msg{Timestamp: "1800000009.000000", ThreadTimestamp: sent.TS, User: "UBOT", BotID: "BBOT", Username: "alice", Text: "on it"}})
	l.refused(opReply, l.spending(ticketID, map[string]any{"channel_id": testChannel, "thread_ts": sent.TS, "text": "answering alice"}), "another Claude agent")
	if posts := l.slack.sent(); len(posts) != 3 {
		t.Fatalf("a ticketed reply answered another agent: %+v", posts)
	}
}

func TestOwedUpdateTicketAnswersAfterThisDaemonsOwnPostOnceItsStateIsWiped(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)

	var sent postResult
	if err := json.Unmarshal(l.post(opSend, map[string]any{"channel_id": testChannel, "text": "PR is up", "no_watch": true}).Body, &sent); err != nil {
		t.Fatal(err)
	}
	ticketID := l.owedUpdate(sent.TS)
	l.do(opReply, l.spending(ticketID, map[string]any{"channel_id": testChannel, "thread_ts": sent.TS, "text": "CI started", "no_watch": true}))
	if _, err := l.srv.DB().ExecContext(l.ctx, `DELETE FROM posts`); err != nil {
		t.Fatal(err)
	}
	l.do(opReply, l.spending(ticketID, map[string]any{"channel_id": testChannel, "thread_ts": sent.TS, "text": "CI is green", "no_watch": true}))
	if posts := l.slack.sent(); len(posts) != 3 || posts[2].threadTS != sent.TS {
		t.Fatalf("posts = %+v, want a ticketed reply after this daemon's own post once its posts table is gone", posts)
	}

	other := `{"event_type":"` + postEventType + `","event_payload":{"user_id":"U0ALICE01"}}`
	var metadata slack.SlackMetadata
	if err := json.Unmarshal([]byte(other), &metadata); err != nil {
		t.Fatal(err)
	}
	l.slack.add(testChannel, slack.Message{Msg: slack.Msg{
		Timestamp: "1800000009.000000", ThreadTimestamp: sent.TS, User: "UBOT", BotID: "BBOT", Username: "alice", Text: "on it", Metadata: metadata,
	}})
	l.refused(opReply, l.spending(ticketID, map[string]any{"channel_id": testChannel, "thread_ts": sent.TS, "text": "answering alice", "no_watch": true}), "another Claude agent")
	if posts := l.slack.sent(); len(posts) != 3 {
		t.Fatalf("a ticketed reply answered another engineer's agent: %+v", posts)
	}
}

func TestOwedUpdateTicketSeesAnotherAgentsReplyInsideTheCacheWindow(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)

	var sent postResult
	if err := json.Unmarshal(l.post(opSend, map[string]any{"channel_id": testChannel, "text": "PR is up", "no_watch": true}).Body, &sent); err != nil {
		t.Fatal(err)
	}
	ticketID := l.owedUpdate(sent.TS)
	l.do(opThread, map[string]any{"channel_id": testChannel, "thread_ts": sent.TS})
	l.slack.add(testChannel, slack.Message{Msg: slack.Msg{Timestamp: "1800000009.000000", ThreadTimestamp: sent.TS, User: "UBOT", BotID: "BBOT", Username: "alice", Text: "on it"}})
	l.refused(opReply, l.spending(ticketID, map[string]any{"channel_id": testChannel, "thread_ts": sent.TS, "text": "answering alice", "no_watch": true}), "another Claude agent")
}

func TestOwnerTicketsAnswerAnotherAgentWithoutReadingTheThread(t *testing.T) {
	for _, rule := range []string{"owner-linked", "judged", "asked-owner", "pasted", "request", "exact", "conversation"} {
		t.Run(rule, func(t *testing.T) {
			l := startDaemon(t)
			loggedIn(t, l)

			var sent postResult
			if err := json.Unmarshal(l.post(opSend, map[string]any{"channel_id": testChannel, "text": "PR is up", "no_watch": true}).Body, &sent); err != nil {
				t.Fatal(err)
			}
			l.slack.add(testChannel, slack.Message{Msg: slack.Msg{Timestamp: "1800000009.000000", ThreadTimestamp: sent.TS, User: "UBOT", BotID: "BBOT", Username: "alice", Text: "on it"}})
			reads := l.slack.callCount(methodReplies)
			l.do(opReply, l.spending(l.placeTicket(rule, sent.TS), map[string]any{"channel_id": testChannel, "thread_ts": sent.TS, "text": "answering alice", "no_watch": true}))
			if posts := l.slack.sent(); len(posts) != 2 || posts[1].threadTS != sent.TS {
				t.Fatalf("posts = %+v, want the owner's ticket to answer another agent", posts)
			}
			if n := l.slack.callCount(methodReplies); n != reads {
				t.Fatalf("conversations.replies calls = %d, want %d: only an owed update reads the thread first", n, reads)
			}
		})
	}
}

func TestOnceOwedUpdateNeverAnswersAnotherAgent(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)

	var sent postResult
	if err := json.Unmarshal(l.post(opSend, map[string]any{"channel_id": testChannel, "text": "PR is up", "no_watch": true}).Body, &sent); err != nil {
		t.Fatal(err)
	}
	l.slack.add(testChannel, slack.Message{Msg: slack.Msg{Timestamp: "1800000009.000000", ThreadTimestamp: sent.TS, User: "UBOT", BotID: "BBOT", Username: "alice", Text: "on it"}})
	update := asBody(map[string]any{"channel_id": testChannel, "thread_ts": sent.TS, "text": "CI is green", "no_watch": true})
	ticketID := l.tickets.mint(fakeTicket{tree: testTree, place: placeOf(opReply, update), reach: reachOnce, rule: owedUpdateRule, fingerprint: fingerprintOf(update)})
	l.refused(opReply, l.spending(ticketID, update), "is an owed update")
	if spends := l.tickets.committed(ticketID); len(spends) != 0 {
		t.Fatalf("committed spends = %+v, want the refused update's use handed back", spends)
	}
	if posts := l.slack.sent(); len(posts) != 1 {
		t.Fatalf("an owed update answered another agent: %+v", posts)
	}
}

func TestThreadPlaceTicketCoversRepliesEditsAndDeletesInItsThreadOnly(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)

	var parent, other, reply, elsewhere postResult
	if err := json.Unmarshal(l.post(opSend, map[string]any{"channel_id": testChannel, "text": "alert"}).Body, &parent); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(l.post(opSend, map[string]any{"channel_id": testChannel, "text": "another alert"}).Body, &other); err != nil {
		t.Fatal(err)
	}
	ticketID := l.standing(parent.TS)
	if err := json.Unmarshal(l.do(opReply, l.spending(ticketID, map[string]any{"channel_id": testChannel, "thread_ts": parent.TS, "text": "mechanism: x"})).Body, &reply); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(l.post(opReply, map[string]any{"channel_id": testChannel, "thread_ts": other.TS, "text": "on it"}).Body, &elsewhere); err != nil {
		t.Fatal(err)
	}

	l.do(opEdit, l.spending(ticketID, map[string]any{"channel_id": testChannel, "thread_ts": parent.TS, "ts": reply.TS, "text": "mechanism: y"}))
	if edits := l.slack.edited(); len(edits) != 1 || edits[0].ts != reply.TS || edits[0].text != "mechanism: y" {
		t.Fatalf("edits = %+v, want the thread ticket to cover editing a reply there", edits)
	}

	l.refused(opEdit, l.spending(ticketID, map[string]any{"channel_id": testChannel, "thread_ts": other.TS, "ts": elsewhere.TS, "text": "x"}), "does not include this action")
	l.refused(opEdit, l.spending(l.standing(""), map[string]any{"channel_id": testChannel, "ts": reply.TS, "text": "x"}), "does not include this action")
	l.refused(opSend, l.spending(ticketID, map[string]any{"channel_id": testChannel, "text": "x"}), "does not include this action")
	if edits := l.slack.edited(); len(edits) != 1 {
		t.Fatalf("refused edits reached Slack: %+v", edits)
	}
	l.do(opEdit, l.spending(ticketID, map[string]any{"channel_id": testChannel, "ts": reply.TS, "text": "mechanism: z"}))
	if edits := l.slack.edited(); len(edits) != 2 || edits[1].ts != reply.TS || edits[1].text != "mechanism: z" {
		t.Fatalf("edits = %+v, want a bare ts resolved to its thread, which the thread ticket covers", edits)
	}

	l.refused(opDelete, l.spending(ticketID, map[string]any{"channel_id": testChannel, "thread_ts": other.TS, "ts": elsewhere.TS}), "does not include this action")
	l.do(opDelete, l.spending(ticketID, map[string]any{"channel_id": testChannel, "thread_ts": parent.TS, "ts": reply.TS}))
	if deletes := l.slack.deleted(); len(deletes) != 1 || deletes[0].ts != reply.TS {
		t.Fatalf("deletes = %+v, want the thread ticket to cover deleting a reply there", deletes)
	}
}

func TestRevokeRevokesTheCaptHookTicket(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)

	ticketID := l.standing(testThread)
	var revoked ticketResult
	if err := json.Unmarshal(l.do(opRevoke, map[string]any{"ticket_id": ticketID}).Body, &revoked); err != nil {
		t.Fatal(err)
	}
	if got, want := l.tickets.lastCall(), []string{testCaptHook, "ticket", "revoke", ticketID}; !slices.Equal(got, want) || revoked.TicketID != ticketID {
		t.Fatalf("capt-hook argv = %q, want %q; result %+v", got, want, revoked)
	}
	l.refused(opRevoke, map[string]any{"ticket_id": "fffffffffff0"}, "no ticket fffffffffff0")
	l.refused(opRevoke, map[string]any{}, "revoke needs ticket_id")
}
