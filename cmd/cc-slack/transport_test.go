package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/slack-go/slack"
)

func transportFixture(t *testing.T, subscriber bool) *fixture {
	t.Helper()
	f := newFixture(t)
	f.id.Config.Transport.Subscriber = subscriber
	if err := saveIdentity(f.ctx, f.plane.st.DB(), f.id); err != nil {
		t.Fatal(err)
	}
	return f
}

func pushFixture(t *testing.T, f *fixture, name string) {
	t.Helper()
	frame := strings.ReplaceAll(string(protocolFixture(t, name)), "U0123ABCDE", testUser)
	if err := f.app.socket.onFrame(f.ctx, f.id, []byte(frame)); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
}

type transportClient struct {
	conn net.Conn
	in   *bufio.Reader
}

func dialTransport(t *testing.T, f *fixture, req transportRequest) *transportClient {
	t.Helper()
	client, server := net.Pipe()
	go f.app.transportConn(f.ctx, server)
	t.Cleanup(func() { _ = client.Close() })
	line, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Write(append(line, '\n')); err != nil {
		t.Fatal(err)
	}
	return &transportClient{conn: client, in: bufio.NewReader(client)}
}

func (c *transportClient) read(t *testing.T, into any) {
	t.Helper()
	line, err := c.in.ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(line, into); err != nil {
		t.Fatalf("%s: %v", line, err)
	}
}

func (c *transportClient) ack(t *testing.T, id string) {
	t.Helper()
	if _, err := c.conn.Write([]byte(`{"ack":"` + id + `"}` + "\n")); err != nil {
		t.Fatal(err)
	}
}

func waitUnacked(t *testing.T, f *fixture, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		rows, _, err := f.app.transport.unacked(f.ctx, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) == n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d events unacked, want %d", len(rows), n)
		}
		runtime.Gosched()
	}
}

func subscribed(t *testing.T, f *fixture) *transportClient {
	t.Helper()
	c := dialTransport(t, f, transportRequest{Op: "subscribe"})
	var hello transportReply
	c.read(t, &hello)
	if !hello.OK {
		t.Fatalf("subscribe answered %+v", hello)
	}
	return c
}

func TestTransportHandsEveryTapEditDeleteAndReactionOfHisToTheSubscriberUntilAcked(t *testing.T) {
	t.Parallel()
	f := transportFixture(t, true)
	for _, name := range []string{"tap.json", "tap_wrote.json", "message_changed_own.json", "message_deleted_own.json", "reaction.json"} {
		pushFixture(t, f, name)
	}
	want := []transportEvent{
		{ID: "tap:C0123ABCDE/1800000002.123456/lane:answer:dec-7:1", Kind: "tap", User: testUser, Channel: "C0123ABCDE", TS: "1800000000.000300", ThreadTS: "1800000000.000100", ActionID: "lane:answer:dec-7:1", Value: "Merge", ActionTS: "1800000002.123456"},
		{ID: "tap:C0123ABCDE/1800000002.123000/lane:wrote:dec-7", Kind: "tap", User: testUser, Channel: "C0123ABCDE", TS: "1800000000.000300", ThreadTS: "1800000000.000100", ActionID: "lane:wrote:dec-7", Value: `{"id":"dec-7","label":"Merge after the freeze","q":"Merge #41 now?","o":["Hold","Merge"]}`, ActionTS: "1800000002.123000"},
		{ID: "edit:C0123ABCDE/1800000000.000200/1800000004.000100", Kind: "edit", User: testUser, Channel: "C0123ABCDE", TS: "1800000000.000200", ThreadTS: "1800000000.000100", Text: "ship it today", Previous: "ship it"},
		{ID: "delete:C0123ABCDE/1800000000.000200", Kind: "delete", User: testUser, Channel: "C0123ABCDE", TS: "1800000000.000200", ThreadTS: "1800000000.000100", Previous: "ship it"},
		{ID: "reaction:C0123ABCDE/1800000000.000200/thumbsup/+/1800000003.000100", Kind: "reaction", User: testUser, Channel: "C0123ABCDE", TS: "1800000000.000200", Reaction: "thumbsup", Added: true},
	}
	first := subscribed(t, f)
	for i, w := range want {
		var got transportEvent
		first.read(t, &got)
		w.Link = f.id.permalink(w.Channel, w.TS, w.ThreadTS)
		if got != w {
			t.Fatalf("event %d = %+v, want %+v", i, got, w)
		}
		if i < 2 {
			first.ack(t, got.ID)
		}
	}
	waitUnacked(t, f, 3)
	second := subscribed(t, f)
	var resent transportEvent
	second.read(t, &resent)
	if resent.ID != want[2].ID {
		t.Fatalf("a new subscriber got %s first, want the first unacked event %s", resent.ID, want[2].ID)
	}
	if _, err := first.in.ReadBytes('\n'); err == nil {
		t.Fatal("the replaced subscriber's connection stayed open")
	}
	pushFixture(t, f, "tap.json")
	rows, _, err := f.app.transport.unacked(f.ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("after a replayed tap that was already acked, %d events wait, want 3", len(rows))
	}
}

func TestTransportKeepsATeammatesMessageButNoneOfTheirTapsOrReactionsAndNothingWithTheSwitchOff(t *testing.T) {
	t.Parallel()
	on := transportFixture(t, true)
	if err := on.app.transport.keep(on.ctx, on.id, reactionEvent(on.id, reactionPush{User: "UOTHER", Reaction: "eyes", Channel: "C1", TS: "1.0", Added: true})); err != nil {
		t.Fatal(err)
	}
	teammate := heardEvent(on.id, "C1", "", slack.Message{Msg: slack.Msg{User: "UOTHER", Timestamp: "2.0", Text: "is it back?"}})
	if err := on.app.transport.keep(on.ctx, on.id, teammate); err != nil {
		t.Fatal(err)
	}
	if rows, bodies, err := on.app.transport.unacked(on.ctx, 0); err != nil || len(rows) != 1 || !strings.Contains(bodies[0], `"user":"UOTHER"`) {
		t.Fatalf("a teammate's message kept as %v (%v), want it alone", bodies, err)
	}
	if err := on.app.transport.ack(on.ctx, teammate.ID); err != nil {
		t.Fatal(err)
	}
	off := transportFixture(t, false)
	pushFixture(t, off, "tap.json")
	for _, f := range []*fixture{on, off} {
		if rows, _, err := f.app.transport.unacked(f.ctx, 0); err != nil || len(rows) != 0 {
			t.Fatalf("kept %d events (%v), want none", len(rows), err)
		}
	}
	var refused transportReply
	dialTransport(t, off, transportRequest{Op: "subscribe"}).read(t, &refused)
	if refused.OK || !strings.Contains(refused.Error, "subscriber is off") {
		t.Fatalf("subscribe with the switch off answered %+v", refused)
	}
}

const transportButtons = `{"type":"actions","block_id":"dec-7","elements":[{"type":"button","action_id":"lane:answer:dec-7:1","text":{"type":"plain_text","text":"Merge"},"value":"Merge"}]}`

func TestTransportDryRunReturnsTheFinalBlocksSenderAndTargetAndPostsNothing(t *testing.T) {
	t.Parallel()
	f := transportFixture(t, true)
	req := transportRequest{Op: "reply", Channel: "C0LANE", ThreadTS: "1800000000.000100", Text: "Merge #41?", Blocks: []json.RawMessage{json.RawMessage(transportButtons)}, Sender: "claude · durable-agents", DryRun: true}
	var got transportReply
	dialTransport(t, f, req).read(t, &got)
	if !got.OK || !got.DryRun || got.Channel != "C0LANE" || got.ThreadTS != req.ThreadTS || got.Sender != req.Sender || got.Text != req.Text {
		t.Fatalf("dry run answered %+v", got)
	}
	if !strings.Contains(string(got.Blocks), `"action_id":"lane:answer:dec-7:1"`) {
		t.Fatalf("dry run blocks %s lost the caller's action id", got.Blocks)
	}
	if len(f.slack.posts) != 0 || len(f.slack.edits) != 0 {
		t.Fatalf("a dry run wrote to Slack: %d posts, %d edits", len(f.slack.posts), len(f.slack.edits))
	}
}

func TestTransportSendPostsUnderItsSenderLabelOncePerKey(t *testing.T) {
	t.Parallel()
	f := transportFixture(t, true)
	req := transportRequest{Op: "send", Channel: "C0LANE", Text: "Merge #41?", Blocks: []json.RawMessage{json.RawMessage(transportButtons)}, Sender: "claude · durable-agents", Key: "post-12"}
	var first, again transportReply
	dialTransport(t, f, req).read(t, &first)
	dialTransport(t, f, req).read(t, &again)
	if !first.OK || first.Replayed || !again.OK || !again.Replayed || again.TS != first.TS || again.Channel != first.Channel {
		t.Fatalf("first send %+v, resend %+v; want one post and its ts again", first, again)
	}
	if len(f.slack.posts) != 1 {
		t.Fatalf("%d posts, want 1", len(f.slack.posts))
	}
	if p := f.slack.posts[0]; p.username != req.Sender || !strings.Contains(p.blocks, "lane:answer:dec-7:1") {
		t.Fatalf("posted as %q with blocks %s", p.username, p.blocks)
	}
}

func nudgedEvent(t *testing.T, name string) transportEvent {
	t.Helper()
	var push socketPush
	var n messageNudge
	if err := json.Unmarshal(protocolFixture(t, name), &push); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(push.EventPayload, &n); err != nil {
		t.Fatal(err)
	}
	e, ok := nudgeEvent(identity{TeamURL: "https://acme.slack.com/"}, n)
	if !ok {
		t.Fatalf("%s made no event", name)
	}
	return e
}

func TestTransportEventsEncodeAsTheSubscriberReadsThem(t *testing.T) {
	t.Parallel()
	const user, dm, channel = "U0123ABCDE", "D0123ABCDE", "C0123ABCDE"
	id := identity{TeamURL: "https://acme.slack.com/"}
	events := []transportEvent{
		heardEvent(id, dm, "1800000000.000100", slack.Message{Msg: slack.Msg{User: user, Timestamp: "1800000000.000200", Text: "ship it"}}),
		heardEvent(id, dm, "", slack.Message{Msg: slack.Msg{User: user, Timestamp: "1800000001.000100", SubType: "file_share"}}),
		tapEvent(id, cardActionPush{User: user, Channel: channel, MessageTS: "1800000000.000300", ThreadTS: "1800000000.000100", ActionID: "ccu:dec-7:1", Value: "Merge", ActionTS: "1800000002.123456"}),
		tapEvent(id, cardActionPush{User: user, Channel: channel, MessageTS: "1800000000.000300", ThreadTS: "1800000000.000300", ActionID: "ccu:dec-7:undo", ActionTS: "1800000002.200000"}),
		tapEvent(id, cardActionPush{User: user, Channel: channel, MessageTS: "1800000000.000300", ActionID: "ccu:wrote:dec-7", Value: `{"id":"dec-7","label":"Merge after the freeze"}`, ActionTS: "1800000002.300000"}),
		nudgedEvent(t, "message_changed_own.json"),
		nudgedEvent(t, "message_deleted_own.json"),
		reactionEvent(id, reactionPush{User: user, Reaction: "thumbsup", Channel: channel, TS: "1800000000.000200", Added: true, EventTS: "1800000003.000100"}),
		reactionEvent(id, reactionPush{User: user, Reaction: "thumbsup", Channel: channel, TS: "1800000000.000200", EventTS: "1800000003.000200"}),
	}
	want, err := os.ReadFile(filepath.Join("testdata", "transport", "events.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	var got []byte
	for _, e := range events {
		line, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		got = append(append(got, line...), '\n')
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("the subscriber's fixture is\n%s\nthe daemon writes\n%s", want, got)
	}
}

func TestTransportAddressesAMessageByItsPermalinkAndKeepsALocalPictureInADryRun(t *testing.T) {
	t.Parallel()
	f := transportFixture(t, true)
	url := f.id.permalink("C0LANE", "1800000000.000300", "1800000000.000100")
	for _, req := range []transportRequest{{Op: "react", URL: url, Reaction: "eyes"}, {Op: "delete", URL: url}} {
		var got transportReply
		dialTransport(t, f, req).read(t, &got)
		if !got.OK || got.Channel != "C0LANE" || got.TS != "1800000000.000300" || got.ThreadTS != "1800000000.000100" {
			t.Fatalf("%s by url answered %+v", req.Op, got)
		}
	}
	if len(f.slack.reactions) != 1 || len(f.slack.deletes) != 1 {
		t.Fatalf("%d reactions and %d deletes, want one each", len(f.slack.reactions), len(f.slack.deletes))
	}
	picture := json.RawMessage(`{"type":"image","image_url":"/pictures/5f2c.png","alt_text":"Where a claim goes"}`)
	var got transportReply
	dialTransport(t, f, transportRequest{Op: "reply", URL: url, Text: "How a claim flows.", Blocks: []json.RawMessage{picture}, DryRun: true}).read(t, &got)
	if !got.OK || !strings.Contains(string(got.Blocks), `"image_url":"/pictures/5f2c.png"`) || got.ThreadTS != "1800000000.000100" {
		t.Fatalf("a dry run with a local picture answered %+v", got)
	}
}
