package main

import (
	"context"
	"time"
)

const hookDeadline = 15 * time.Second

const unknownOpPrefix = "unknown op: "

type forceUpgradeKey struct{}

func forcingUpgrade(ctx context.Context) context.Context {
	return context.WithValue(ctx, forceUpgradeKey{}, true)
}

func upgradeForced(ctx context.Context) bool {
	forced, _ := ctx.Value(forceUpgradeKey{}).(bool)
	return forced
}

func (q *tapQueue) settle(ctx context.Context) {
	<-ctx.Done()
	q.busy.Wait()
}
