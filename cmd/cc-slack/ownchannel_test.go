package main

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/slack-go/slack"
	"github.com/yasyf/cc-interact/agent"
)

func ownChannelOf(t *testing.T, l *liveDaemon, name string) ownChannelResult {
	t.Helper()
	var got ownChannelResult
	if err := json.Unmarshal(l.do(opOwnChannel, map[string]string{"name": name}).Body, &got); err != nil {
		t.Fatal(err)
	}
	return got
}

func TestOwnChannelMakesAPrivateChannelOfHimAndTheBotThenFindsIt(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	user := l.user.as(testUser)
	l.slack.setPrivate("GNEW1", testUser, "UBOT")

	made := ownChannelOf(t, l, "drive-ssql-duckdb")
	if made.ChannelID != "GNEW1" || !made.Created || made.Name != "drive-ssql-duckdb" {
		t.Fatalf("own-channel = %+v, want the private channel it made", made)
	}
	if _, _, invites, _ := user.channelChanges(); !slices.Equal(invites["GNEW1"], []string{"UBOT"}) {
		t.Fatalf("invites = %v, want the bot added to the private channel as him", invites)
	}

	taken := slack.Channel{}
	taken.ID, taken.Name, taken.IsPrivate = "GNEW1", "drive-ssql-duckdb", true
	user.list(taken)
	user.fail("conversations.create", slack.SlackErrorResponse{Err: "name_taken"})
	if again := ownChannelOf(t, l, "drive-ssql-duckdb"); again.ChannelID != "GNEW1" || again.Created {
		t.Fatalf("own-channel again = %+v, want the channel it made before", again)
	}

	l.slack.setPrivate("GNEW1", testUser, "UBOT", "UOTHER")
	user.fail("conversations.create", slack.SlackErrorResponse{Err: "name_taken"})
	l.refused(opOwnChannel, map[string]string{"name": "drive-ssql-duckdb"}, "holds people besides you")
}

func TestAPrivateChannelOfHimAndTheBotServesAsHisDM(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	l.slack.setPrivate("GOWN", testUser, "UBOT")
	l.slack.setPrivate("GSHARED", testUser, "UBOT", "UOTHER")

	body := cardBody("Merge the stack?", "merge it")
	body["channel_id"] = "GOWN"
	l.do(opDMStatus, body)
	posted := l.slack.sentTo("GOWN")
	if len(posted) != 1 || len(l.slack.sentTo("D"+testUser)) != 0 {
		t.Fatalf("posts = %+v, want the card in his private channel alone", l.slack.sent())
	}
	shared := cardBody("Merge the stack?", "merge it")
	shared["channel_id"] = "GSHARED"
	l.refused(opDMStatus, shared, "is neither")

	l.do(opFollow, map[string]string{"channel_id": "GOWN"})
	id, err := loadIdentity(l.ctx, l.srv.DB())
	if err != nil {
		t.Fatal(err)
	}
	l.tap(id, cardActionPush{User: testUser, Channel: "GOWN", MessageTS: posted[0].ts, ThreadTS: posted[0].ts,
		ActionID: cardOptionAction + "0", Value: "merge it", ActionTS: "1800000100.000000", Seq: "001800000100000-abcd0123"})
	if got := l.inject(agent.TopLevel); !strings.Contains(got, `they chose "merge it"`) {
		t.Fatalf("directive = %q, want his tap in his private channel to reach the session holding it", got)
	}
	l.tap(id, cardActionPush{User: testUser, Channel: "GSHARED", MessageTS: posted[0].ts, ThreadTS: posted[0].ts,
		ActionID: cardOptionAction + "0", Value: "merge it", ActionTS: "1800000101.000000", Seq: "001800000101000-abcd0124"})
	if got := l.inject(agent.TopLevel); got != "" {
		t.Fatalf("directive = %q, want a tap in a channel others are in dropped", got)
	}

	for channel, want := range map[string]dmPeerResult{
		"GOWN":    {ChannelID: "GOWN", Peer: testUser, PeerName: "yasyf", PeerNames: []string{"Yasyf Mohamedali", "yasyf", "ym"}, Own: true},
		"GSHARED": {ChannelID: "GSHARED"},
	} {
		var got dmPeerResult
		if err := json.Unmarshal(l.do(opDMPeer, map[string]string{"channel_id": channel}).Body, &got); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("dm-peer %s = %+v, want %+v", channel, got, want)
		}
	}
}
