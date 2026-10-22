package main

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/slack-go/slack"

	"github.com/yasyf/cc-interact/daemon"
)

func decodeChange(t *testing.T, reply daemon.Reply) channelResult {
	t.Helper()
	var res channelResult
	if err := json.Unmarshal(reply.Body, &res); err != nil {
		t.Fatal(err)
	}
	return res
}

func TestChannelChangesRunUnderTheirOwnTicket(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)

	create := map[string]any{"name": "incident-2026-10-03"}
	l.refused(opChannelCreate, create, "no ticket covers this create")
	approved := l.hooked(opChannelCreate, create)
	made := decodeChange(t, l.do(opChannelCreate, approved))
	if made.ChannelID != "CNEW1" || made.Permalink != "https://acme.slack.com/archives/CNEW1" || made.TicketID == "" || made.Replayed {
		t.Fatalf("create = %+v, want CNEW1 created under its ticket", made)
	}
	if again := decodeChange(t, l.do(opChannelCreate, approved)); !again.Replayed || again.ChannelID != made.ChannelID {
		t.Fatalf("repeated create = %+v, want the receipt replayed", again)
	}

	invited := decodeChange(t, l.post(opChannelInvite, map[string]any{"channel_id": made.ChannelID, "users": []string{"U0SECOND", "U0FIRST"}}))
	topic := "Live doc: https://docs.example.com/incidents/2026-10-03"
	l.post(opChannelTopic, map[string]any{"channel_id": made.ChannelID, "topic": topic})
	l.post(opChannelArchive, map[string]any{"url": made.Permalink})

	created, archived, invites, topics := l.slack.channelChanges()
	if !slices.Equal(created, []string{"incident-2026-10-03"}) || !slices.Equal(archived, []string{made.ChannelID}) {
		t.Fatalf("created %v, archived %v, want one channel created once and archived", created, archived)
	}
	if invited.ChannelID != made.ChannelID || !slices.Equal(invites[made.ChannelID], []string{"U0FIRST", "U0SECOND"}) || topics[made.ChannelID] != topic {
		t.Fatalf("invites %v, topics %v, want both people invited and the topic set", invites, topics)
	}
}

func TestChannelJoinAddsTheBotToPublicAndPrivateChannels(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	user := l.user.as(testUser)

	l.refused(opChannelJoin, map[string]any{"channel_id": testChannel}, "no ticket covers this join")
	public := decodeChange(t, l.post(opChannelJoin, map[string]any{"channel_id": testChannel}))
	if public.ChannelID != testChannel || !slices.Equal(l.slack.joinedChannels(), []string{testChannel}) {
		t.Fatalf("join = %+v, joined %v, want the bot to join %s itself", public, l.slack.joinedChannels(), testChannel)
	}
	if n := user.callCount("conversations.invite"); n != 0 {
		t.Fatalf("conversations.invite calls = %d, want none for a public channel", n)
	}

	const private = "C0PRIVATE1"
	l.slack.fail("conversations.join", slack.SlackErrorResponse{Err: "method_not_supported_for_channel_type"})
	added := decodeChange(t, l.post(opChannelJoin, map[string]any{"channel_id": private}))
	if _, _, invites, _ := user.channelChanges(); added.ChannelID != private || !slices.Equal(invites[private], []string{"UBOT"}) {
		t.Fatalf("join = %+v, invites as the user %v, want the bot invited to %s", added, invites, private)
	}
}

func TestChannelChangesNeedTheirArguments(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)

	l.refused(opChannelCreate, map[string]any{}, "channel-create needs name")
	l.refused(opChannelArchive, map[string]any{}, "channel-archive needs channel_id")
	l.refused(opChannelInvite, map[string]any{"channel_id": testChannel}, "channel-invite needs users")
	l.refused(opChannelTopic, map[string]any{"channel_id": testChannel}, "channel-topic needs topic")
	l.refused(opChannelJoin, map[string]any{}, "channel-join needs channel_id")
}
