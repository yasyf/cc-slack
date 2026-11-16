package main

import (
	"context"
	"testing"
	"time"
)

func TestStoppingWaitsForQueuedTapsUpToItsGrace(t *testing.T) {
	q := tapQueue{last: map[string]chan struct{}{}}
	release := make(chan struct{})
	finished := false
	q.run(context.Background(), "C1/1.1", func(context.Context) {
		<-release
		finished = true
	})
	if q.drain(10 * time.Millisecond) {
		t.Fatal("drain returned while a tap was still running")
	}
	close(release)
	if !q.drain(time.Second) || !finished {
		t.Fatal("drain gave up on a tap that finished within its grace")
	}
}
