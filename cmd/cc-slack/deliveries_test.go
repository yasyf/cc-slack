package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const sweptReply = "1800000050.000200"

func sessionTranscriptAt(t *testing.T, f *fixture) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	project := filepath.Join(dir, "projects", "-repo")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	transcript := filepath.Join(project, testSession+".jsonl")
	if err := os.WriteFile(transcript, []byte(`{"type":"user","message":{"content":"earlier"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db().ExecContext(f.ctx, `INSERT INTO subjects(id, session_id, scope, created_at, updated_at) VALUES (?, ?, 'test', 0, 0)
ON CONFLICT(id) DO UPDATE SET session_id = excluded.session_id`, testSubject, testSession); err != nil {
		t.Fatal(err)
	}
	return transcript
}

func takeEveryDirective(t *testing.T, f *fixture) {
	t.Helper()
	if _, err := f.db().ExecContext(f.ctx, `UPDATE directives SET delivered_at = ? WHERE delivered_at IS NULL`, f.now.Unix()); err != nil {
		t.Fatal(err)
	}
}

func redeliveries(f *fixture) []string {
	var lines []string
	for _, d := range f.plane.directives {
		if strings.HasPrefix(d.text, "cc-slack delivers this again") {
			lines = append(lines, d.text)
		}
	}
	return lines
}

func inFlightMarks(f *fixture) []string {
	var marked []string
	for _, r := range f.slack.reactions {
		if r.text == inFlightReaction {
			marked = append(marked, r.ts)
		}
	}
	return marked
}

func TestAnOwnerMessageMissingFromTheHoldersTranscriptIsDeliveredAgain(t *testing.T) {
	f := newFixture(t)
	transcript := sessionTranscriptAt(t, f)
	holdDM(t, f)
	thread := postDMThread(t, f)
	f.tick()

	ownerReplies(f, thread, sweptReply, "ship it")
	takeEveryDirective(t, f)
	f.now = f.now.Add(redeliverAfter - time.Second)
	f.tick()
	if got := redeliveries(f); len(got) != 0 {
		t.Fatalf("redeliveries = %q, want none before %s", got, redeliverAfter)
	}

	f.now = f.now.Add(sweepEvery)
	f.tick()
	got := redeliveries(f)
	if len(got) != 1 || !strings.Contains(got[0], sweptReply) {
		t.Fatalf("redeliveries = %q, want one naming %s", got, sweptReply)
	}

	logToTranscript(t, transcript, `{"type":"user","message":{"content":"`+sweptReply+`"}}`)
	takeEveryDirective(t, f)
	f.now = f.now.Add(redeliverAfter + sweepEvery)
	f.tick()
	if got := redeliveries(f); len(got) != 1 {
		t.Fatalf("redeliveries = %d, want still one once the transcript shows it", len(got))
	}
}

func TestAnOwnerMessageStillUnansweredAfterThreeMinutesShowsItIsInFlight(t *testing.T) {
	f := newFixture(t)
	sessionTranscriptAt(t, f)
	holdDM(t, f)
	thread := postDMThread(t, f)
	f.tick()

	ownerReplies(f, thread, sweptReply, "ship it")
	f.now = f.now.Add(inFlightAfter - sweepEvery)
	f.tick()
	if got := inFlightMarks(f); len(got) != 0 {
		t.Fatalf("in-flight marks on %v, want none before %s", got, inFlightAfter)
	}
	f.now = f.now.Add(sweepEvery)
	f.tick()
	if got := inFlightMarks(f); !slices.Equal(got, []string{sweptReply}) {
		t.Fatalf("in-flight marks on %v, want one on %s", got, sweptReply)
	}
	f.now = f.now.Add(sweepEvery)
	f.tick()
	if got := inFlightMarks(f); len(got) != 1 {
		t.Fatalf("in-flight marks on %v, want still one", got)
	}
}

func TestAnAnsweredOwnerMessageIsNoLongerSwept(t *testing.T) {
	f := newFixture(t)
	sessionTranscriptAt(t, f)
	holdDM(t, f)
	thread := postDMThread(t, f)
	f.tick()

	ownerReplies(f, thread, sweptReply, "ship it")
	if err := f.app.owed.answer(f.ctx, sweptReply); err != nil {
		t.Fatal(err)
	}
	takeEveryDirective(t, f)
	f.now = f.now.Add(inFlightAfter)
	f.tick()
	if got := redeliveries(f); len(got) != 0 {
		t.Fatalf("redeliveries = %q, want none for an answered message", got)
	}
	if got := inFlightMarks(f); len(got) != 0 {
		t.Fatalf("in-flight marks on %v, want none for an answered message", got)
	}
	open, err := f.app.poller.holds.ownerMessages(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 0 {
		t.Fatalf("open owner messages = %d, want none once answered", len(open))
	}
}
