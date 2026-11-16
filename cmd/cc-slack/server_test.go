package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestTheCardCommandFindsClaudeInLocalBinUnderTheLaunchAgentPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", "/usr/bin:/bin:/usr/sbin:/sbin")
	bin := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\necho '{\"cards\":[],\"prs\":[]}'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	card := filepath.Join(t.TempDir(), "card")
	if err := os.WriteFile(card, []byte("#!/bin/sh\nexec claude\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	ask := map[string]any{"numbers": []int{1}}
	if _, err := runCardCommand(context.Background(), []string{card}, ask); err == nil {
		t.Fatal("card command found claude before reachUserBin, want it missing from the LaunchAgent's PATH")
	}
	for range 2 {
		if err := reachUserBin(); err != nil {
			t.Fatal(err)
		}
	}
	out, err := runCardCommand(context.Background(), []string{card}, ask)
	if err != nil || string(out) != "{\"cards\":[],\"prs\":[]}\n" {
		t.Fatalf("card command = %q, %v; want claude's output from ~/.local/bin", out, err)
	}
	if n := len(slices.DeleteFunc(filepath.SplitList(os.Getenv("PATH")), func(dir string) bool { return dir != bin })); n != 1 {
		t.Fatalf("PATH holds %s %d times after two calls, want once, as the upgrade's re-exec calls it again", bin, n)
	}
}
