package main

import (
	"bytes"
	"errors"
	"fmt"
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
		"waits with its card": {"The Hima fix waits on your pick: <https://acme.slack.com/archives/D0C4HQWPLCC/p1794277125284779|the Hima card>", 0, nil},
		"bare card link": {"The Hima fix waits on your pick: <https://acme.slack.com/archives/D0C4HQWPLCC/p1794277125284779>", 1,
			[]string{`raw-card-link "https://acme.slack.com/archives/D0C4HQWPLCC/p1794277125284779"`}},
		"card link as its own label": {"The Hima fix waits on your pick: <https://acme.slack.com/archives/D0C4HQWPLCC/p1794277125284779|acme.slack.com/archives/D0C4HQWPLCC/p1794277125284779>", 1,
			[]string{`raw-card-link "https://acme.slack.com/archives/D0C4HQWPLCC/p1794277125284779"`}},
		"six cards on one line":       {"Tonight: " + strings.Join(dmCardLinks(6), ", "), 1, []string{"1:1 links-past-five: line 1 links 6 Slack messages"}},
		"six cards a line each":       {strings.Join(dmCardLinks(6), "\n"), 0, nil},
		"six cards in a table row":    {"| When | Cards |\n|---|---|\n| tonight | " + strings.Join(dmCardLinks(6), " ") + " |", 0, nil},
		"six room messages, no cards": {"Tonight: " + strings.Join(roomLinks(1, 6), ", "), 0, nil},
		"wall of text":                {"The build is red. The cause is a flaky test. A retry is running. It ends at 5:25pm.", 1, []string{`1:1 wall-of-text "The build is red. The cause": 4 sentences in one paragraph`}},
		"status table":                {stuckTable, 0, nil},
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

func TestCheckPostPassesTheDriveInAnAnswerToAQuestionAboutIt(t *testing.T) {
	plugin, err := filepath.Abs(filepath.Join("..", "..", "plugin"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(pluginRootEnv, plugin)
	cases := map[string]struct {
		asked  []string
		status exitStatus
	}{
		"answers his question about the drive": {[]string{"how far along is the drive?"}, 0},
		"answers a question about the deploy":  {[]string{"is the deploy out?"}, 1},
		"answers nothing":                      {nil, 1},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			check := checkPostCmd()
			args := []string{"-"}
			for _, message := range tc.asked {
				args = append(args, "--asked", message)
			}
			check.SetArgs(args)
			check.SetIn(strings.NewReader("The drive has two PRs left to merge."))
			check.SetOut(&out)
			check.SetErr(&out)
			err := check.Execute()
			status, exited := errors.AsType[exitStatus](err)
			if err != nil && !exited || status != tc.status {
				t.Fatalf("check-post = %v, output %q, want exit status %d", err, out.String(), tc.status)
			}
			if tc.status == 1 && !strings.Contains(out.String(), "drive-term 'The drive'") {
				t.Fatalf("output = %q, want the drive flagged", out.String())
			}
		})
	}
}

const stuckTable = `Four PRs are stuck, each on its own fix:

| PR | Stuck since | Why | Next |
| --- | --- | --- | --- |
| the cc-slack table fix | 2:10 AM | its review asked for tests. | tests are pushed. CI is running. It ends at 2:50. |
| the say hook fix | 2:12 AM | CI is red. The lint step fails. It names one file. | the fix is pushed. |
| the card fix | 1:40 AM | it waits on a rebase. Its base moved. | the rebase runs now. |
| the Jev judge fix | 1:55 AM | it waits on his pick. | nothing until he answers. |`

func TestWallsCountSentencesOnlyInProseAsCCSlackDrawsIt(t *testing.T) {
	cases := map[string]struct {
		text  string
		walls []string
	}{
		"a status table":                         {stuckTable, nil},
		"a table of two-sentence cells":          {"| Fix | State |\n| --- | --- |\n| It is merged. It is live. | It is proven. It is done. |", nil},
		"a heading between two short paragraphs": {"The fix is merged. It is live.\n## Next\nThe release runs tonight. It needs nothing.", nil},
		"a picture between two short paragraphs": {"The fix is merged. It is live.\n![the card after](/tmp/after.png)\nThe release runs tonight. It needs nothing.", nil},
		"a code block of sentences":              {"Run this once.\n```\none. two. three. four. five.\n```", nil},
		"list items between short paragraphs":    {"It is merged. It is live.\n- one. two. three. four.\nIt runs tonight. It needs nothing.", nil},
		"sentences inside links and code":        {"See <https://example.com/a|one. two. three.> and `a. b. c.` now. It is live.", nil},
		"his quoted words":                       {"His o333 words were “Put it on the new tree. Keep the old one. Delete it next week.” It is built. It is live.", nil},
		"his words in straight quotes":           {`He said "Put it on the new tree. Keep the old one. Delete it next week." It is built. It is live.`, nil},
		"a blockquote of his":                    {"It is built. It is live.\n> Put it on the new tree. Keep the old one. Delete it next week.\nIt needs nothing.", nil},
		"four sentences of its own around a quote": {`The build is red. The cause is "a flaky test." A retry is running. It ends at 5:25pm. It needs nothing.`,
			[]string{`1:1 wall-of-text "The build is red. The cause": 4 sentences in one paragraph`}},
		"one paragraph of four sentences": {"The build is red. The cause is a flaky test. A retry is running. It ends at 5:25pm.",
			[]string{`1:1 wall-of-text "The build is red. The cause": 4 sentences in one paragraph`}},
		"a wall after a blank line and a table": {"\n| a | b |\n| --- | --- |\n| 1 | 2 |\n\nOne. *Two.* Three. Four.",
			[]string{`6:1 wall-of-text "One. *Two.* Three. Four.": 4 sentences in one paragraph`}},
		"a dense ask": {"Hi Anubhav, Yasyf asked me to ask you for one thing on our litestream fork. Could you create a fine-grained personal access token with access to only the repository Forge-AI/litestream. Then save it as a repository secret named FORGE_REBASE_TOKEN. It lets the nightly job rebase our patches onto upstream. Please do not send the token to anyone, me included.",
			[]string{`1:1 wall-of-text "Hi Anubhav, Yasyf asked me to": 5 sentences in one paragraph`}},
		"three short lines Slack draws as three paragraphs": {"*Drive fix live: `drive job add` no longer refuses a builder over a design question you already answered.*\nBefore, a research job's design choice blocked every later builder, even after you answered it on cards and the work was built.\n<https://github.com/yasyf/cc-skills/pull/4836|The fix> is live in long-running 1.0.693; its reproduction passes. Nothing waits on you.\n", nil},
		"a line of four sentences between short lines": {"It is merged.\nThe build is red. The cause is a flaky test. A retry is running. It ends at 5:25pm.\nIt needs nothing.",
			[]string{`2:1 wall-of-text "The build is red. The cause": 4 sentences in one paragraph`}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var got []string
			for _, wall := range wallsIn(tc.text) {
				got = append(got, wall.String())
			}
			if len(got) != len(tc.walls) {
				t.Fatalf("walls = %q, want %q", got, tc.walls)
			}
			for i, want := range tc.walls {
				if !strings.HasPrefix(got[i], want) || strings.Contains(got[i], "question") {
					t.Fatalf("wall %d = %q, want it to open %q and ask for no question", i, got[i], want)
				}
			}
		})
	}
}

func dmCardLinks(n int) []string {
	links := make([]string, n)
	for i := range links {
		links[i] = fmt.Sprintf("<https://acme.slack.com/archives/D0C4HQWPLCC/p17942771252847%02d|card %d>", i, i+1)
	}
	return links
}
