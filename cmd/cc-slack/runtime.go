package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/yasyf/cc-interact/daemon"
	"github.com/yasyf/daemonkit"
	"github.com/yasyf/daemonkit/paths"
)

var (
	appVersion = "0.0.0"
	rebuild    bool
)

const (
	binaryName   = "cc-slack"
	appDir       = ".local/state/cc-slack"
	serviceLabel = "com.yasyf.cc-slack"
	scopeName    = "cc-slack"
	statusOpen   = "open"
	statusClosed = "closed"
)

func appPaths() paths.Paths { return paths.Paths{App: appDir} }

func appSpec() (daemonkit.Daemon, error) {
	program, err := daemonkit.Stable()
	if err != nil {
		return daemonkit.Daemon{}, fmt.Errorf("build stable daemon program: %w", err)
	}
	return daemon.Spec(daemonkit.Daemon{
		Label:   serviceLabel,
		Program: program,
		Args:    []string{"daemon"},
		Log:     appPaths().LogPath(),
		Restart: daemonkit.RestartOnFailure,
		Trust:   daemonkit.Trust{Serving: daemonkit.ServingSameUser()},
	}), nil
}

var launcher = sync.OnceValue(func() daemon.Launcher {
	spec, err := appSpec()
	if err != nil {
		panic(err)
	}
	return daemon.Launcher{Daemon: spec, Paths: appPaths(), RuntimeBuild: appVersion}
})

func restarting(err error) bool {
	return daemonkit.Undispatched(err) && (errors.Is(err, daemonkit.ErrAbsent) ||
		errors.Is(err, daemonkit.ErrDraining) ||
		errors.Is(err, daemonkit.ErrNotReady) ||
		errors.Is(err, daemonkit.ErrPeerGone))
}

func awaitRestart(ctx context.Context) error {
	client, err := daemonkit.Open(launcher().Daemon)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, daemon.UpgradeTimeout)
	defer cancel()
	_, err = client.WaitReady(ctx)
	return err
}

func acrossRestart(await, call func(context.Context) error) func(context.Context) error {
	return func(ctx context.Context) error {
		err := call(ctx)
		if !restarting(err) {
			return err
		}
		if waitErr := await(ctx); waitErr != nil {
			return errors.Join(err, waitErr)
		}
		return call(ctx)
	}
}

func probeIncumbent(ctx context.Context) (daemonkit.Health, error) {
	client, err := daemonkit.Open(launcher().Daemon)
	if err != nil {
		return daemonkit.Health{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, daemon.UpgradeTimeout)
	defer cancel()
	control, err := client.Control(ctx)
	if err != nil {
		return daemonkit.Health{}, err
	}
	defer func() { _ = control.Close(ctx) }()
	return control.Health(ctx)
}

func servesRelease(health daemonkit.Health, build string) bool {
	var detail daemon.HealthDetail
	if health.Phase != daemonkit.PhaseReady || json.Unmarshal(health.Detail, &detail) != nil {
		return false
	}
	return detail.RuntimeBuild == build
}

func unlessServingRelease(probe func(context.Context) (daemonkit.Health, error), ensure func(context.Context) error) func(context.Context) error {
	return func(ctx context.Context) error {
		if !rebuild {
			if health, err := probe(ctx); err == nil && servesRelease(health, appVersion) {
				return nil
			}
		}
		return ensure(ctx)
	}
}

func newClient(context.Context) (*daemon.Client, error) { return launcher().NewClient() }

func cwdOr(cwd string) string {
	if cwd != "" {
		return cwd
	}
	d, err := os.Getwd()
	if err != nil {
		panic(err)
	}
	return d
}
