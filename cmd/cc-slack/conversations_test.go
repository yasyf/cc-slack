package main

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/slack-go/slack"

	"github.com/yasyf/cc-interact/agent"
	"github.com/yasyf/cc-interact/cmd"
)

const driveChannelName = "drive-ssql-duckdb"

func openDriveChannel(t *testing.T, l *liveDaemon) channelResult {
	t.Helper()
	return decodeChange(t, l.do(opChannelCreate, map[string]any{"name": driveChannelName, "private": true}))
}

func TestAPrivateChannelCreateNeedsNoTicketInvitesHimAndBecomesHisConversation(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)

	made := openDriveChannel(t, l)
	created, _, invites, _ := l.slack.channelChanges()
	if made.ChannelID != "CNEW1" || made.TicketID != "" || !slices.Equal(l.slack.privateChannels(), []string{driveChannelName}) {
		t.Fatalf("create = %+v, private %v, want %s made private with no ticket", made, l.slack.privateChannels(), driveChannelName)
	}
	if !slices.Equal(invites[made.ChannelID], []string{testUser}) || !l.app.convs.has(made.ChannelID) {
		t.Fatalf("invites %v, registered %t, want him invited and the channel kept as his conversation", invites, l.app.convs.has(made.ChannelID))
	}

	if again := openDriveChannel(t, l); again.ChannelID != made.ChannelID {
		t.Fatalf("second create = %+v, want the kept %s", again, made.ChannelID)
	}
	if after, _, _, _ := l.slack.channelChanges(); !slices.Equal(after, created) {
		t.Fatalf("created %v, want one conversations.create for one name", after)
	}
	l.refused(opChannelCreate, map[string]any{"name": "incident-2026-10-03"}, "no ticket covers this create")
}

func TestDMStatusPostsInHisPrivateChannelAndRefusesAnyOtherChannel(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	drive := openDriveChannel(t, l).ChannelID

	var posted map[string]string
	if err := json.Unmarshal(l.do(opDMStatus, map[string]string{"text": "starting", "channel_id": drive}).Body, &posted); err != nil {
		t.Fatal(err)
	}
	sent := l.slack.sent()
	if last := sent[len(sent)-1]; last.channelID != drive || last.threadTS != "" || posted["channel_id"] != drive {
		t.Fatalf("dm-status post = %+v, result %v, want it at the root of %s", last, posted, drive)
	}
	l.refused(opDMStatus, map[string]string{"text": "starting", "channel_id": testChannel}, "is not one of your conversations with the app")
}

func TestOneSessionHoldsHisPrivateChannel(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	drive := openDriveChannel(t, l).ChannelID
	other := l.deps
	other.ClaudePID = func() int { return testPID + 3 }
	follow := func(d cmd.Deps, session, channelID string) followResult {
		t.Helper()
		reply, err := invoke(l.ctx, d, session, "/any", opFollow, mustJSON(map[string]string{"channel_id": channelID}))
		if err != nil {
			t.Fatal(err)
		}
		var res followResult
		if err := json.Unmarshal(reply.Body, &res); err != nil {
			t.Fatal(err)
		}
		return res
	}

	if held := follow(l.deps, testSession, drive); held.WatchID == 0 || held.HeldBy != "" {
		t.Fatalf("first hold = %+v, want this session holding %s", held, drive)
	}
	if refused := follow(other, "other-session", drive); refused.WatchID != 0 || refused.HeldBy != testSession {
		t.Fatalf("second session's hold = %+v, want it refused and named", refused)
	}
	if dm := follow(other, "other-session", "D"+testUser); dm.WatchID == 0 || dm.HeldBy != "" {
		t.Fatalf("hold of his DM = %+v, want it free while another session holds the drive channel", dm)
	}
}

func TestHisRootMessageInHisPrivateChannelReachesItsHolderOnlyWithEyesAndIsOwedThere(t *testing.T) {
	f := newFixture(t)
	const drive = "C0DRIVE01"
	if err := f.app.convs.add(f.ctx, drive, driveChannelName, f.now); err != nil {
		t.Fatal(err)
	}
	second := secondSession(t, f)
	if _, _, err := insertWatch(f.ctx, f.db(), second.ID, agent.TopLevel, kindChannel, f.id.DMChannelID, "", beforeEveryPost, f.now); err != nil {
		t.Fatal(err)
	}
	if _, _, err := insertWatch(f.ctx, f.db(), testSubject, agent.TopLevel, kindChannel, drive, "", beforeEveryPost, f.now); err != nil {
		t.Fatal(err)
	}
	f.tick()

	const root = "1800000090.000000"
	f.slack.add(drive, slack.Message{Msg: slack.Msg{Timestamp: root, User: testUser, Text: "ship the duckdb reader"}})
	f.poller.nudged(messageNudge{Channel: drive, TS: root})
	f.tick()

	if len(f.plane.events) != 1 || f.plane.events[0].SubjectID != testSubject {
		t.Fatalf("delivered %+v, want his message only to the drive channel's holder", f.plane.events)
	}
	if !slices.Contains(f.slack.reactedTo(root), pickupReaction) {
		t.Fatalf("reactions on %s = %v, want eyes", root, f.slack.reactedTo(root))
	}
	owed, err := f.app.owed.list(f.ctx, drive)
	if err != nil {
		t.Fatal(err)
	}
	if len(owed) != 1 || owed[0].TS != root || len(owedStamps(t, f)) != 0 {
		t.Fatalf("owed in %s = %+v, in his DM %v, want the root owed in the drive channel alone", drive, owed, owedStamps(t, f))
	}
}

func TestAWaitingCardInHisPrivateChannelMentionsHimAndTakesHisTap(t *testing.T) {
	cards := fakeCardCommand(t, nil)
	l, _ := prLinksDaemon(t, map[string]any{"card_command": cards.argv})
	id, err := loadIdentity(l.ctx, l.srv.DB())
	if err != nil {
		t.Fatal(err)
	}
	drive := openDriveChannel(t, l).ChannelID
	card := statusCard{Status: "needs_you", Title: "Which store keeps the tally?", Drive: "ssql-duckdb", Waits: "ssql-duckdb:j3-661440", Options: []string{"Ring", "Table"}, Recommended: "Ring"}
	l.do(opDMStatus, map[string]any{"card": card, "channel_id": drive})
	inDM := card
	inDM.Waits = "ssql-duckdb:j3-661441"
	l.do(opDMStatus, map[string]any{"card": inDM})

	sent := l.slack.sent()
	if len(sent) != 2 || sent[0].channelID != drive || !strings.HasPrefix(sent[0].text, "<@"+testUser+"> ") || strings.Contains(sent[1].text, "<@") {
		t.Fatalf("posts = %+v, want the drive channel's card to mention him and the DM's not", sent)
	}
	l.tap(id, cardActionPush{User: testUser, Channel: drive, MessageTS: sent[0].ts, ThreadTS: sent[0].ts,
		ActionID: cardWaitAction + "0:" + card.Waits, Value: string(mustJSON(askTap{ID: card.Waits, Label: "Ring", Question: card.Title, Options: card.Options})),
		ActionTS: fmt.Sprintf("%d.000100", time.Now().Unix()), Seq: "001800000100000-abcd0411"})
	if ans, done := l.app.cards.answer(drive, sent[0].ts, card.Waits); !done || ans.Label != "Ring" {
		t.Fatalf("answer in %s = %+v, %v, want his tap kept", drive, ans, done)
	}
}
