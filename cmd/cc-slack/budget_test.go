package main

import (
	"errors"
	"testing"
	"time"

	"github.com/slack-go/slack"
)

func TestSessionReadsYieldWhileThePollerWaitsOnHisConversation(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	b := repliesBudget{now: func() time.Time { return now }}
	key := cursorKey{"D1", "1800000000.000100"}
	tests := []struct {
		name  string
		step  func()
		yield bool
	}{
		{"nothing waits", func() {}, false},
		{"the poller is limited on his thread", func() { b.pollerWaits(key, true) }, true},
		{"its read goes through", func() { b.pollerWaits(key, false) }, false},
		{"limited again", func() { b.pollerWaits(key, true) }, true},
		{"the wait outlives its watch", func() { now = now.Add(pollerWaitTTL) }, false},
	}
	for _, tt := range tests {
		tt.step()
		err := b.yield()
		if got := err != nil; got != tt.yield {
			t.Fatalf("%s: yield = %v, want a refusal %v", tt.name, err, tt.yield)
		}
		if err != nil {
			if _, ok := errors.AsType[*slack.RateLimitedError](err); !ok {
				t.Fatalf("%s: refusal = %T, want a rate limit the read waits out", tt.name, err)
			}
		}
	}
}
