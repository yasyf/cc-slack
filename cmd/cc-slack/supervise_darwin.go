package main

import (
	"context"
	"errors"
)

func runSupervisor(context.Context) error {
	return errors.New("cc-slack supervise runs on linux only; launchd supervises the daemon on macOS")
}
