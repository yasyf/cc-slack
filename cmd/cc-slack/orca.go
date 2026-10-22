package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	orcaReadyWait     = 60 * time.Second
	orcaReadyPoll     = 5 * time.Second
	orcaCreateTimeout = 10 * time.Minute
	orcaSettleWait    = 10 * time.Minute
	orcaSettlePoll    = 10 * time.Second
	maxOrcaErrorBytes = 500

	orcaRuntimeUnavailable = "runtime_unavailable"
)

type orcaRunner func(ctx context.Context, bin string, args ...string) ([]byte, error)

func execOrca(ctx context.Context, bin string, args ...string) ([]byte, error) {
	var stderr bytes.Buffer
	c := exec.CommandContext(ctx, bin, args...) //nolint:gosec // G204: argv to the orca binary login recorded, never a shell
	c.Stderr = &stderr
	out, err := c.Output()
	if err != nil {
		return out, fmt.Errorf("%w: %s", err, clip(stderr.String()))
	}
	return out, nil
}

type orcaEnvelope[T any] struct {
	OK     bool `json:"ok"`
	Result T    `json:"result"`
	Error  *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type orcaStatus struct {
	Runtime struct {
		Reachable bool   `json:"reachable"`
		State     string `json:"state"`
	} `json:"runtime"`
}

type orcaRepos struct {
	Repos []struct {
		ID   string `json:"id"`
		Path string `json:"path"`
	} `json:"repos"`
}

type orcaWorktree struct {
	ID   string `json:"id"`
	Path string `json:"path"`
}

type orcaCreateResult struct {
	Worktree orcaWorktree `json:"worktree"`
}

type orcaWorktrees struct {
	Worktrees []orcaWorktree `json:"worktrees"`
}

type orcaError struct {
	Code    string
	Message string
}

func (e *orcaError) Error() string { return fmt.Sprintf("%s (%s)", e.Message, e.Code) }

func orcaCall[T any](ctx context.Context, run orcaRunner, bin string, args ...string) (T, error) {
	var env orcaEnvelope[T]
	out, runErr := run(ctx, bin, args...)
	if err := json.Unmarshal(out, &env); err != nil {
		if runErr != nil {
			return env.Result, runErr
		}
		return env.Result, fmt.Errorf("orca %s: unreadable output %q: %w", args[0], clip(string(out)), err)
	}
	if !env.OK {
		if env.Error != nil {
			return env.Result, &orcaError{Code: env.Error.Code, Message: env.Error.Message}
		}
		return env.Result, fmt.Errorf("orca %s answered ok: false", args[0])
	}
	return env.Result, runErr
}

func clip(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > maxOrcaErrorBytes {
		return s[:maxOrcaErrorBytes] + "…"
	}
	return s
}

func (h *shortcutHandler) waitReachable(ctx context.Context, bin string) error {
	ctx, cancel := context.WithTimeout(ctx, h.readyWait)
	defer cancel()
	for {
		st, err := orcaCall[orcaStatus](ctx, h.orca, bin, "status", "--json")
		if err == nil && st.Runtime.Reachable {
			return nil
		}
		if err == nil {
			err = fmt.Errorf("runtime.reachable is false (state %q)", st.Runtime.State)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("orca is not reachable after %s; start the Orca app and invoke the shortcut again: %w", h.readyWait, err)
		case <-time.After(h.readyPoll):
		}
	}
}

func (h *shortcutHandler) findRepo(ctx context.Context, bin, root string) (string, error) {
	repos, err := orcaCall[orcaRepos](ctx, h.orca, bin, "repo", "list", "--json")
	if err != nil {
		return "", fmt.Errorf("orca repo list: %w", err)
	}
	for _, r := range repos.Repos {
		if filepath.Clean(r.Path) == filepath.Clean(root) {
			return r.ID, nil
		}
	}
	return "", fmt.Errorf("no orca repo has path %s: add the checkout in Orca, or run `cc-slack login --repo <path>` with one it has", root)
}

func (h *shortcutHandler) findWorktree(ctx context.Context, bin, repoID, name string) (orcaWorktree, bool, error) {
	list, err := orcaCall[orcaWorktrees](ctx, h.orca, bin, "worktree", "list", "--repo", "id:"+repoID, "--json")
	if err != nil {
		return orcaWorktree{}, false, fmt.Errorf("orca worktree list: %w", err)
	}
	for _, wt := range list.Worktrees {
		if filepath.Base(wt.Path) == name {
			return wt, true, nil
		}
	}
	return orcaWorktree{}, false, nil
}

func createMaySettle(createCtx context.Context, err error) bool {
	var oe *orcaError
	return (errors.As(err, &oe) && oe.Code == orcaRuntimeUnavailable) || errors.Is(createCtx.Err(), context.DeadlineExceeded)
}

func (h *shortcutHandler) awaitWorktree(ctx context.Context, bin, repoID, name string, cause error) (orcaWorktree, error) {
	ctx, cancel := context.WithTimeout(ctx, h.settleWait)
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			return orcaWorktree{}, fmt.Errorf("%w; no worktree %s appeared within %s", cause, name, h.settleWait)
		case <-time.After(h.settlePoll):
		}
		wt, found, err := h.findWorktree(ctx, bin, repoID, name)
		if err != nil && ctx.Err() == nil {
			h.log.Printf("shortcut: waiting for worktree %s: %v", name, err)
		}
		if found {
			return wt, nil
		}
	}
}
