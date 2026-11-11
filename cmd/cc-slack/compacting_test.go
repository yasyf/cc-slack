package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/yasyf/cc-interact/agent"
)

const compactingReply = "1800000001.500000"

func compactWithTranscript(t *testing.T, f *fixture) string {
	t.Helper()
	transcript := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(transcript, []byte(`{"type":"user","message":{"content":"earlier"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mark, err := markTranscript(transcript)
	if err != nil {
		t.Fatal(err)
	}
	f.poller.startCompacting(testSubject, mark)
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

const boundaryRecord = `{"type":"system","subtype":"compact_boundary","content":"Conversation compacted","compactMetadata":{"trigger":"auto","preTokens":567047}}`

func assistantUsage(model string, tokens int64) string {
	return fmt.Sprintf(`{"type":"assistant","isSidechain":false,"message":{"model":%q,"usage":{"input_tokens":2,"cache_creation_input_tokens":%d,"cache_read_input_tokens":1000,"output_tokens":90}}}`, model, tokens-1002)
}

func writeTranscript(t *testing.T, lines ...string) string {
	t.Helper()
	transcript := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(transcript, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return transcript
}

func TestAPreCompactAtNinetyPercentOfTheCompactionPointOrAManualOneMarksTheSession(t *testing.T) {
	for _, c := range []struct {
		name    string
		body    preCompactBody
		marking bool
	}{
		{"an agent compacting while the session stands at 63% of its point", preCompactBody{Trigger: "auto", Context: 358273, Point: 567047}, false},
		{"the session at 99% of its point", preCompactBody{Trigger: "auto", Context: 366054, Point: 368634}, true},
		{"a manual compact well below the point", preCompactBody{Trigger: "manual", Context: 120000, Point: 567047}, true},
		{"a session with no point on disk", preCompactBody{Trigger: "auto", Context: 120000}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			l := startDaemon(t)
			c.body.Transcript = writeTranscript(t, assistantUsage("claude-opus-5-5", c.body.Context))
			l.do(opPreCompact, c.body)
			if got := len(l.app.poller.compacting([]watch{{SubjectID: l.subID, Owner: agent.TopLevel}})) > 0; got != c.marking {
				t.Fatalf("compacting = %v, want %v", got, c.marking)
			}
		})
	}
}

func TestAPreCompactWithNoTranscriptMarksNothing(t *testing.T) {
	l := startDaemon(t)
	l.do(opPreCompact, preCompactBody{Trigger: "manual"})
	if len(l.app.poller.compacting([]watch{{SubjectID: l.subID, Owner: agent.TopLevel}})) != 0 {
		t.Fatal("the session is compacting with no transcript to show a message missing")
	}
}

func TestAnAgentsSessionStartAfterItCompactedLeavesTheSessionsMark(t *testing.T) {
	l := startDaemon(t)
	held := []watch{{SubjectID: l.subID, Owner: agent.TopLevel}}
	l.do(opPreCompact, preCompactBody{Transcript: writeTranscript(t, assistantUsage("claude-opus-5-5", 567000)), Trigger: "manual"})
	l.do(opSessionStart, sessionStartBody{Source: "compact"})
	if len(l.app.poller.compacting(held)) == 0 {
		t.Fatal("a SessionStart(compact) ended the session's mark")
	}
	l.do(opSessionStart, sessionStartBody{Source: "startup"})
	if len(l.app.poller.compacting(held)) != 0 {
		t.Fatal("the session is still compacting after its process started again")
	}
}

func TestAMessageAfterTheSessionsCompactBoundaryGetsNoAckAndEndsTheMark(t *testing.T) {
	f, thread := compactingDM(t)
	transcript := compactWithTranscript(t, f)
	logToTranscript(t, transcript, boundaryRecord)

	ownerReplies(f, thread, compactingReply, "ship it")
	if next := f.tick(); next.IsZero() {
		t.Fatal("no ack held for a message that arrived while the session was marked compacting")
	}
	f.now = f.now.Add(compactionAckWait)
	f.tick()
	f.now = f.now.Add(compactionAckWait)
	f.tick()

	if got := compactionAcks(f); len(got) != 0 {
		t.Fatalf("acks = %+v, want none once the transcript records the compaction", got)
	}
	if f.poller.endCompacting(testSubject) {
		t.Fatal("the session is still marked compacting after its compact_boundary record")
	}
}

func TestTheTranscriptTailIsTheLastMainAssistantUsageAndTheLastAutoCompaction(t *testing.T) {
	agentProgress := `{"type":"progress","data":{"message":{"type":"assistant","message":{"model":"claude-sonnet-5-5","usage":{"input_tokens":5,"cache_creation_input_tokens":0,"cache_read_input_tokens":250000}}}}}`
	sidechain := `{"type":"assistant","isSidechain":true,"message":{"model":"claude-sonnet-5-5","usage":{"input_tokens":5,"cache_creation_input_tokens":0,"cache_read_input_tokens":250000}}}`
	synthetic := `{"type":"assistant","message":{"model":"<synthetic>","usage":{"input_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`
	bigResult := `{"type":"user","message":{"content":"` + strings.Repeat("x", 3*transcriptTailChunk) + `"}}`
	for _, c := range []struct {
		name  string
		lines []string
		want  transcriptTail
	}{
		{"after an auto compaction", []string{assistantUsage("claude-opus-5-5", 566000), boundaryRecord, assistantUsage("claude-opus-5-5", 358273), bigResult, agentProgress, sidechain, synthetic},
			transcriptTail{model: "claude-opus-5-5", context: 358273, point: 567047}},
		{"right after a compaction", []string{assistantUsage("claude-opus-5-5", 566000), boundaryRecord},
			transcriptTail{point: 567047}},
		{"after a manual compaction", []string{boundaryRecord, strings.Replace(boundaryRecord, `"auto"`, `"manual"`, 1), assistantUsage("claude-fable-5-2", 200000)},
			transcriptTail{model: "claude-fable-5-2", context: 200000}},
		{"never compacted", []string{bigResult, assistantUsage("claude-opus-5-5", 120000), bigResult},
			transcriptTail{model: "claude-opus-5-5", context: 120000}},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := readTranscriptTail(writeTranscript(t, c.lines...))
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Fatalf("tail = %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestTheCompactionPointIsTheMeasuredOneCappedByTheWindowClaudeCodeResolves(t *testing.T) {
	config := t.TempDir()
	settings := `{"autoCompactWindow":600000,"modelSettings":{"claude-fable-5-2":{"autoCompactWindow":400000},"claude-haiku-5-5":{"autoCompactWindow":"auto"}}}`
	if err := os.WriteFile(filepath.Join(config, "settings.json"), []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		tail transcriptTail
		env  string
		want int64
	}{
		{"measured under the window", transcriptTail{model: "claude-opus-5-5", point: 567047}, "", 567047},
		{"measured over a lowered per-model window", transcriptTail{model: "claude-fable-5-2", point: 567047}, "", 400000},
		{"no compaction yet", transcriptTail{model: "claude-opus-5-5"}, "", 600000},
		{"the environment over settings, in Claude Code's shorthand", transcriptTail{model: "claude-fable-5-2"}, "450k", 450000},
		{"an environment window below Claude Code's minimum", transcriptTail{model: "claude-opus-5-5"}, "50000", 100000},
		{"an unusable environment window", transcriptTail{model: "claude-opus-5-5"}, "lots", 600000},
		{"a model set to auto", transcriptTail{model: "claude-haiku-5-5"}, "", 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			env := map[string]string{"CLAUDE_CONFIG_DIR": config, "CLAUDE_CODE_AUTO_COMPACT_WINDOW": c.env}
			if got := compactionPoint(c.tail, func(k string) string { return env[k] }); got != c.want {
				t.Fatalf("point = %d, want %d", got, c.want)
			}
		})
	}
}
