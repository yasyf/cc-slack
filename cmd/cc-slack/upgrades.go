package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/yasyf/cc-interact/daemon"
	"github.com/yasyf/cc-interact/version"
)

const (
	upgradeEvery = 15 * time.Minute
	tapDrain     = 20 * time.Second
)

func daemonStartedPath() string { return filepath.Join(appPaths().StateDir(), "daemon-started") }

func markDaemonStarted() error {
	return os.WriteFile(daemonStartedPath(), []byte(appVersion), 0o600)
}

const unknownOpPrefix = "unknown op: "

type forceUpgradeKey struct{}

func forcingUpgrade(ctx context.Context) context.Context {
	return context.WithValue(ctx, forceUpgradeKey{}, true)
}

func upgradeForced(ctx context.Context) bool {
	forced, _ := ctx.Value(forceUpgradeKey{}).(bool)
	return forced
}

func coalesceUpgrade(ensure func(context.Context) error) func(context.Context) error {
	return func(ctx context.Context) error {
		if !upgradeForced(ctx) && upgradeHeld(ctx, time.Now()) {
			return nil
		}
		return ensure(ctx)
	}
}

func upgradeHeld(ctx context.Context, now time.Time) bool {
	health, err := probeIncumbent(ctx)
	if err != nil {
		return false
	}
	var detail daemon.HealthDetail
	if json.Unmarshal(health.Detail, &detail) != nil || !version.Newer(appVersion, detail.RuntimeBuild) {
		return false
	}
	started, err := os.Stat(daemonStartedPath())
	return err == nil && now.Sub(started.ModTime()) < upgradeEvery
}

func (q *tapQueue) drain(grace time.Duration) bool {
	done := make(chan struct{})
	go func() {
		q.busy.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(grace):
		return false
	}
}
