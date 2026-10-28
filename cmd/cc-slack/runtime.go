package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/yasyf/cc-interact/daemon"
	"github.com/yasyf/daemonkit"
	"github.com/yasyf/daemonkit/paths"
)

var appVersion = "0.0.0"

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
	program, err := daemonkit.Stable(appVersion)
	if err != nil {
		return daemonkit.Daemon{}, fmt.Errorf("build stable daemon program: %w", err)
	}
	return daemon.Spec(daemonkit.Daemon{
		Label:   serviceLabel,
		Program: program,
		Args:    []string{"daemon"},
		Log:     appPaths().LogPath(),
		Restart: daemonkit.RestartOnFailure,
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
		errors.Is(err, daemonkit.ErrNotReady))
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
	defer func() { _ = client.Close(ctx) }()
	return client.Health(ctx)
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
