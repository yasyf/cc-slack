package main

import (
	"cmp"
	"context"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/slack-go/slack"
)

const laneHeardLimit = 30 * time.Second

const laneHeardReaction = "eyes"

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
		out, err := exec.CommandContext(ctx, command[0], args...).CombinedOutput() //nolint:gosec // G204: argv to the heard_command login resolved, never a shell
		if err != nil {
			p.log.Printf("lane heard %s/%s: %v: %s", channelID, ts, err, strings.TrimSpace(string(out)))
			return
		}
		if strings.HasPrefix(words, "tapped ") || !strings.Contains(string(out), " new") {
			return
		}
		if err := p.newAPI(id.Token).AddReactionContext(ctx, laneHeardReaction, slack.NewRefToMessage(channelID, ts)); err != nil && errorCode(err) != "already_reacted" {
			p.log.Printf("lane heard %s/%s: no %s: %v", channelID, ts, laneHeardReaction, err)
		}
	}(context.WithoutCancel(ctx))
}

func tapWords(k askTap) string {
	if k.Question == "" {
		return fmt.Sprintf("tapped %q", k.Label)
	}
	return fmt.Sprintf("tapped %q on %q", k.Label, k.Question)
}

// voiceAnswer takes the operator's tap on a lane question the voice sender posted: it reaches the lane as his words, and the card loses its buttons for a line naming his pick, so a second tap has nothing to press.
func (p *poller) voiceAnswer(ctx context.Context, id identity, a cardActionPush) error {
	if a.User != id.UserID || !slices.Contains(id.operatorChannelIDs(), a.Channel) || !slackTSPattern.MatchString(a.MessageTS) {
		p.log.Printf("voice option %s: dropped: tap by %s in %s is not the operator's in one of his channels", a.ActionID, a.User, a.Channel)
		return nil
	}
	thread := cmp.Or(a.ThreadTS, a.MessageTS)
	api := p.newAPI(id.Token)
	m, err := messageAt(ctx, api, a.Channel, thread, a.MessageTS)
	if err != nil {
		return fmt.Errorf("voice option %s: read the question: %w", a.ActionID, err)
	}
	if !slices.ContainsFunc(m.Blocks.BlockSet, func(b slack.Block) bool { return b.BlockType() == slack.MBTAction }) {
		p.log.Printf("voice option %s on %s: dropped: already answered", a.ActionID, a.MessageTS)
		return nil
	}
	question, _, _ := strings.Cut(strings.TrimSpace(m.Text), "\n")
	words := tapWords(askTap{Label: a.Value, Question: clipRunes(question, 200)})
	if card := voiceCardOf(m.Blocks.BlockSet); card != "" {
		words += " (card " + card + ")"
	}
	p.laneHeard(ctx, id, a.Channel, thread, a.MessageTS, words)
	if err := voiceTapped(a.Channel, a.MessageTS, a.Value); err != nil {
		p.log.Printf("voice option %s on %s: tap not logged: %v", a.ActionID, a.MessageTS, err)
	}
	return updateMessage(ctx, api, a.Channel, a.MessageTS, m.Text, voiceAnswered(m.Blocks.BlockSet, a.Value, slackTime(a.ActionTS)))
}
