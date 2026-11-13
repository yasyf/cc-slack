package main

import (
	"os"
	"reflect"
	"slices"
	"testing"
	"time"
)

func unchangedLiveFiles(t *testing.T, cards *liveCards) func() {
	t.Helper()
	type savedFile struct {
		path string
		info os.FileInfo
		data []byte
	}
	var saved []savedFile
	for _, path := range []string{cards.path, cards.refsPath()} {
		file, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { file.Close() })
		info, err := file.Stat()
		if err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		saved = append(saved, savedFile{path, info, data})
	}
	return func() {
		t.Helper()
		for _, before := range saved {
			info, err := os.Stat(before.path)
			if err != nil {
				t.Fatal(err)
			}
			if !os.SameFile(before.info, info) {
				t.Errorf("%s was replaced by a no-op", before.path)
			}
			data, err := os.ReadFile(before.path)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(before.data, data) {
				t.Errorf("%s contents changed during a no-op", before.path)
			}
		}
	}
}

func TestLiveCardAnswerNoopsPreserveFiles(t *testing.T) {
	l := startDaemon(t)
	_, srv := serveRefs(t)
	clock := &testClock{}
	clock.at.Store(time.Date(2026, 11, 12, 17, 0, 0, 0, time.UTC).UnixNano())
	tests := []struct {
		name string
		run  func(*liveCards, cardRef) []undoneAnswer
	}{
		{"undo missing tap", func(cards *liveCards, card cardRef) []undoneAnswer {
			return cards.undo(card.channel, "missing")
		}},
		{"undo wrong channel", func(cards *liveCards, _ cardRef) []undoneAnswer {
			return cards.undo("missing", "tap")
		}},
		{"undo reply wrong words", func(cards *liveCards, card cardRef) []undoneAnswer {
			return cards.undoReply(card, "missing")
		}},
		{"undo reply missing message", func(cards *liveCards, card cardRef) []undoneAnswer {
			card.ts = "missing"
			return cards.undoReply(card, "Hold")
		}},
		{"release missing answer", func(cards *liveCards, card cardRef) []undoneAnswer {
			cards.release(card.channel, card.thread, "missing")
			return nil
		}},
		{"release missing thread", func(cards *liveCards, card cardRef) []undoneAnswer {
			cards.release(card.channel, "missing", testWaits)
			return nil
		}},
		{"answer already drawn", func(cards *liveCards, card cardRef) []undoneAnswer {
			cards.answerDrawn(card.channel, card.thread, testWaits)
			return nil
		}},
		{"draw missing answer", func(cards *liveCards, card cardRef) []undoneAnswer {
			cards.answerDrawn(card.channel, card.thread, "missing")
			return nil
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cards := standaloneCards(t, l, srv, clock)
			seedLiveThread(cards, statusCard{Title: "Question", Waits: testWaits}, clock.now().UTC())
			card := cardRef{"D" + testUser, "1800000000.000100", "1800000000.000100"}
			thread := cards.threads[threadKey(card.channel, card.thread)]
			thread.Answers = map[string]askAnswer{testWaits: {At: clock.now().UTC(), Label: "Hold", Other: true, Drawn: true, Tap: "tap"}}
			thread.Reply.Tapped = "Hold"
			cards.refs[testPRURL] = refState{State: "merged"}
			cards.save()
			before := mustJSON(cards.threads)
			checkFiles := unchangedLiveFiles(t, cards)
			if got := tt.run(cards, card); len(got) != 0 {
				t.Fatalf("no-op = %v, want no cards", got)
			}
			checkFiles()
			if !slices.Equal(before, mustJSON(cards.threads)) {
				t.Fatal("no-op changed the retained state")
			}
		})
	}
}

func TestLiveCardAnswerMutationsPersist(t *testing.T) {
	l := startDaemon(t)
	_, srv := serveRefs(t)
	clock := &testClock{}
	clock.at.Store(time.Date(2026, 11, 12, 17, 0, 0, 0, time.UTC).UnixNano())
	tests := []struct {
		name       string
		run        func(*liveCards, cardRef) []undoneAnswer
		unretained bool
		wantUndone int
		wantDrawn  bool
		wantTapped string
	}{
		{name: "undo", run: func(cards *liveCards, card cardRef) []undoneAnswer {
			return cards.undo(card.channel, "tap")
		}, wantUndone: 1},
		{name: "undo reply", run: func(cards *liveCards, card cardRef) []undoneAnswer {
			return cards.undoReply(card, "Hold")
		}, wantUndone: 1},
		{name: "undo without retained message", run: func(cards *liveCards, card cardRef) []undoneAnswer {
			return cards.undo(card.channel, "tap")
		}, unretained: true, wantTapped: "Hold"},
		{name: "release", run: func(cards *liveCards, card cardRef) []undoneAnswer {
			cards.release(card.channel, card.thread, testWaits)
			return nil
		}, wantTapped: "Hold"},
		{name: "draw answer", run: func(cards *liveCards, card cardRef) []undoneAnswer {
			cards.answerDrawn(card.channel, card.thread, testWaits)
			return nil
		}, wantDrawn: true, wantTapped: "Hold"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cards := standaloneCards(t, l, srv, clock)
			seedLiveThread(cards, statusCard{Title: "Question", Waits: testWaits}, clock.now().UTC())
			card := cardRef{"D" + testUser, "1800000000.000100", "1800000000.000100"}
			thread := cards.threads[threadKey(card.channel, card.thread)]
			answer := askAnswer{At: clock.now().UTC(), Label: "Hold", Other: true, Tap: "tap"}
			keep := askAnswer{At: clock.now().UTC(), Label: "Keep", Tap: "keep"}
			waits := testWaits
			if tt.unretained {
				waits = "unretained"
			}
			thread.Answers = map[string]askAnswer{waits: answer, "keep": keep}
			thread.Reply.Tapped = "Hold"
			cards.refs[testPRURL] = refState{State: "merged"}
			cards.save()
			if got := tt.run(cards, card); len(got) != tt.wantUndone {
				t.Fatalf("undone = %v, want %d cards", got, tt.wantUndone)
			}
			reloaded := standaloneCards(t, l, srv, clock)
			reloaded.path = cards.path
			if err := reloaded.load(); err != nil {
				t.Fatal(err)
			}
			wantAnswers := map[string]askAnswer{"keep": keep}
			if tt.wantDrawn {
				answer.Drawn = true
				wantAnswers[waits] = answer
			}
			got := reloaded.threads[threadKey(card.channel, card.thread)]
			if got == nil || !reflect.DeepEqual(got.Answers, wantAnswers) {
				t.Fatalf("reloaded thread = %+v, want answers %+v", got, wantAnswers)
			}
			if got.Reply.Tapped != tt.wantTapped {
				t.Errorf("reloaded tap = %q, want %q", got.Reply.Tapped, tt.wantTapped)
			}
			if !reflect.DeepEqual(reloaded.refs, cards.refs) {
				t.Errorf("reloaded refs = %+v, want %+v", reloaded.refs, cards.refs)
			}
			checkFiles := unchangedLiveFiles(t, reloaded)
			if got := tt.run(reloaded, card); len(got) != 0 {
				t.Errorf("repeated mutation = %v, want no cards", got)
			}
			checkFiles()
		})
	}
}
