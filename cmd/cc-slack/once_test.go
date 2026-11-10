package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/slack-go/slack"

	"github.com/yasyf/cc-interact/agent"
	"github.com/yasyf/cc-interact/consume"
	"github.com/yasyf/cc-interact/daemon"
	"github.com/yasyf/cc-interact/event"
)

type tag struct {
	seq  int64
	data map[string]any
}

func (l *liveDaemon) attachChannel(proven bool) <-chan tag {
	l.t.Helper()
	tags := make(chan tag, 64)
	ctx, cancel := context.WithCancel(l.ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = consume.ConsumeEvents(ctx, consume.StreamSource{
			Port: l.port, SubjectID: l.subID, Consumer: channelConsumer, ClaudePID: testPID, ExcludeOrigin: event.OriginAgent, Paths: appPaths(),
		}, func(seq int64, data string) (bool, error) {
			var got map[string]any
			if err := json.Unmarshal([]byte(data), &got); err != nil {
				return false, err
			}
			if got["type"] == eventMessage || got["type"] == eventAction {
				tags <- tag{seq, got}
			}
			return false, nil
		})
	}()
	l.t.Cleanup(func() {
		cancel()
		<-done
	})
	deadline := time.Now().Add(10 * time.Second)
	for {
		var status daemon.StatusBody
		if err := json.Unmarshal(l.do(daemon.OpStatus, nil).Body, &status); err != nil {
			l.t.Fatal(err)
		}
		if status.Consumers[channelConsumer] > 0 {
			break
		}
		if time.Now().After(deadline) {
			l.t.Fatal("the channel never attached")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if proven {
		l.do(daemon.OpChannelAck, nil)
	}
	return tags
}

func (l *liveDaemon) pushed(tags <-chan tag, typ string) tag {
	l.t.Helper()
	var got tag
	select {
	case got = <-tags:
	case <-time.After(30 * time.Second):
		l.t.Fatalf("the channel never pushed a %s", typ)
	}
	if got.data["type"] != typ {
		l.t.Fatalf("channel pushed %v, want a %s", got.data, typ)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		cursor, err := consume.Cursor(appPaths(), l.subID, channelConsumer)
		if err != nil {
			l.t.Fatal(err)
		}
		if cursor >= got.seq {
			return got
		}
		if time.Now().After(deadline) {
			l.t.Fatalf("the channel cursor stayed at %d, below the pushed %d", cursor, got.seq)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestEveryTopLevelKindReachesAProvenChannelOnce(t *testing.T) {
	for _, tc := range []struct {
		name    string
		typ     string
		deliver func(*testing.T, *liveDaemon)
		said    string
	}{
		{
			name: "operator message on a watched thread",
			typ:  eventMessage,
			deliver: func(t *testing.T, l *liveDaemon) {
				w := l.addWatch("1800000000.000400")
				id := identity{TeamURL: "https://acme.slack.com/", UserID: testUser, BotID: "BBOT", BotUserID: "UBOT"}
				m := slack.Message{Msg: slack.Msg{Timestamp: "1800000000.000401", ThreadTimestamp: w.ThreadTS, User: testUser, Text: "ship the fix"}}
				d, err := l.app.poller.messageDelivery(l.ctx, l.slack, id, w, m, nil, "")
				if err != nil {
					t.Fatal(err)
				}
				if err := l.app.poller.deliver(l.ctx, l.srv.DB(), w, d); err != nil {
					t.Fatal(err)
				}
			},
			said: "Act on it as operator steering",
		},
		{
			name: "tap on a reply card",
			typ:  eventAction,
			deliver: func(t *testing.T, l *liveDaemon) {
				loggedIn(t, l)
				l.do(opDMStatus, cardBody("Merge the stack?", "merge it"))
				id, err := loadIdentity(l.ctx, l.srv.DB())
				if err != nil {
					t.Fatal(err)
				}
				reply := l.slack.sent()[0].ts
				l.tap(id, cardActionPush{User: testUser, Channel: id.DMChannelID, MessageTS: reply, ThreadTS: reply,
					ActionID: cardOptionAction + "0", Value: "merge it", ActionTS: "1800000100.000000", Seq: "001800000100000-abcd0123"})
			},
			said: `they chose "merge it"`,
		},
	} {
		for _, proven := range []bool{true, false} {
			name := tc.name + ", unproven channel"
			if proven {
				name = tc.name + ", proven channel"
			}
			t.Run(name, func(t *testing.T) {
				l := startDaemon(t)
				tags := l.attachChannel(proven)
				tc.deliver(t, l)
				got := l.pushed(tags, tc.typ)
				if tc.typ == eventMessage && got.data["operator"] != true {
					t.Fatalf("tag %v, want operator set for the operator's message", got.data)
				}
				drained := l.inject(agent.TopLevel)
				if proven && drained != "" {
					t.Fatalf("hook delivered %q after the proven channel pushed the tag, want nothing", drained)
				}
				if !proven && !strings.Contains(drained, tc.said) {
					t.Fatalf("hook delivered %q, want the directive saying %q while the channel is unproven", drained, tc.said)
				}
				select {
				case again := <-tags:
					t.Fatalf("channel pushed a second %v", again.data)
				default:
				}
			})
		}
	}
}
