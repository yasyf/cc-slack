package main

import (
	"encoding/json"
	"fmt"
	"slices"
	"testing"

	"github.com/slack-go/slack"

	"github.com/yasyf/cc-interact/agent"
	"github.com/yasyf/cc-interact/cmd"
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
