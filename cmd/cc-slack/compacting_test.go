package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const compactingReply = "1800000001.500000"

func compactWithTranscript(t *testing.T, f *fixture) string {
	t.Helper()
	transcript := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(transcript, []byte(`{"type":"user","message":{"content":"earlier"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.poller.startCompacting(testSubject, transcript)
	return transcript
}

func logToTranscript(t *testing.T, transcript, line string) {
	t.Helper()
	tf, err := os.OpenFile(transcript, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tf.Close() }()
	if _, err := tf.WriteString(line + "\n"); err != nil {
		t.Fatal(err)
	}
}

func compactionAcks(f *fixture) []fakePost {
	return slices.DeleteFunc(f.slack.sent(), func(p fakePost) bool { return p.text != compactionAck })
}

func compactingDM(t *testing.T) (*fixture, string) {
	t.Helper()
	f := newFixture(t)
	holdDM(t, f)
	thread := postDMThread(t, f)
	f.tick()
	return f, thread
}

func TestAMessageInTheSessionsTranscriptWithinEightSecondsOfArrivingGetsNoAck(t *testing.T) {
	f, thread := compactingDM(t)
	transcript := compactWithTranscript(t, f)

	ownerReplies(f, thread, compactingReply, "ship it")
	logToTranscript(t, transcript, `{"type":"user","message":{"content":"<channel source=\"plugin:cc-slack:cc-slack\">{\"ts\":\"`+compactingReply+`\"}</channel>"}}`)
	f.now = f.now.Add(compactionAckWait)
	f.tick()

	if got := compactionAcks(f); len(got) != 0 {
		t.Fatalf("acks = %+v, want none for a message the session logged", got)
	}
}

func TestAMessageMissingFromTheSessionsTranscriptEightSecondsAfterArrivingGetsOneAck(t *testing.T) {
	f, thread := compactingDM(t)
	transcript := compactWithTranscript(t, f)

	ownerReplies(f, thread, compactingReply, "ship it")
	logToTranscript(t, transcript, `{"type":"assistant","message":{"content":"still compacting someone else's work"}}`)
	if next := f.tick(); next.IsZero() || next.After(f.now.Add(compactionAckWait)) {
		t.Fatalf("next tick at %v, want one by %v to settle the held ack", next, f.now.Add(compactionAckWait))
	}
	if got := compactionAcks(f); len(got) != 0 {
		t.Fatalf("acks = %+v, want none before %s pass", got, compactionAckWait)
	}

	f.now = f.now.Add(compactionAckWait)
	f.tick()
	f.now = f.now.Add(compactionAckWait)
	f.tick()

	posted := compactionAcks(f)
	if len(posted) != 1 || posted[0].threadTS != thread {
		t.Fatalf("acks = %+v, want one %q in thread %s", posted, compactionAck, thread)
	}
	last := f.plane.events[len(f.plane.events)-1]
	var told ackedPayload
	if err := json.Unmarshal(last.Payload, &told); err != nil {
		t.Fatal(err)
	}
	if last.Type != eventAcked || told.TS != compactingReply || told.Acked != posted[0].ts {
		t.Fatalf("event %s %+v, want %s naming ack %s of %s", last.Type, told, eventAcked, posted[0].ts, compactingReply)
	}
	if got := f.plane.directives[len(f.plane.directives)-1].text; !strings.Contains(got, "do not ack it again") {
		t.Fatalf("directive = %q, want it to say the message is already acked", got)
	}
	f.poller.nudged(messageNudge{Channel: f.id.DMChannelID, TS: posted[0].ts, ThreadTS: thread})
	f.tick()
	if got := owedStamps(t, f); !slices.Equal(got, []string{compactingReply}) {
		t.Fatalf("owed %v, want the acked message still owed its answer", got)
	}
}

func TestAMessageAfterTheSessionStartsAgainGetsNoAck(t *testing.T) {
	f, thread := compactingDM(t)
	compactWithTranscript(t, f)
	if !f.poller.endCompacting(testSubject) {
		t.Fatal("the session was not marked compacting")
	}

	ownerReplies(f, thread, compactingReply, "ship it")
	f.now = f.now.Add(compactionAckWait)
	f.tick()

	if got := compactionAcks(f); len(got) != 0 {
		t.Fatalf("acks = %+v, want none once the session is back", got)
	}
}

func TestAMessageWhileTheSessionCompactsWithNoTranscriptGetsNoAck(t *testing.T) {
	f, thread := compactingDM(t)
	f.poller.startCompacting(testSubject, "")

	ownerReplies(f, thread, compactingReply, "ship it")
	f.now = f.now.Add(compactionAckWait)
	f.tick()

	if got := compactionAcks(f); len(got) != 0 {
		t.Fatalf("acks = %+v, want none when no transcript can show the message is missing", got)
	}
}
