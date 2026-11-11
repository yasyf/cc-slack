package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckPostFindsTheNewestPluginVersionInUseWithoutTheLauncher(t *testing.T) {
	config := t.TempDir()
	t.Setenv(pluginRootEnv, "")
	t.Setenv("CLAUDE_CONFIG_DIR", config)
	versions := filepath.Join(config, "plugins", "cache", channelPlugin.Marketplace, channelPlugin.Name)
	for _, v := range []string{"0.9.4", "0.11.2", "0.13.0", "scratch"} {
		if err := os.MkdirAll(filepath.Join(versions, v), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(versions, "0.13.0", ".orphaned_at"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if root, err := pluginRoot(); err != nil || root != filepath.Join(versions, "0.11.2") {
		t.Fatalf("plugin root = %q, %v; want 0.11.2, the newest version Claude Code has not orphaned", root, err)
	}
	launcher := t.TempDir()
	if err := os.MkdirAll(filepath.Join(launcher, filepath.Dir(checkPostScript)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(launcher, checkPostScript), nil, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(pluginRootEnv, launcher)
	if root, err := pluginRoot(); err != nil || root != launcher {
		t.Fatalf("plugin root = %q, %v; want the launcher's root", root, err)
	}
	t.Setenv(pluginRootEnv, t.TempDir())
	if root, err := pluginRoot(); err != nil || root != filepath.Join(versions, "0.11.2") {
		t.Fatalf("plugin root = %q, %v; want 0.11.2 when the inherited root is another plugin's", root, err)
	}
}

func TestCheckPostFlagsABadPostAndPassesACleanOneWithTheScriptsExitStatus(t *testing.T) {
	plugin, err := filepath.Abs(filepath.Join("..", "..", "plugin"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(pluginRootEnv, plugin)
	cases := map[string]struct {
		text     string
		status   exitStatus
		findings []string
	}{
		"bad":                 {"Hi Anubhav, I rolled restate to 06ca99cdcd.", 1, []string{"plain-name 'Hi Anubhav'", "commit-sha '06ca99cdcd'"}},
		"clean":               {"The release is live, and every chat opens again.", 0, nil},
		"waits with no card":  {"The Hima fix waits on your pick.", 1, []string{`waits-without-card "The Hima fix waits on your pick"`}},
		"waits with its card": {"The Hima fix waits on your pick: https://acme.slack.com/archives/D0C4HQWPLCC/p1794277125284779", 0, nil},
		"card link as its own label": {"The Hima fix waits on your pick: <https://acme.slack.com/archives/D0C4HQWPLCC/p1794277125284779|https://acme.slack.com/archives/D0C4HQWPLCC/p1794277125284779>", 1,
			[]string{`wrapped-card-link "<https://acme.slack.com/archives/D0C4HQWPLCC/p1794277125284779|https://acme.slack.com/archives/D0C4HQWPLCC/p1794277125284779>"`}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			check := checkPostCmd()
			check.SetArgs([]string{"-"})
			check.SetIn(strings.NewReader(tc.text))
			check.SetOut(&out)
			check.SetErr(&out)
			err := check.Execute()
			status, exited := errors.AsType[exitStatus](err)
			if err != nil && !exited || status != tc.status {
				t.Fatalf("check-post = %v, want exit status %d", err, tc.status)
			}
			for _, want := range tc.findings {
				if !strings.Contains(out.String(), want) {
					t.Fatalf("output = %q, want %q in it", out.String(), want)
				}
			}
			if tc.findings == nil && out.Len() != 0 {
				t.Fatalf("output = %q, want none for a clean post", out.String())
			}
		})
	}
}
