package main

import (
	"context"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"time"
)

const laneHeardLimit = 30 * time.Second

type lanesConfig struct {
	HeardCommand []string `toml:"heard_command" json:"heard_command,omitempty"`
}

func (p *poller) laneHeard(ctx context.Context, id identity, channelID, threadTS, ts, words string) {
	command := id.Config.Lanes.HeardCommand
	if len(command) == 0 {
		return
	}
	args := append(slices.Clone(command[1:]), "--channel", channelID, "--ts", ts, "--words", words, "--link", id.permalink(channelID, ts, threadTS))
	if threadTS != "" && threadTS != ts {
		args = append(args, "--thread", threadTS)
	}
	go func(ctx context.Context) {
		ctx, cancel := context.WithTimeout(ctx, laneHeardLimit)
		defer cancel()
		if out, err := exec.CommandContext(ctx, command[0], args...).CombinedOutput(); err != nil { //nolint:gosec // G204: argv to the heard_command login resolved, never a shell
			p.log.Printf("lane heard %s/%s: %v: %s", channelID, ts, err, strings.TrimSpace(string(out)))
		}
	}(context.WithoutCancel(ctx))
}

func tapWords(k askTap) string {
	if k.Question == "" {
		return fmt.Sprintf("tapped %q", k.Label)
	}
	return fmt.Sprintf("tapped %q on %q", k.Label, k.Question)
}
