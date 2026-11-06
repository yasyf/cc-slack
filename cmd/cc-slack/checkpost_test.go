package main

import (
	"bytes"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

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
		"bad":   {"Hi Anubhav, I rolled restate to 06ca99cdcd.", 1, []string{"plain-name 'Hi Anubhav'", "commit-sha '06ca99cdcd'"}},
		"clean": {"The release is live, and every chat opens again.", 0, nil},
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
