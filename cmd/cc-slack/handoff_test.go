package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yasyf/cc-interact/daemon"
	"github.com/yasyf/daemonkit"
)

const fakeReleaseEnv = "CC_SLACK_TEST_RELEASE"

func TestMain(m *testing.M) {
	if release := os.Getenv(fakeReleaseEnv); release != "" && len(os.Args) == 2 && os.Args[1] == "--version" {
		fmt.Printf("%s version %s\n", binaryName, release)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestNewerForeignIncumbentOrdersTheSocketHoldersRelease(t *testing.T) {
	dir, err := os.MkdirTemp("", "ccs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "d.sock")
	ln, err := new(net.ListenConfig).Listen(context.Background(), "unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, release, want string
	}{
		{"newer release hands off", "0.4.6", self},
		{"same release stays", "0.4.5", ""},
		{"older release stays", "0.2.3", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(fakeReleaseEnv, tc.release)
			got, release, err := newerForeignIncumbent(context.Background(), socket, "0.4.5")
			if err != nil || got != tc.want {
				t.Fatalf("newerForeignIncumbent = %q, %q, %v, want %q", got, release, err, tc.want)
			}
			if got != "" && release != tc.release {
				t.Fatalf("release = %q, want %q", release, tc.release)
			}
		})
	}
}

func TestNewerForeignIncumbentRefusesAnEmptySocket(t *testing.T) {
	_, _, err := newerForeignIncumbent(context.Background(), filepath.Join(t.TempDir(), "absent.sock"), "0.4.5")
	if err == nil || !strings.Contains(err.Error(), "its process is unreadable") {
		t.Fatalf("newerForeignIncumbent = %v, want the unreadable-process refusal", err)
	}
}

func TestReportedReleaseRefusesAForeignVersionLine(t *testing.T) {
	build := filepath.Join(t.TempDir(), "cc-slack")
	if err := os.WriteFile(build, []byte("#!/bin/sh\necho 'other version 1.0'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := reportedRelease(context.Background(), build); err == nil || !strings.Contains(err.Error(), "other version 1.0") {
		t.Fatalf("reportedRelease = %v, want the unparsed version line", err)
	}
}

func TestNewerIncumbentNamesTheDaemonsOwnExecutable(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	detail := func(build string) []byte {
		raw, err := json.Marshal(daemon.HealthDetail{RuntimeBuild: build})
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	cases := []struct {
		name   string
		health daemonkit.Health
		want   string
	}{
		{"newer release hands off", daemonkit.Health{PID: os.Getpid(), Detail: detail("0.1.1791094166")}, self},
		{"newer release while draining hands off", daemonkit.Health{Phase: daemonkit.PhaseDraining, PID: os.Getpid(), Detail: detail("0.1.1791094166")}, self},
		{"same release stays", daemonkit.Health{PID: os.Getpid(), Detail: detail("0.1.1791092636")}, ""},
		{"older release stays", daemonkit.Health{PID: os.Getpid(), Detail: detail("0.1.1791000000")}, ""},
		{"no detail stays", daemonkit.Health{PID: os.Getpid()}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _, err := newerIncumbent(tc.health, "0.1.1791092636")
			if err != nil || got != tc.want {
				t.Fatalf("newerIncumbent = %q, %v, want %q", got, err, tc.want)
			}
		})
	}
}

func TestNewerIncumbentRefusesAnUnreadableExecutable(t *testing.T) {
	raw, err := json.Marshal(daemon.HealthDetail{RuntimeBuild: "0.1.1791094166"})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = newerIncumbent(daemonkit.Health{PID: math.MaxInt32, Detail: raw}, "0.1.1791092636")
	if err == nil || !strings.Contains(err.Error(), "0.1.1791094166 supersedes 0.1.1791092636") {
		t.Fatalf("newerIncumbent = %v, want the unreadable-executable refusal", err)
	}
}

func TestHandoffFloorNeverHandsOffBelowTheReleaseAlreadyHandedOffTo(t *testing.T) {
	prev := appVersion
	appVersion = "0.1.1791092636"
	t.Cleanup(func() { appVersion = prev })
	cases := []struct{ env, want string }{
		{"", "0.1.1791092636"},
		{"0.1.1791094166", "0.1.1791094166"},
		{"0.1.1791000000", "0.1.1791092636"},
	}
	for _, tc := range cases {
		t.Setenv(handoffEnv, tc.env)
		if got := handoffFloor(); got != tc.want {
			t.Errorf("handoffFloor with %q = %s, want %s", tc.env, got, tc.want)
		}
	}
}

func TestExecutablePathReadsTheKernelsExecPath(t *testing.T) {
	c := exec.Command("/bin/sleep", "5")
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Process.Kill(); _ = c.Wait() })
	got, err := executablePath(c.Process.Pid)
	if err != nil || got != "/bin/sleep" {
		t.Fatalf("executablePath = %q, %v, want /bin/sleep", got, err)
	}
}

func TestInvokeViaRunsTheOpThroughTheNewerBuild(t *testing.T) {
	dir := t.TempDir()
	build := filepath.Join(dir, "cc-slack-1790888838-23c738bd415a")
	script := `#!/bin/sh
[ "$CC_SLACK_HANDED_OFF_TO" = 0.1.1790888838 ] || { echo "handoff env $CC_SLACK_HANDED_OFF_TO" >&2; exit 4; }
body="$(cat)"
case "$*" in
"invoke --op send --session s1 --cwd /scope") printf '{"ok":true,"body":%s}\n' "$body" ;;
"invoke --op react --session s1 --cwd /scope") printf '{"ok":false,"error":"not_in_channel"}\n' ;;
*) echo "unexpected args: $*" >&2; exit 3 ;;
esac
`
	if err := os.WriteFile(build, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	reply, err := invokeVia(ctx, build, "0.1.1790888838", "s1", "/scope", opSend, json.RawMessage(`{"text":"hi"}`))
	if err != nil || !reply.OK || string(reply.Body) != `{"text":"hi"}` {
		t.Fatalf("send = %+v, %v", reply, err)
	}

	reply, err = invokeVia(ctx, build, "0.1.1790888838", "s1", "/scope", opReact, nil)
	if err == nil || reply.Error != "not_in_channel" {
		t.Fatalf("react = %+v, %v, want the refusal reply", reply, err)
	}

	_, err = invokeVia(ctx, build, "0.1.1790888838", "s2", "/scope", opSend, nil)
	if err == nil || !strings.Contains(err.Error(), "unexpected args") {
		t.Fatalf("unexpected session = %v, want the build's stderr", err)
	}
}
