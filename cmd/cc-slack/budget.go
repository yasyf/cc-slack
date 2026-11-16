package main

import (
	"maps"
	"sync"
	"time"

	"github.com/slack-go/slack"
)

const (
	sessionYield  = 5 * time.Second
	pollerWaitTTL = 2 * time.Minute
)

type repliesBudget struct {
	mu      sync.Mutex
	waiting map[cursorKey]time.Time
	now     func() time.Time
}

func (b *repliesBudget) clock() time.Time {
	if b.now == nil {
		return time.Now()
	}
	return b.now()
}

func (b *repliesBudget) pollerWaits(key cursorKey, waits bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !waits {
		delete(b.waiting, key)
		return
	}
	if b.waiting == nil {
		b.waiting = map[cursorKey]time.Time{}
	}
	b.waiting[key] = b.clock()
}

func (b *repliesBudget) yield() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.clock()
	maps.DeleteFunc(b.waiting, func(_ cursorKey, since time.Time) bool { return now.Sub(since) >= pollerWaitTTL })
	if len(b.waiting) == 0 {
		return nil
	}
	return &slack.RateLimitedError{RetryAfter: sessionYield}
}
