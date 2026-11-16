package main

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/slack-go/slack"
)

const pushedReply = "1800000060.000300"

func TestHisMessageCarriedInTheNudgeReachesTheHolderWhileSlackLimitsReads(t *testing.T) {
	f := newFixture(t)
	holdDM(t, f)
	thread := postDMThread(t, f)
	f.tick()

	m := slack.Message{Msg: slack.Msg{Timestamp: pushedReply, ThreadTimestamp: thread, User: testUser, Text: "ship it"}}
	f.slack.fail(methodReplies, &slack.RateLimitedError{RetryAfter: 30 * time.Second})
	f.poller.nudged(messageNudge{Channel: f.id.DMChannelID, TS: pushedReply, ThreadTS: thread, Message: &m})
	f.tick()

	if n := f.slack.callCount(methodReplies); n > 1 {
		t.Fatalf("replies calls = %d, want at most the one read Slack refused", n)
	}
	if got := deliveredStamps(f); len(got) != 1 || got[0] != pushedReply {
		t.Fatalf("delivered %v, want his message %s from the nudge alone", got, pushedReply)
	}

	f.slack.add(f.id.DMChannelID, m)
	f.now = f.now.Add(time.Minute)
	f.poller.nudged(messageNudge{Channel: f.id.DMChannelID, TS: pushedReply, ThreadTS: thread})
	f.tick()
	if got := deliveredStamps(f); len(got) != 1 {
		t.Fatalf("delivered %v after the thread read, want it once", got)
	}
}

func TestAnotherPersonsMessageInANudgeIsReadFromSlack(t *testing.T) {
	f := newFixture(t)
	holdDM(t, f)
	thread := postDMThread(t, f)
	f.tick()

	m := slack.Message{Msg: slack.Msg{Timestamp: pushedReply, ThreadTimestamp: thread, User: "UOTHER", Text: "ship it"}}
	f.slack.fail(methodReplies, &slack.RateLimitedError{RetryAfter: 30 * time.Second})
	f.poller.nudged(messageNudge{Channel: f.id.DMChannelID, TS: pushedReply, ThreadTS: thread, Message: &m})
	f.tick()
	if got := deliveredStamps(f); len(got) != 0 {
		t.Fatalf("delivered %v, want nothing routed from a nudge that is not his", got)
	}
}

func deliveredStamps(f *fixture) []string {
	var stamps []string
	for _, e := range f.plane.events {
		if e.Type != eventMessage {
			continue
		}
		var msg messagePayload
		if err := json.Unmarshal(e.Payload, &msg); err == nil {
			stamps = append(stamps, msg.TS)
		}
	}
	return stamps
}
