package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func fakeCCI(t *testing.T, script string) string {
	t.Helper()
	bin := t.TempDir()
	claims := filepath.Join(bin, "claims")
	body := "#!/bin/sh\necho \"$CLAUDE_CODE_SESSION_ID $*\" >> '" + claims + "'\n" + script
	if err := os.WriteFile(filepath.Join(bin, cciBinary), []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return claims
}

func readClaims(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(raw)), "\n")
}

func TestPostsClaimTheirThreadForTheSession(t *testing.T) {
	claims := fakeCCI(t, "")
	l := startDaemon(t)
	loggedIn(t, l)

	var sent postResult
	if err := json.Unmarshal(l.post(opSend, map[string]string{"channel_id": testChannel, "text": "PR is up"}).Body, &sent); err != nil {
		t.Fatal(err)
	}
	if sent.ThreadTS != sent.TS {
		t.Fatalf("send thread_ts = %q, want its own ts %q", sent.ThreadTS, sent.TS)
	}
	var replied postResult
	if err := json.Unmarshal(l.post(opReply, map[string]string{"url": sent.Permalink, "text": "CI is green"}).Body, &replied); err != nil {
		t.Fatal(err)
	}
	if replied.ThreadTS != sent.TS {
		t.Fatalf("reply thread_ts = %q, want the parent's ts %q", replied.ThreadTS, sent.TS)
	}
	l.do(opReact, map[string]string{"channel_id": testChannel, "ts": sent.TS, "name": ":white_check_mark:"})
	var status map[string]string
	if err := json.Unmarshal(l.do(opDMStatus, map[string]string{"text": "starting"}).Body, &status); err != nil {
		t.Fatal(err)
	}

	posted := testSession + " claim --thread " + testChannel + "/" + sent.TS + " --rel posted"
	want := []string{posted, posted, testSession + " claim --thread " + status["channel_id"] + "/" + status["thread_ts"] + " --rel posted"}
	if got := readClaims(t, claims); !slices.Equal(got, want) {
		t.Fatalf("cci calls = %q, want %q", got, want)
	}
}

func TestFailedClaimLeavesThePostSucceeded(t *testing.T) {
	claims := fakeCCI(t, "echo 'no such store' >&2\nexit 1\n")
	l := startDaemon(t)
	loggedIn(t, l)

	var sent postResult
	if err := json.Unmarshal(l.post(opSend, map[string]string{"channel_id": testChannel, "text": "PR is up"}).Body, &sent); err != nil {
		t.Fatal(err)
	}
	if !sent.Posted || len(l.slack.sent()) != 1 {
		t.Fatalf("send = %+v with %d Slack posts, want one posted message", sent, len(l.slack.sent()))
	}
	if got := readClaims(t, claims); len(got) != 1 {
		t.Fatalf("cci calls = %q, want one claim attempt", got)
	}
}
