package main

import (
	"fmt"
	"strings"

	"github.com/slack-go/slack"
	"github.com/yasyf/cc-interact/daemon"
)

const (
	dashboardEventType = "cc_slack_dashboard_words"
	dashboardMark      = "_You, on the dashboard:_ "
)

func (a *app) handleDMRelay(hc daemon.HandlerCtx) daemon.Reply {
	b, err := decodeBody(hc)
	if err != nil {
		return fail(err.Error())
	}
	words := strings.TrimSpace(b.Text)
	if words == "" {
		return fail("dm-relay needs text, the words typed on the dashboard")
	}
	id, err := loadIdentity(hc.Ctx, hc.DB)
	if err != nil {
		return fail(err.Error())
	}
	if b.URL != "" && (b.ChannelID != id.DMChannelID || b.TS == "") {
		return fail(fmt.Sprintf("dm-relay url %q is not a message in your DM with the app", b.URL))
	}
	metadata := slack.MsgOptionMetadata(slack.SlackMetadata{EventType: dashboardEventType, EventPayload: map[string]any{"user_id": id.UserID, "text": words}})
	ch, ts, err := publish(hc.Ctx, a.poller.newAPI(id.Token), nil, id, id.DMChannelID, b.ThreadTS, dashboardMark+mrkdwnEscapes.Replace(words), nil, nil, metadata)
	if err != nil {
		return fail(err.Error())
	}
	a.wrote(hc.Ctx, ch, b.ThreadTS)
	a.poller.nudged(messageNudge{Channel: ch, TS: ts, ThreadTS: b.ThreadTS})
	return ok(postResult{ChannelID: ch, TS: ts, ThreadTS: b.ThreadTS, Permalink: id.permalink(ch, ts, b.ThreadTS), Posted: true})
}

func dashboardWords(id identity, m slack.Message) slack.Message {
	if !id.botPosted(m) || m.Metadata.EventType != dashboardEventType || m.Metadata.EventPayload["user_id"] != id.UserID {
		return m
	}
	words, _ := m.Metadata.EventPayload["text"].(string)
	m.User, m.BotID, m.SubType, m.Text, m.Blocks, m.Username = id.UserID, "", "", words, slack.Blocks{}, ""
	return m
}

func typedOnDashboard(m slack.Message) bool {
	return m.Metadata.EventType == dashboardEventType
}
