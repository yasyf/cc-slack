package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"

	"github.com/yasyf/cc-interact/cmd"
	"github.com/yasyf/cc-interact/daemon"
	"github.com/yasyf/cc-interact/version"
	"github.com/yasyf/daemonkit"
)

const (
	invokeUse  = "invoke"
	handoffEnv = "CC_SLACK_HANDED_OFF_TO"
)

func newerIncumbent(health daemonkit.Health, floor string) (path, release string, err error) {
	var detail daemon.HealthDetail
	if json.Unmarshal(health.Detail, &detail) == nil && version.Newer(detail.RuntimeBuild, floor) {
		path, err = executablePath(health.PID)
		if err != nil {
			return "", "", fmt.Errorf("%s: daemon release %s supersedes %s, and its executable is unreadable: %w", binaryName, detail.RuntimeBuild, floor, err)
		}
		return path, detail.RuntimeBuild, nil
	}
	return "", "", nil
}

func handoffFloor() string {
	if from := os.Getenv(handoffEnv); from != "" && version.Newer(from, appVersion) {
		return from
	}
	return appVersion
}

func incumbentBinary(ctx context.Context) (path, release string, err error) {
	if health, probeErr := probeIncumbent(ctx); probeErr == nil {
		return newerIncumbent(health, handoffFloor())
	}
	return "", "", nil
}

func execIncumbent(ctx context.Context) error {
	path, release, err := incumbentBinary(ctx)
	if err != nil || path == "" {
		return err
	}
	return syscall.Exec(path, append([]string{path}, os.Args[1:]...), handedOffEnv(release)) //nolint:gosec // G204: argv to the incumbent cc-slack daemon's own executable, never a shell
}

// daemonkit.Stable rereads os.Executable, which keeps the shared link's path, and an old
// launcher's non-atomic relink can unlink it mid-read.
func execResolvedExecutable() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("%s: resolve the running executable: %w", binaryName, err)
	}
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return fmt.Errorf("%s: resolve %s: %w", binaryName, exe, err)
	}
	if resolved == exe {
		return nil
	}
	return syscall.Exec(resolved, append([]string{resolved}, os.Args[1:]...), os.Environ()) //nolint:gosec // G204: argv to this same cc-slack build at its resolved path, never a shell
}

func handedOffEnv(release string) []string {
	return append(os.Environ(), handoffEnv+"="+release)
}

func executablePath(pid int) (string, error) {
	args, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return "", fmt.Errorf("read pid %d arguments: %w", pid, err)
	}
	if len(args) < 4 {
		return "", fmt.Errorf("pid %d arguments: %d bytes", pid, len(args))
	}
	path, _, ok := bytes.Cut(args[4:], []byte{0})
	if !ok || !filepath.IsAbs(string(path)) {
		return "", fmt.Errorf("pid %d exec path %q is not absolute", pid, path)
	}
	return string(path), nil
}

func invokeVia(ctx context.Context, build, release, session, scope string, op daemon.Op, body json.RawMessage) (daemon.Reply, error) {
	c := exec.CommandContext(ctx, build, invokeUse, "--op", string(op), "--session", session, "--cwd", scope) //nolint:gosec // G204: argv to a sibling cc-slack build, never a shell
	c.Env = handedOffEnv(release)
	c.Stdin = bytes.NewReader(body)
	var stderr bytes.Buffer
	c.Stderr = &stderr
	out, err := c.Output()
	if err != nil {
		return daemon.Reply{}, fmt.Errorf("%s: %w: %s", build, err, bytes.TrimSpace(stderr.Bytes()))
	}
	_, _ = os.Stderr.Write(stderr.Bytes())
	var reply daemon.Reply
	if err := json.Unmarshal(out, &reply); err != nil {
		return daemon.Reply{}, fmt.Errorf("%s: decode reply: %w", build, err)
	}
	if !reply.OK {
		return reply, errors.New(reply.Error)
	}
	return reply, nil
}

func invokeCmd(d cmd.Deps) *cobra.Command {
	var (
		f  sessionFlags
		op string
	)
	c := &cobra.Command{
		Use:    invokeUse,
		Hidden: true,
		Short:  "Run one daemon op for an older cc-slack build: body on stdin, reply JSON on stdout",
		Args:   cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			body, err := io.ReadAll(c.InOrStdin())
			if err != nil {
				return err
			}
			reply, err := invoke(c.Context(), d, f.session, cwdOr(f.cwd), daemon.Op(op), body)
			if err != nil && reply.Error == "" {
				return err
			}
			return json.NewEncoder(c.OutOrStdout()).Encode(reply)
		},
	}
	f.bind(c)
	c.Flags().StringVar(&op, "op", "", "daemon op")
	return c
}
