package main

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/slack-go/slack"

	"github.com/yasyf/cc-interact/agent"
	"github.com/yasyf/cc-interact/cmd"
	"github.com/yasyf/cc-interact/daemon"
	"github.com/yasyf/cc-interact/subject"
)

const (
	beforeEveryPost = "1700000000.000000"
	stoodDownPID    = testPID + 1
	stoodDownID     = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

func holdDM(t *testing.T, f *fixture) {
	t.Helper()
	if _, _, err := insertWatch(f.ctx, f.db(), testSubject, agent.TopLevel, kindChannel, f.id.DMChannelID, "", beforeEveryPost, f.now); err != nil {
		t.Fatal(err)
	}
}

func postDMThread(t *testing.T, f *fixture) string {
	t.Helper()
	posted, err := f.app.post(f.ctx, f.db(), f.id, f.id.DMChannelID, "", "which one?", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return posted.TS
}

func ownerReplies(f *fixture, thread, ts, text string) {
	f.slack.add(f.id.DMChannelID, slack.Message{Msg: slack.Msg{Timestamp: ts, ThreadTimestamp: thread, User: testUser, Text: text}})
	f.poller.nudged(messageNudge{Channel: f.id.DMChannelID, TS: ts, ThreadTS: thread})
	f.tick()
}

func owedStamps(t *testing.T, f *fixture) []string {
	t.Helper()
	replies, err := f.app.owed.list(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	stamps := make([]string, 0, len(replies))
	for _, r := range replies {
		stamps = append(stamps, r.TS)
	}
	return stamps
}

func deliveredKinds(t *testing.T, f *fixture) []string {
	t.Helper()
	delivered := payloads(t, f)
	kinds := make([]string, 0, len(delivered))
	for _, got := range delivered {
		kinds = append(kinds, got.Kind)
	}
	slices.Sort(kinds)
	return kinds
}

func TestAReplyInALanesDMThreadReachesTheLaneAndTheHolder(t *testing.T) {
	f := newFixture(t)
	holdDM(t, f)
	thread := postDMThread(t, f)
	lane := secondSession(t, f)
	if _, _, err := insertWatch(f.ctx, f.db(), lane.ID, agent.TopLevel, kindThread, f.id.DMChannelID, thread, thread, f.now); err != nil {
		t.Fatal(err)
	}
	f.tick()

	ownerReplies(f, thread, "1800000001.500000", "A")

	if got := deliveredKinds(t, f); !slices.Equal(got, []string{kindChannel, kindThread}) {
		t.Fatalf("delivered to %v, want the holder's channel watch and the lane's thread watch", got)
	}
}

func TestAReplyInAStoodDownLanesDMThreadReachesTheHolder(t *testing.T) {
	f := newFixture(t)
	holdDM(t, f)
	thread := postDMThread(t, f)
	if _, err := storeSubjects(f).Create(f.ctx, stoodDownID, "", "sess-gone", scopeName, stoodDownPID, statusOpen); err != nil {
		t.Fatal(err)
	}
	gone, _, err := insertWatch(f.ctx, f.db(), stoodDownID, agent.TopLevel, kindThread, f.id.DMChannelID, thread, thread, f.now)
	if err != nil {
		t.Fatal(err)
	}
	f.tick()

	ownerReplies(f, thread, "1800000001.500000", "A")

	if got := deliveredKinds(t, f); !slices.Equal(got, []string{kindChannel}) {
		t.Fatalf("delivered to %v, want only the holder's channel watch", got)
	}
	if status := f.watchStatus(gone); status != "orphaned" {
		t.Fatalf("stood-down lane's watch is %q, want orphaned", status)
	}
}

func cardSessionElsewhere(t *testing.T, f *fixture) subject.Subject {
	t.Helper()
	const id, pid = "abcdef0123456789abcdef0123456789", testPID + 2
	f.alive[pid] = true
	if _, err := storeSubjects(f).Create(f.ctx, id, "", "sess-card", scopeName, pid, statusOpen); err != nil {
		t.Fatal(err)
	}
	sub, err := storeSubjects(f).Get(f.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return sub
}

func owedToHolder(t *testing.T, f *fixture) []owedReply {
	t.Helper()
	var owed dmOwedResult
	if err := json.Unmarshal(f.app.handleDMOwed(daemon.HandlerCtx{Ctx: f.ctx, DB: f.db()}).Body, &owed); err != nil {
		t.Fatal(err)
	}
	return owed.Replies
}

func TestAReplyInAnotherSessionsCardThreadReachesOnlyThatSession(t *testing.T) {
	f := newFixture(t)
	holdDM(t, f)
	thread := dmMessageOf(t, f, cardSessionElsewhere(t, f))
	if _, _, err := insertWatch(f.ctx, f.db(), testSubject, agent.TopLevel, kindThread, f.id.DMChannelID, thread, thread, f.now); err != nil {
		t.Fatal(err)
	}
	f.tick()

	ownerReplies(f, thread, "1800000001.500000", "A")

	if got := deliveredKinds(t, f); !slices.Equal(got, []string{kindDM}) {
		t.Fatalf("delivered to %v, want only the card's session", got)
	}
	if got := owedToHolder(t, f); len(got) != 0 {
		t.Fatalf("holder owes %+v, want nothing from another session's card", got)
	}
}

func TestTheHolderGetsACardThreadBackWhenItsSessionEnds(t *testing.T) {
	f := newFixture(t)
	holdDM(t, f)
	sub := cardSessionElsewhere(t, f)
	thread := dmMessageOf(t, f, sub)
	f.tick()
	f.alive[sub.ClaudePID] = false
	f.tick()

	ownerReplies(f, thread, "1800000001.500000", "A")

	if got := deliveredKinds(t, f); !slices.Equal(got, []string{kindChannel}) {
		t.Fatalf("delivered to %v, want only the holder once the card's session ended", got)
	}
	if got := owedToHolder(t, f); len(got) != 1 {
		t.Fatalf("holder owes %+v, want the reply", got)
	}
}

func TestAReplyOnACardFromTheHoldersOwnProcessStillReachesTheHolder(t *testing.T) {
	f := newFixture(t)
	holdDM(t, f)
	thread := dmMessageOf(t, f, secondSession(t, f))
	f.tick()

	ownerReplies(f, thread, "1800000001.500000", "A")

	if got := deliveredKinds(t, f); !slices.Equal(got, []string{kindChannel, kindDM}) {
		t.Fatalf("delivered to %v, want the holder and the card's earlier session in the same process", got)
	}
	if got := owedToHolder(t, f); len(got) != 1 {
		t.Fatalf("holder owes %+v, want the reply", got)
	}
}

func TestAReplyWithNoLiveSessionIsStillOwed(t *testing.T) {
	f := newFixture(t)
	thread := postDMThread(t, f)
	f.tick()

	ownerReplies(f, thread, "1800000001.500000", "A")

	if got := deliveredKinds(t, f); len(got) != 0 {
		t.Fatalf("delivered to %v with no session on the thread", got)
	}
	if got := owedStamps(t, f); !slices.Equal(got, []string{"1800000001.500000"}) {
		t.Fatalf("owed %v, want the reply no session heard", got)
	}
}

func TestAPostInTheThreadAnswersOnlyEarlierRepliesThere(t *testing.T) {
	f := newFixture(t)
	first, second := postDMThread(t, f), postDMThread(t, f)
	f.tick()
	ownerReplies(f, first, "1800000002.100000", "A")
	ownerReplies(f, second, "1800000002.200000", "B")

	if _, err := f.app.post(f.ctx, f.db(), f.id, f.id.DMChannelID, first, "Seen: A is applied.", nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := owedStamps(t, f); !slices.Equal(got, []string{"1800000002.200000"}) {
		t.Fatalf("owed %v, want only the reply in the other thread", got)
	}

	ownerReplies(f, first, "1800000003.500000", "and one more thing")
	if got := owedStamps(t, f); !slices.Equal(got, []string{"1800000002.200000", "1800000003.500000"}) {
		t.Fatalf("owed %v, want the newer reply owed again", got)
	}
}

func TestAPostAtTheDMRootAnswersNoThread(t *testing.T) {
	f := newFixture(t)
	thread := postDMThread(t, f)
	f.tick()
	ownerReplies(f, thread, "1800000001.500000", "A")

	postDMThread(t, f)

	if got := owedStamps(t, f); !slices.Equal(got, []string{"1800000001.500000"}) {
		t.Fatalf("owed %v, want the reply still owed after an unrelated card", got)
	}
}

func TestABotMessageIsNeverOwedOrDeliveredToTheHolder(t *testing.T) {
	f := newFixture(t)
	holdDM(t, f)
	thread := postDMThread(t, f)
	f.tick()
	ownerReplies(f, thread, "1800000001.500000", "A")

	const fromAnotherHost = "1800000001.600000"
	f.slack.add(f.id.DMChannelID, slack.Message{Msg: slack.Msg{
		Timestamp: fromAnotherHost, ThreadTimestamp: thread, SubType: "bot_message", User: f.id.BotUserID, BotID: f.id.BotID, Username: "Yasyf (claude)", Text: "Seen: A is applied.",
	}})
	f.poller.nudged(messageNudge{Channel: f.id.DMChannelID, TS: fromAnotherHost, ThreadTS: thread, Subtype: "bot_message"})
	f.tick()

	if got := owedStamps(t, f); len(got) != 0 {
		t.Fatalf("owed %v, want the bot's answer to clear the reply and owe nothing itself", got)
	}
	if got := payloads(t, f); len(got) != 1 || got[0].TS != "1800000001.500000" {
		t.Fatalf("holder got %+v, want only the owner's reply", got)
	}
}

func TestAnOwnersDMRootMessageGoesToTheHolderWithoutASessionList(t *testing.T) {
	f := newFixture(t)
	dm := f.id.DMChannelID
	holdDM(t, f)
	dmMessageOf(t, f, subjectFor(t, f))
	dmMessageOf(t, f, secondSession(t, f))
	f.tick()
	sent := len(f.slack.sent())

	const root = "1800000090.000000"
	f.slack.add(dm, slack.Message{Msg: slack.Msg{Timestamp: root, User: testUser, Text: "which one?"}})
	f.poller.nudged(messageNudge{Channel: dm, TS: root})
	f.tick()

	if got := f.slack.sent(); len(got) != sent {
		t.Fatalf("posted %+v, want no auto-reply and no session list when a session holds the DM", got[sent:])
	}
	if got := deliveredKinds(t, f); !slices.Equal(got, []string{kindChannel}) {
		t.Fatalf("delivered to %v, want only the holder", got)
	}
	if got := owedStamps(t, f); !slices.Equal(got, []string{root}) {
		t.Fatalf("owed %v, want the root message owed", got)
	}
}

func TestOneReplyCostsOneReadWhateverTheNumberOfDMThreads(t *testing.T) {
	for _, threads := range []int{1, 1000} {
		t.Run(fmt.Sprint(threads), func(t *testing.T) {
			f := newFixture(t)
			dm := f.id.DMChannelID
			holdDM(t, f)
			for i := range threads {
				f.slack.add(dm, slack.Message{Msg: slack.Msg{Timestamp: fmt.Sprintf("1700000001.%06d", i), User: "UBOT", BotID: f.id.BotID, Text: "a card"}})
			}
			f.tick()
			replies, history := f.slack.callCount(methodReplies), f.slack.callCount(methodHistory)

			ownerReplies(f, "1700000001.000000", "1800000001.500000", "A")
			if got := owedStamps(t, f); !slices.Equal(got, []string{"1800000001.500000"}) {
				t.Fatalf("owed %v, want the one reply", got)
			}

			if n := f.slack.callCount(methodReplies) - replies; n != 1 {
				t.Fatalf("%d thread reads at %d threads, want 1", n, threads)
			}
			if n := f.slack.callCount(methodHistory) - history; n != 0 {
				t.Fatalf("%d history reads at %d threads, want 0", n, threads)
			}
		})
	}
}

func TestOneSessionHoldsTheDM(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	dm := "D" + testUser
	l.slack.add(dm, slack.Message{Msg: slack.Msg{Timestamp: testThread, Text: "parent"}})
	other := l.deps
	other.ClaudePID = func() int { return testPID + 3 }
	follow := func(d cmd.Deps, session string) followResult {
		t.Helper()
		reply, err := invoke(l.ctx, d, session, "/any", opFollow, mustJSON(map[string]string{"channel_id": dm}))
		if err != nil {
			t.Fatal(err)
		}
		var res followResult
		if err := json.Unmarshal(reply.Body, &res); err != nil {
			t.Fatal(err)
		}
		return res
	}
	holder := func() string {
		t.Helper()
		var owed dmOwedResult
		if err := json.Unmarshal(l.do(opDMOwed, map[string]any{}).Body, &owed); err != nil {
			t.Fatal(err)
		}
		return owed.HolderSession
	}

	if got := holder(); got != "" {
		t.Fatalf("holder before any hold = %q, want none", got)
	}
	held := follow(l.deps, testSession)
	if held.WatchID == 0 || held.HeldBy != "" || holder() != testSession {
		t.Fatalf("first hold = %+v, holder %q, want this session holding", held, holder())
	}
	if refused := follow(other, "other-session"); refused.WatchID != 0 || refused.HeldBy != testSession || holder() != testSession {
		t.Fatalf("second session's hold = %+v, holder %q, want it refused and named", refused, holder())
	}
	if again := follow(l.deps, testSession); again.WatchID != held.WatchID {
		t.Fatalf("holder's next prompt made watch %d, want its own %d kept", again.WatchID, held.WatchID)
	}

	l.do(opUnfollow, map[string]int64{"watch_id": held.WatchID})

	if taken := follow(other, "other-session"); taken.WatchID == 0 || holder() != "other-session" {
		t.Fatalf("hold after the holder left = %+v, holder %q, want the other session holding", taken, holder())
	}
}

func TestForgettingAThreadKeepsOtherThreadsOwed(t *testing.T) {
	f := newFixture(t)
	holdDM(t, f)
	thread := postDMThread(t, f)
	kept := postDMThread(t, f)
	f.tick()
	ownerReplies(f, thread, "1800000090.000000", "A")
	ownerReplies(f, kept, "1800000090.000100", "B")

	if err := f.app.owed.forget(f.ctx, thread); err != nil {
		t.Fatal(err)
	}

	if got := owedStamps(t, f); !slices.Equal(got, []string{"1800000090.000100"}) {
		t.Fatalf("owed = %v, want only the other thread still owed", got)
	}
}

func TestAReplyDrawsNoAutoReply(t *testing.T) {
	for name, hold := range map[string]func(*testing.T, *fixture){"held": holdDM, "never held": func(*testing.T, *fixture) {}} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			hold(t, f)
			thread := postDMThread(t, f)
			f.tick()
			sent := len(f.slack.sent())

			ownerReplies(f, thread, "1800000001.500000", "A")

			if got := f.slack.sent(); len(got) != sent {
				t.Fatalf("posted %+v, want the daemon to post nothing of its own", got[sent:])
			}
			if got := owedStamps(t, f); !slices.Equal(got, []string{"1800000001.500000"}) {
				t.Fatalf("owed %v, want the reply still owed", got)
			}
		})
	}
}

func TestAThumbsUpOnADMReplyAnswersOnlyThatReply(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	dm := "D" + testUser
	const seen, liked, other = "1800000090.000000", "1800000090.000100", "1800000090.000200"
	for _, ts := range []string{seen, liked, other} {
		if err := l.app.owed.owe(l.ctx, owedReply{ThreadTS: testThread, TS: ts, Text: "A"}); err != nil {
			t.Fatal(err)
		}
	}

	l.do(opReact, map[string]string{"channel_id": dm, "ts": seen, "name": "eyes"})
	l.do(opReact, map[string]string{"channel_id": dm, "ts": liked, "name": ":+1:"})
	l.do(opReact, map[string]string{"channel_id": testChannel, "ts": other, "name": "+1"})

	replies, err := l.app.owed.list(l.ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(replies))
	for _, r := range replies {
		got = append(got, r.TS)
	}
	if !slices.Equal(got, []string{seen, other}) {
		t.Fatalf("owed = %v, want only the thumbs-up in the DM to answer its reply", got)
	}
}

func TestAReplyUnderAnotherSessionsCardTellsTheHolderWhoseItIs(t *testing.T) {
	f := newFixture(t)
	holdDM(t, f)
	thread := postDMThread(t, f)
	lane := secondSession(t, f)
	if err := f.app.openSessionThread(f.ctx, f.db(), lane, f.id.DMChannelID, thread); err != nil {
		t.Fatal(err)
	}
	f.app.cards.threads[threadKey(f.id.DMChannelID, thread)] = &liveThread{Channel: f.id.DMChannelID, Thread: thread,
		Waiting: map[string]*liveMessage{"guardduty": {TS: thread, Card: statusCard{Title: "Try it once?", Waits: "guardduty"}}}}
	f.tick()

	ownerReplies(f, thread, "1800000001.500000", "tag the engines instead")

	var held *messagePayload
	for _, got := range payloads(t, f) {
		if got.Kind == kindChannel {
			held = &got
		}
	}
	want := threadOwner{Session: "sess-2", Card: "guardduty", Title: "Try it once?"}
	if held == nil || held.ThreadOwner == nil || *held.ThreadOwner != want {
		t.Fatalf("holder's payload = %+v, want thread owner %+v", held, want)
	}
	for _, d := range f.plane.directives {
		if d.subjectID == testSubject && !strings.Contains(d.text, `the card "Try it once?" (card guardduty), which session sess-2 posted`) {
			t.Fatalf("holder's directive = %q, want it to name the card and the session that owns it", d.text)
		}
	}
}

func TestAReplyUnderAnEndedSessionsCardTellsTheHolderItEnded(t *testing.T) {
	f := newFixture(t)
	holdDM(t, f)
	thread := postDMThread(t, f)
	if _, err := storeSubjects(f).Create(f.ctx, stoodDownID, "", "sess-gone", scopeName, stoodDownPID, statusOpen); err != nil {
		t.Fatal(err)
	}
	gone, err := storeSubjects(f).Get(f.ctx, stoodDownID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.app.openSessionThread(f.ctx, f.db(), gone, f.id.DMChannelID, thread); err != nil {
		t.Fatal(err)
	}
	f.tick()

	ownerReplies(f, thread, "1800000001.500000", "A")

	got := payloads(t, f)
	if len(got) != 1 || got[0].ThreadOwner == nil || *got[0].ThreadOwner != (threadOwner{Session: "sess-gone", Ended: true}) {
		t.Fatalf("delivered %+v, want the reply on the holder's watch naming the ended session", got)
	}
	if text := f.plane.directives[0].text; !strings.Contains(text, "session sess-gone posted; that session has ended") {
		t.Fatalf("holder's directive = %q, want it to say the owning session ended", text)
	}
}

func TestAReplyUnderTheHoldersOwnCardNamesNoOtherOwner(t *testing.T) {
	f := newFixture(t)
	holdDM(t, f)
	thread := postDMThread(t, f)
	if err := f.app.openSessionThread(f.ctx, f.db(), subjectFor(t, f), f.id.DMChannelID, thread); err != nil {
		t.Fatal(err)
	}
	f.tick()

	ownerReplies(f, thread, "1800000001.500000", "A")

	got := payloads(t, f)
	if len(got) != 1 || got[0].ThreadOwner != nil {
		t.Fatalf("delivered %+v, want the reply once with no other owner", got)
	}
}

func TestAReplyUnderTheHoldersOwnCardNamesTheCard(t *testing.T) {
	f := newFixture(t)
	holdDM(t, f)
	thread := postDMThread(t, f)
	if err := f.app.openSessionThread(f.ctx, f.db(), subjectFor(t, f), f.id.DMChannelID, thread); err != nil {
		t.Fatal(err)
	}
	f.app.cards.threads[threadKey(f.id.DMChannelID, thread)] = &liveThread{Channel: f.id.DMChannelID, Thread: thread,
		Waiting: map[string]*liveMessage{"card-chat": {TS: thread, Card: statusCard{Title: "Put Chat live?", Waits: "card-chat"}}}}
	f.tick()

	ownerReplies(f, thread, "1800000001.500000", "looks good, but why the outbox?")

	got := payloads(t, f)
	if len(got) != 1 || got[0].ThreadOwner != nil || got[0].ThreadCard != "card-chat" {
		t.Fatalf("delivered %+v, want the reply naming card-chat and no other owner", got)
	}
}

func TestAReplyUnderACardCopyNamesTheCardItCopies(t *testing.T) {
	f := newFixture(t)
	holdDM(t, f)
	copied := postDMThread(t, f)
	if err := f.app.openSessionThread(f.ctx, f.db(), subjectFor(t, f), f.id.DMChannelID, copied); err != nil {
		t.Fatal(err)
	}
	const card = "1800000000.100000"
	f.app.cards.threads[threadKey(f.id.DMChannelID, card)] = &liveThread{Channel: f.id.DMChannelID, Thread: card,
		Waiting: map[string]*liveMessage{"card-chat": {TS: card, Card: statusCard{Title: "Put Chat live?", Waits: "card-chat"}}}}
	f.app.cards.threads[threadKey(f.id.DMChannelID, copied)] = &liveThread{Channel: f.id.DMChannelID, Thread: copied,
		Copies: []liveCardCopy{{Card: card, TS: copied}}}
	f.tick()

	ownerReplies(f, copied, "1800000001.500000", "approved, but confirm the shared code")

	got := payloads(t, f)
	if len(got) != 1 || got[0].ThreadCard != "card-chat" {
		t.Fatalf("delivered %+v, want the reply naming the card its thread copies", got)
	}
}

func TestABareFileShareAnswersNoOwedReply(t *testing.T) {
	f := newFixture(t)
	id := f.id
	id.BotID = "BPICTURES"
	if err := f.app.owed.owe(f.ctx, owedReply{ThreadTS: testThread, TS: "1800000000.000100", Text: "the images are broken", Permalink: "https://slack/p1"}); err != nil {
		t.Fatal(err)
	}
	owedCount := func() int {
		replies, err := f.app.owed.list(f.ctx)
		if err != nil {
			t.Fatal(err)
		}
		return len(replies)
	}
	share := slack.Message{Msg: slack.Msg{Timestamp: "1800000000.000200", ThreadTimestamp: testThread, BotID: id.BotID, Files: []slack.File{{ID: "Fa"}}}}
	if err := f.app.poller.trackOwed(f.ctx, id, share); err != nil {
		t.Fatal(err)
	}
	if n := owedCount(); n != 1 {
		t.Fatalf("owed = %d after a picture share, want the reply still owed", n)
	}
	answer := slack.Message{Msg: slack.Msg{Timestamp: "1800000000.000300", ThreadTimestamp: testThread, BotID: id.BotID, Text: "fixed"}}
	if err := f.app.poller.trackOwed(f.ctx, id, answer); err != nil {
		t.Fatal(err)
	}
	if n := owedCount(); n != 0 {
		t.Fatalf("owed = %d after a text answer, want none", n)
	}
}
