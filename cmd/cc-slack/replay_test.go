package main

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/yasyf/cc-interact/agent"
)

func seqAt(at time.Time, tiebreak string) string {
	return fmt.Sprintf("%015d-%s", at.UnixMilli(), tiebreak)
}

func savedSeq(t *testing.T, r *replayCursor) string {
	t.Helper()
	data, err := os.ReadFile(r.path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestReplayCursorSaysHelloSinceItsSavedSeqUntilTheLogDropsIt(t *testing.T) {
	r := openTestReplay(t)
	now := time.Unix(1_800_000_000, 0)
	if got := r.hello(now); got != (helloFrame{Type: helloType}) {
		t.Fatalf("hello with no seq = %+v, want no since", got)
	}
	seq := seqAt(now, "")
	r.caughtUp(seq)
	if got := r.hello(now); got.Since != "" {
		t.Fatalf("hello names since %q before the seq was saved", got.Since)
	}
	if err := r.save(r.latest()); err != nil {
		t.Fatal(err)
	}
	if got := savedSeq(t, r); got != seq {
		t.Fatalf("saved seq = %q, want %q", got, seq)
	}
	reloaded, err := loadReplayCursor(r.path)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		after time.Duration
		since string
	}{
		{time.Hour, seq},
		{nudgeRetention - replayMargin, seq},
		{nudgeRetention - replayMargin + time.Second, ""},
	} {
		if got := reloaded.hello(now.Add(c.after)); got.Since != c.since {
			t.Fatalf("hello %s after the seq names since %q, want %q", c.after, got.Since, c.since)
		}
	}
}

func TestReplayCursorHoldsLiveSeqsUntilTheReplayIsCaughtUp(t *testing.T) {
	r := openTestReplay(t)
	now := time.Unix(1_800_000_000, 0)
	r.caughtUp(seqAt(now, ""))
	live := seqAt(now.Add(time.Second), "0a")
	r.nudged(live)
	r.nudged("")
	if got := r.latest(); got != live {
		t.Fatalf("latest = %q after a live nudge, want %q", got, live)
	}

	r.hello(now.Add(time.Minute))
	during := seqAt(now.Add(2*time.Minute), "0b")
	r.nudged(during)
	if got := r.latest(); got != live {
		t.Fatalf("latest = %q while a replay was in flight, want it held at %q", got, live)
	}
	answered := seqAt(now.Add(time.Minute), "")
	r.caughtUp(answered)
	if got := r.latest(); got != answered {
		t.Fatalf("latest = %q once caught up, want the answer %q", got, answered)
	}
	r.caughtUp(seqAt(now, ""))
	r.nudged(during)
	if got := r.latest(); got != during {
		t.Fatalf("latest = %q, want live nudges to advance it again and an older answer never to rewind it", got)
	}
	if err := r.save(during); err != nil {
		t.Fatal(err)
	}
	if err := r.save(answered); err != nil {
		t.Fatal(err)
	}
	if got := savedSeq(t, r); got != during {
		t.Fatalf("saved seq = %q, want an older save never to rewind %q", got, during)
	}
}

func TestHelloReconcilesOnlyWithoutAFreshSeq(t *testing.T) {
	f := newFixture(t)
	f.tick()
	frame := mustHello(t, f)
	if frame != (helloFrame{Type: helloType}) || !f.poller.reconcile.Load() {
		t.Fatalf("hello = %+v, reconcile = %v; want no since and a reconcile of every watch", frame, f.poller.reconcile.Load())
	}
	seq := seqAt(f.now, "")
	f.push(mustJSON(map[string]string{"type": caughtUpType, "seq": seq}))
	f.tick()

	frame = mustHello(t, f)
	if frame.Since != seq || f.poller.reconcile.Load() {
		t.Fatalf("hello = %+v, reconcile = %v; want since %q and no reconcile", frame, f.poller.reconcile.Load(), seq)
	}
}

func TestPollerSavesTheSeqOnlyOnceEveryNudgeBeforeItIsRead(t *testing.T) {
	f := newFixture(t)
	f.watch(agent.TopLevel, testThread)
	f.tick()
	f.push(mustJSON(map[string]string{"type": caughtUpType, "seq": seqAt(f.now, "")}))
	f.tick()
	f.poller.paused[methodReplies] = f.now.Add(time.Hour)
	seq := seqAt(f.now.Add(time.Second), "c0ffee00")
	f.push(mustJSON(map[string]any{"event_type": nudgeEventType, "event_payload": map[string]string{
		"channel": testChannel, "ts": "1800000000.000300", "thread_ts": testThread, "seq": seq,
	}}))
	f.tick()
	if got := savedSeq(t, f.app.replay); got != seqAt(f.now, "") {
		t.Fatalf("saved seq = %q while the nudged thread waits out a rate limit, want the seq before it", got)
	}

	delete(f.poller.paused, methodReplies)
	f.tick()
	if got := savedSeq(t, f.app.replay); got != seq {
		t.Fatalf("saved seq = %q once the thread was read, want the nudge's %q", got, seq)
	}
}

func TestReplayFramesWakeThePollerToSaveTheirSeq(t *testing.T) {
	f := newFixture(t)
	f.tick()
	for _, frame := range [][]byte{
		mustJSON(map[string]string{"type": caughtUpType, "seq": seqAt(f.now, "")}),
		mustJSON(map[string]any{"event_type": nudgeEventType, "event_payload": map[string]string{
			"channel": testChannel, "ts": "1800000000.000300", "subtype": "message_changed", "seq": seqAt(f.now, "0a"),
		}}),
	} {
		select {
		case <-f.poller.wakeCh:
		default:
		}
		f.push(frame)
		select {
		case <-f.poller.wakeCh:
		default:
			t.Fatalf("frame %s left the poller asleep with its seq unsaved", frame)
		}
	}
}

func TestAnyAnswerToTheHelloReleasesTheHeldSeq(t *testing.T) {
	f := newFixture(t)
	f.app.replay.hello(f.now)
	seq := seqAt(f.now, "")
	f.push(mustJSON(map[string]string{"type": "upgrade", "seq": seq, "min_version": "0.1.1"}))
	live := seqAt(f.now.Add(time.Second), "0a")
	f.app.replay.nudged(live)
	if got := f.app.replay.latest(); got != live {
		t.Fatalf("latest = %q, want an answer this build does not know to release the hold so %q advances it", got, live)
	}
}

func mustHello(t *testing.T, f *fixture) helloFrame {
	t.Helper()
	raw, err := f.app.hello()
	if err != nil {
		t.Fatal(err)
	}
	var frame helloFrame
	if err := json.Unmarshal(raw, &frame); err != nil {
		t.Fatal(err)
	}
	return frame
}
