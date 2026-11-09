package main

import (
	"fmt"
	"io"
	"log"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAPRRowShowsItsTitleAndNeverItsCommit(t *testing.T) {
	p := cardPR{URL: "https://github.com/acme/app/pull/41", Title: "Fix the login form", Head: "abc1234def"}
	v := cardView{refs: map[string]refState{}}
	for _, answered := range []bool{true, false} {
		if got := string(mustJSON(p.blocks(v, answered))); !strings.Contains(got, "Fix the login form") || strings.Contains(got, "abc1234") {
			t.Fatalf("answered=%v row = %s, want the PR's title and no commit", answered, got)
		}
	}
}

func TestAnEditThatTakesAPROffItsPostDropsThatPRsApprove(t *testing.T) {
	now := time.Unix(1800000000, 0)
	l := &liveCards{
		path:    filepath.Join(t.TempDir(), "live-cards.json"),
		log:     log.New(io.Discard, "", 0),
		now:     func() time.Time { return now },
		wakeCh:  make(chan struct{}, 1),
		threads: map[string]*liveThread{},
		refs:    map[string]refState{},
		hosts:   map[string]*refHost{},
		drawing: map[string]*sync.Mutex{},
		undrawn: map[string]map[string]bool{},
	}
	const channel, cards, post = "D1", "1800000000.000200", "1800000000.000100"
	prCard := func(n int) *liveMessage {
		return &liveMessage{TS: fmt.Sprintf("1800000000.0002%02d", n-40), Card: statusCard{PRs: []cardPR{{URL: fmt.Sprintf("https://github.com/acme/app/pull/%d", n)}}}}
	}
	l.threads[threadKey(channel, cards)] = &liveThread{Channel: channel, Thread: cards, Updated: now, PRCards: []*liveMessage{prCard(41), prCard(42)}}
	l.threads[threadKey(channel, post)] = &liveThread{Channel: channel, Thread: post, Updated: now, Carries: []string{prCard(41).TS, prCard(42).TS}}
	still, _ := parseRef("https://github.com/acme/app/pull/42")

	l.keepCarried(channel, post, map[string]bool{still.key(): true}, now)

	if got := l.threads[threadKey(channel, post)].Carries; !slices.Equal(got, []string{prCard(42).TS}) {
		t.Fatalf("carried cards = %v, want only #42's, since the edit took #41 off the post", got)
	}
}
