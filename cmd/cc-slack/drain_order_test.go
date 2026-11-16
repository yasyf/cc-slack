package main

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/yasyf/cc-interact/daemon"
)

func TestKeepIncumbentServesThroughADeferredUpgrade(t *testing.T) {
	refused := errors.New("refused")
	for _, tt := range []struct {
		name   string
		ensure error
		want   error
	}{
		{"deferred upgrade", fmt.Errorf("%w: no time", daemon.ErrUpgradeDeferred), nil},
		{"other failure", refused, refused},
		{"current daemon", nil, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := keepIncumbent(func(context.Context) error { return tt.ensure })(context.Background())
			if !errors.Is(got, tt.want) {
				t.Fatalf("keepIncumbent over %v = %v, want %v", tt.ensure, got, tt.want)
			}
		})
	}
}

func TestTapsSettleInsideTheDrain(t *testing.T) {
	q := tapQueue{last: map[string]chan struct{}{}}
	release := make(chan struct{})
	q.run(context.Background(), "C1/1.0", func(context.Context) { <-release })
	ctx, cancel := context.WithCancel(context.Background())
	settled := make(chan struct{})
	go func() {
		q.settle(ctx)
		close(settled)
	}()
	cancel()
	select {
	case <-settled:
		t.Fatal("settle returned with a tap still running")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	select {
	case <-settled:
	case <-time.After(5 * time.Second):
		t.Fatal("settle did not return once the tap finished")
	}
}
