package main

import (
	"context"

	"github.com/yasyf/daemonkit"
)

func runSupervisor(ctx context.Context) error {
	return daemonkit.Supervise(ctx, serviceLabel)
}
