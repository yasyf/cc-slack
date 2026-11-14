package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yasyf/cc-interact/store"
)

func liveStoreFixture(t *testing.T) *liveCards {
	t.Helper()
	now := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	l := &liveCards{path: filepath.Join(t.TempDir(), "live-cards.db"), log: log.New(io.Discard, "", 0), now: func() time.Time { return now }}
	t.Cleanup(func() { _ = l.close() })
	return l
}

func loadLiveStore(t *testing.T, l *liveCards) {
	t.Helper()
	if err := l.load(); err != nil {
		t.Fatal(err)
	}
}

func liveStoreChanges(t *testing.T, l *liveCards) int {
	t.Helper()
	var n int
	if err := l.st.DB().QueryRow("SELECT total_changes()").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestLiveStoreKeyedMutationAndRestart(t *testing.T) {
	l := liveStoreFixture(t)
	loadLiveStore(t, l)
	chosen := &liveThread{Channel: "D1", Thread: "1", Updated: l.now(), Answers: map[string]askAnswer{"ask": {Label: "Hold", Tap: "tap", Undelivered: "delivery"}}}
	other := &liveThread{Channel: "D1", Thread: "2", Updated: l.now()}
	key, otherKey := threadKey(chosen.Channel, chosen.Thread), threadKey(other.Channel, other.Thread)
	l.threads[key], l.threads[otherKey] = chosen, other
	l.refs[testPRURL] = refState{State: "open"}
	l.save([]string{key, otherKey}, []string{testPRURL})
	other.Updated = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	before := liveStoreChanges(t, l)
	l.answerDrawn(chosen.Channel, chosen.Thread, "ask")
	if got := liveStoreChanges(t, l) - before; got != 1 {
		t.Fatalf("answer changed %d rows, want one", got)
	}
	l.answerDelivered(chosen.Channel, chosen.Thread, "ask")
	loadLiveStore(t, l)
	answer := l.threads[key].Answers["ask"]
	if !answer.Drawn || answer.Undelivered != "" || answer.Tap != "tap" {
		t.Fatalf("restarted answer = %+v", answer)
	}
	if !l.threads[otherKey].Updated.Equal(l.now()) || l.refs[testPRURL].State != "open" {
		t.Fatal("unrelated state changed")
	}
	before = liveStoreChanges(t, l)
	l.refs[testPRURL] = refState{State: "merged", Head: "head"}
	l.save(nil, []string{testPRURL})
	if got := liveStoreChanges(t, l) - before; got != 1 {
		t.Fatalf("reference changed %d rows, want one", got)
	}
	loadLiveStore(t, l)
	if l.refs[testPRURL].State != "merged" {
		t.Fatal("reference-only update did not survive restart")
	}
}

func TestLiveStoreTransactionRollbackAndRetry(t *testing.T) {
	l := liveStoreFixture(t)
	loadLiveStore(t, l)
	thread := &liveThread{Channel: "D1", Thread: "1", Updated: l.now()}
	key := threadKey(thread.Channel, thread.Thread)
	l.threads[key], l.refs[testPRURL] = thread, refState{State: "open"}
	l.save([]string{key}, []string{testPRURL})
	if _, err := l.st.DB().Exec("CREATE TEMP TRIGGER reject_ref BEFORE UPDATE ON live_card_refs BEGIN SELECT RAISE(ABORT, 'reject ref'); END"); err != nil {
		t.Fatal(err)
	}
	thread.Stale, l.refs[testPRURL] = true, refState{State: "merged"}
	l.save([]string{key}, []string{testPRURL})
	if len(l.dirtyThreads) != 1 || len(l.dirtyRefs) != 1 {
		t.Fatal("failed transaction lost its dirty keys")
	}
	var stale bool
	if err := readLiveRows(l.st.DB(), liveThreadRows, func(_ string, data []byte) error {
		var saved liveThread
		if err := json.Unmarshal(data, &saved); err != nil {
			return err
		}
		stale = saved.Stale
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if stale {
		t.Fatal("failed reference write committed the related thread")
	}
	if _, err := l.st.DB().Exec("DROP TRIGGER reject_ref"); err != nil {
		t.Fatal(err)
	}
	l.save(nil, nil)
	loadLiveStore(t, l)
	if !l.threads[key].Stale || l.refs[testPRURL].State != "merged" {
		t.Fatal("retry did not commit both changes")
	}
}

func TestLiveStoreUndoExpiryWithoutRetainedMessage(t *testing.T) {
	l := liveStoreFixture(t)
	loadLiveStore(t, l)
	key := threadKey("D1", "1")
	l.threads[key] = &liveThread{
		Channel: "D1", Thread: "1", Updated: l.now(),
		Answers: map[string]askAnswer{"ask": {Tap: "tap", UndoUntil: l.now().Add(-time.Second)}},
	}
	l.save([]string{key}, nil)
	l.closeUndos(l.now())
	loadLiveStore(t, l)
	answer := l.threads[key].Answers["ask"]
	if answer.Tap != "" || !answer.UndoUntil.IsZero() {
		t.Fatal("expired undo returned after restart")
	}
}

func TestLiveStorePostedPRSeedSurvivesRestart(t *testing.T) {
	l := liveStoreFixture(t)
	loadLiveStore(t, l)
	pr, _ := parseRef(testPRURL)
	state := refState{State: "open", Head: "head", Checks: "passed"}
	prs := []linkedPR{{ref: pr, state: state}}
	if asked, _, _ := l.toCard("D1", "1", nil, prs); len(asked) != 1 {
		t.Fatal("new PR did not seed a card")
	}
	l.commit(&liveThread{
		Channel: "D1", Thread: "1", Updated: l.now(),
		Reply: &liveMessage{Card: statusCard{Refs: []cardLink{{URL: testPRURL}}}},
	})
	before := liveStoreChanges(t, l)
	l.toCard("D1", "1", nil, prs)
	l.save(nil, nil)
	if liveStoreChanges(t, l) != before {
		t.Fatal("unchanged PR seed wrote state again")
	}
	loadLiveStore(t, l)
	if l.refs[pr.key()] != state {
		t.Fatal("posted PR seed did not survive restart")
	}
}

func TestLiveStoreSharedOwedTapRescheduleSurvivesRestart(t *testing.T) {
	l := liveStoreFixture(t)
	loadLiveStore(t, l)
	from := &liveThread{
		Channel: "D1", Thread: "1", Updated: l.now(),
		Root:    &liveMessage{Card: statusCard{Title: "Retained root"}},
		Answers: map[string]askAnswer{"ask": {Owed: &owedTap{Card: "card", Due: l.now().Add(-time.Second)}}},
	}
	into := &liveThread{Channel: "D1", Thread: "2", Updated: l.now()}
	into.carryAnswers(from)
	keys := []string{threadKey(from.Channel, from.Thread), threadKey(into.Channel, into.Thread)}
	l.threads[keys[0]], l.threads[keys[1]] = from, into
	l.save(keys, nil)
	due, _ := l.owedTaps(l.now(), time.Minute)
	if len(due) != 1 {
		t.Fatalf("shared tap scheduled %d retries, want one", len(due))
	}
	loadLiveStore(t, l)
	for _, key := range keys {
		if got := l.threads[key].Answers["ask"].Owed.Due; !got.Equal(l.now().Add(time.Minute)) {
			t.Fatalf("%s retry due restored as %s", key, got)
		}
	}
}

func TestLiveStorePruningPersists(t *testing.T) {
	l := liveStoreFixture(t)
	loadLiveStore(t, l)
	old := &liveThread{Channel: "D1", Thread: "old", Updated: l.now().Add(-liveCardIdle - time.Second), Reply: &liveMessage{Card: statusCard{Refs: []cardLink{{URL: testPRURL}}}}}
	key := threadKey(old.Channel, old.Thread)
	l.threads[key], l.refs[testPRURL] = old, refState{State: "open"}
	l.save([]string{key}, []string{testPRURL})
	l.pending(l.now())
	loadLiveStore(t, l)
	if len(l.threads) != 0 || len(l.refs) != 0 {
		t.Fatal("expired thread or unused reference survived pruning")
	}
	var keys []string
	for i := 0; i <= liveMaxThreads; i++ {
		thread := &liveThread{Channel: "D1", Thread: fmt.Sprint(i), Updated: l.now().Add(time.Duration(i) * time.Second)}
		key := threadKey(thread.Channel, thread.Thread)
		l.threads[key] = thread
		keys = append(keys, key)
	}
	l.save(keys, nil)
	loadLiveStore(t, l)
	if len(l.threads) != liveMaxThreads || l.threads[threadKey("D1", "0")] != nil {
		t.Fatal("thread limit did not remove the oldest row")
	}
	loadLiveStore(t, l)
	if len(l.threads) != liveMaxThreads {
		t.Fatal("pruned rows returned after restart")
	}
}

func TestLiveStorePruningKeepsAskingCards(t *testing.T) {
	l := liveStoreFixture(t)
	loadLiveStore(t, l)
	idle := l.now().Add(-liveCardIdle - time.Hour)
	asking := []*liveThread{
		{Channel: "D1", Thread: "pr", Updated: idle, PRCards: []*liveMessage{{TS: "pr", Card: statusCard{Waits: "pr-card"}}}},
		{Channel: "D1", Thread: "question", Updated: idle, Waiting: map[string]*liveMessage{"ask": {TS: "question", Card: statusCard{Waits: "ask"}}}},
		{Channel: "D1", Thread: "owed", Updated: idle, Answers: map[string]askAnswer{"ask": {Label: "Hold", Drawn: true, Owed: &owedTap{Card: "owed", Question: &questionAnswer{Label: "Hold"}, Due: idle}}}},
	}
	tapped := &liveThread{Channel: "D1", Thread: "tapped", Updated: idle, PRCards: []*liveMessage{{TS: "tapped", Card: statusCard{Waits: "tapped-card"}, Tapped: "Approved"}}}
	var keys []string
	for _, thread := range append(asking, tapped) {
		key := threadKey(thread.Channel, thread.Thread)
		l.threads[key] = thread
		keys = append(keys, key)
	}
	for i := 0; i <= liveMaxThreads; i++ {
		thread := &liveThread{Channel: "D1", Thread: fmt.Sprint(i), Updated: l.now().Add(time.Duration(i) * time.Second)}
		key := threadKey(thread.Channel, thread.Thread)
		l.threads[key] = thread
		keys = append(keys, key)
	}
	l.save(keys, nil)
	for range 2 {
		loadLiveStore(t, l)
		if len(l.threads) != liveMaxThreads {
			t.Fatalf("store kept %d threads, want %d", len(l.threads), liveMaxThreads)
		}
		for _, thread := range asking {
			if l.threads[threadKey(thread.Channel, thread.Thread)] == nil {
				t.Fatalf("pruning forgot thread %s, whose card still asks or owes a tap", thread.Thread)
			}
		}
		if l.threads[threadKey(tapped.Channel, tapped.Thread)] != nil || l.threads[threadKey("D1", "3")] != nil || l.threads[threadKey("D1", "4")] == nil {
			t.Fatal("pruning kept a settled thread past the limit or forgot a newer one")
		}
	}
}

func TestLiveStoreLegacyCutoverIsIdempotent(t *testing.T) {
	l := liveStoreFixture(t)
	thread := &liveThread{Channel: "D1", Thread: "1", Updated: l.now(), Answers: map[string]askAnswer{"ask": {Label: "Hold", Tap: "tap", Drawn: true}}}
	paths := []string{filepath.Join(filepath.Dir(l.path), "live-cards.json"), filepath.Join(filepath.Dir(l.path), "live-refs.json")}
	for i, data := range [][]byte{mustJSON([]*liveThread{thread}), mustJSON(map[string]refState{testPRURL: {State: "merged"}})} {
		if err := os.WriteFile(paths[i], data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	loadLiveStore(t, l)
	key := threadKey(thread.Channel, thread.Thread)
	if !l.threads[key].Answers["ask"].Drawn || l.refs[testPRURL].State != "merged" {
		t.Fatal("cutover lost retained state")
	}
	for _, path := range paths {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("legacy writer path remains: %s: %v", path, err)
		}
		if err := os.Rename(path+".imported", path); err != nil {
			t.Fatal(err)
		}
	}
	delete(l.threads, key)
	delete(l.refs, testPRURL)
	l.save([]string{key}, []string{testPRURL})
	loadLiveStore(t, l)
	if len(l.threads) != 0 || len(l.refs) != 0 {
		t.Fatal("interrupted retirement resurrected deleted rows")
	}
	loadLiveStore(t, l)
	if len(l.threads) != 0 || len(l.refs) != 0 {
		t.Fatal("completed cutover imported legacy state again")
	}
}

func TestLiveStoreFailedImportPreservesLegacyState(t *testing.T) {
	for _, invalidRefs := range []bool{true, false} {
		t.Run(fmt.Sprint(invalidRefs), func(t *testing.T) {
			l := liveStoreFixture(t)
			thread := &liveThread{Channel: "D1", Thread: "1", Updated: l.now()}
			threads := []*liveThread{thread}
			refs := mustJSON(map[string]refState{testPRURL: {State: "open"}})
			if invalidRefs {
				refs = []byte("{")
			} else {
				threads = append(threads, nil)
			}
			paths := []string{filepath.Join(filepath.Dir(l.path), "live-cards.json"), filepath.Join(filepath.Dir(l.path), "live-refs.json")}
			for i, data := range [][]byte{mustJSON(threads), refs} {
				if err := os.WriteFile(paths[i], data, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := l.load(); err == nil {
				t.Fatal("invalid legacy state imported")
			}
			st, err := store.Open(context.Background(), l.path, liveCardsSchema)
			if err != nil {
				t.Fatal(err)
			}
			var n int
			err = st.DB().QueryRow("SELECT (SELECT count(*) FROM live_card_threads) + (SELECT count(*) FROM live_card_refs) + (SELECT count(*) FROM live_card_import)").Scan(&n)
			_ = st.Close()
			if err != nil || n != 0 {
				t.Fatalf("failed import left %d rows: %v", n, err)
			}
			for _, path := range paths {
				if _, err := os.Stat(path); err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(path + ".imported"); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("failed import retired %s: %v", path, err)
				}
			}
			if err := os.WriteFile(paths[0], mustJSON([]*liveThread{thread}), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(paths[1], mustJSON(map[string]refState{testPRURL: {State: "open"}}), 0o600); err != nil {
				t.Fatal(err)
			}
			loadLiveStore(t, l)
			if len(l.threads) != 1 || len(l.refs) != 1 {
				t.Fatal("repaired import did not retry")
			}
		})
	}
}
