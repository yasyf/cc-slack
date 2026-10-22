package main

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/yasyf/cc-interact/daemon"
	"github.com/yasyf/daemonkit"
)

func reporting(t *testing.T, phase daemonkit.Phase, build string) daemonkit.Health {
	t.Helper()
	detail, err := json.Marshal(daemon.HealthDetail{RuntimeBuild: build})
	if err != nil {
		t.Fatal(err)
	}
	return daemonkit.Health{Phase: phase, Detail: detail}
}

func TestEnsureLeavesADaemonServingThisReleaseRunning(t *testing.T) {
	cases := []struct {
		name    string
		health  daemonkit.Health
		probe   error
		rebuild bool
		ensures bool
	}{
		{"same release from another checkout", reporting(t, daemonkit.PhaseReady, appVersion), nil, false, false},
		{"same release under --rebuild", reporting(t, daemonkit.PhaseReady, appVersion), nil, true, true},
		{"older incumbent", reporting(t, daemonkit.PhaseReady, "0.0.0-older"), nil, false, true},
		{"newer incumbent", reporting(t, daemonkit.PhaseReady, "0.1.9999999999"), nil, false, true},
		{"same release still starting", reporting(t, daemonkit.PhaseStarting, appVersion), nil, false, true},
		{"same release failed", reporting(t, daemonkit.PhaseFailed, appVersion), nil, false, true},
		{"no release detail", daemonkit.Health{Phase: daemonkit.PhaseReady}, nil, false, true},
		{"nothing serving", daemonkit.Health{}, daemonkit.ErrAbsent, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prev := rebuild
			rebuild = tc.rebuild
			t.Cleanup(func() { rebuild = prev })
			ensured := false
			sentinel := errors.New("ensured")
			err := unlessServingRelease(
				func(context.Context) (daemonkit.Health, error) { return tc.health, tc.probe },
				func(context.Context) error { ensured = true; return sentinel },
			)(context.Background())
			if ensured != tc.ensures {
				t.Fatalf("ensured = %v, want %v", ensured, tc.ensures)
			}
			if tc.ensures != errors.Is(err, sentinel) {
				t.Fatalf("err = %v, want the ensure result exactly when it ran", err)
			}
		})
	}
}

func daemonStatus(ctx context.Context) error {
	client, err := newClient(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	_, err = client.Do(ctx, daemon.Envelope{Op: daemon.OpStatus})
	return err
}

func TestAcrossRestartResendsACallTheRestartRefused(t *testing.T) {
	shortHome(t)
	if err := daemonStatus(context.Background()); !restarting(err) {
		t.Fatalf("status with no daemon listening = %v, want a restart refusal", err)
	}
	awaited := false
	err := acrossRestart(func(context.Context) error {
		awaited = true
		launchDaemon(t)
		return nil
	}, daemonStatus)(context.Background())
	if err != nil || !awaited {
		t.Fatalf("status across a restart = %v (awaited %v), want the resent call to succeed", err, awaited)
	}
}

func TestAcrossRestartReportsARestartThatNeverSettles(t *testing.T) {
	shortHome(t)
	stalled := errors.New("restart window spent")
	calls := 0
	err := acrossRestart(func(context.Context) error { return stalled }, func(ctx context.Context) error {
		calls++
		return daemonStatus(ctx)
	})(context.Background())
	if calls != 1 || !errors.Is(err, stalled) || !errors.Is(err, daemonkit.ErrAbsent) {
		t.Fatalf("calls = %d, err = %v, want one call and both the refusal and the stalled wait", calls, err)
	}
}

func TestAcrossRestartPassesOtherOutcomesThrough(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"success", nil},
		{"product failure", errors.New("channel_not_found")},
		{"absence outside a call", daemonkit.ErrAbsent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			err := acrossRestart(func(context.Context) error {
				t.Fatal("waited for a restart nothing refused")
				return nil
			}, func(context.Context) error {
				calls++
				return tc.err
			})(context.Background())
			if calls != 1 || !errors.Is(err, tc.err) || (tc.err == nil) != (err == nil) {
				t.Fatalf("calls = %d, err = %v, want one call returning %v", calls, err, tc.err)
			}
		})
	}
}
