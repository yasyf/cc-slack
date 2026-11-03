package main

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"syscall"
	"testing"
	"time"
)

func missingBinary(bin string) error {
	return &fs.PathError{Op: "fork/exec", Path: bin, Err: syscall.ENOENT}
}

func TestAWriteWaitsOutACaptHookReinstall(t *testing.T) {
	calls := 0
	run := waitingForReinstall(time.Minute, time.Millisecond, func(_ context.Context, bin string, _ ...string) ([]byte, error) {
		calls++
		if calls < 3 {
			return nil, missingBinary(bin)
		}
		return []byte("spent"), nil
	})

	out, err := run(context.Background(), "/bin/capt-hook", "ticket", "spend")

	if err != nil || string(out) != "spent" || calls != 3 {
		t.Fatalf("out %q, err %v, %d calls; want it spent on the third call", out, err, calls)
	}
}

func TestACaptHookThatStaysMissingSaysSo(t *testing.T) {
	run := waitingForReinstall(0, time.Millisecond, func(_ context.Context, bin string, _ ...string) ([]byte, error) {
		return nil, missingBinary(bin)
	})

	_, err := run(context.Background(), "/bin/capt-hook", "ticket", "spend")

	if !errors.Is(err, errCaptHookMissing) || !strings.Contains(err.Error(), "/bin/capt-hook") {
		t.Fatalf("err %v, want capt-hook named missing at its path", err)
	}
}

func TestARefusalFromCaptHookIsNotRetried(t *testing.T) {
	calls := 0
	refused := errors.New("ticket expired")
	run := waitingForReinstall(time.Minute, time.Millisecond, func(context.Context, string, ...string) ([]byte, error) {
		calls++
		return nil, refused
	})

	if _, err := run(context.Background(), "/bin/capt-hook", "ticket", "spend"); !errors.Is(err, refused) || calls != 1 {
		t.Fatalf("err %v, %d calls; want the refusal returned at once", err, calls)
	}
}

func TestAMissingCaptHookIsNotBlamedOnTheTicket(t *testing.T) {
	l := startDaemon(t, func(a *app) {
		a.captHook = waitingForReinstall(0, time.Millisecond, func(_ context.Context, bin string, _ ...string) ([]byte, error) {
			return nil, missingBinary(bin)
		})
	})
	loggedIn(t, l)

	l.refused(opSend, l.spending("0123456789ab", map[string]any{"channel_id": testChannel, "text": "PR is up"}), "capt-hook is missing at")
}
