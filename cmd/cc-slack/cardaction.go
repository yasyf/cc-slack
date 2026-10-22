package main

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/slack-go/slack"

	"github.com/yasyf/cc-interact/event"
)

const (
	cardActionEventType = "cc_slack_action"
	eventAction         = "slack.action"
)

type cardActionPush struct {
	User      string `json:"user"`
	Channel   string `json:"channel"`
	MessageTS string `json:"message_ts"`
	ThreadTS  string `json:"thread_ts"`
	ActionID  string `json:"action_id"`
	Value     string `json:"value"`
	ActionTS  string `json:"action_ts"`
	Seq       string `json:"seq"`
}

type actionPayload struct {
	Type      string `json:"type"`
	WatchID   int64  `json:"watch_id"`
	ChannelID string `json:"channel_id"`
	ThreadTS  string `json:"thread_ts"`
	TS        string `json:"ts"`
	ActionTS  string `json:"action_ts"`
	Label     string `json:"label"`
	Permalink string `json:"permalink"`
}

func (a cardActionPush) label() (string, bool) {
	switch {
	case a.ActionID == cardAckAction:
		return cardAckLabel, true
	case a.ActionID == cardPauseAction:
		return cardPauseLabel, true
	case strings.HasPrefix(a.ActionID, cardOptionAction) && a.Value != "":
		return a.Value, true
	}
	return "", false
}

func (a cardActionPush) check(id identity) error {
	if a.User != id.UserID || a.Channel != id.DMChannelID {
		return fmt.Errorf("tap by %s in %s is not %s's own status thread", a.User, a.Channel, id.UserID)
	}
	for _, ts := range []string{a.MessageTS, a.ThreadTS, a.ActionTS} {
		if !slackTSPattern.MatchString(ts) {
			return fmt.Errorf("tap ts %q is not a Slack ts", ts)
		}
	}
	return nil
}

func slackTime(ts string) time.Time {
	secs, _, _ := strings.Cut(ts, ".")
	n, err := strconv.ParseInt(secs, 10, 64)
	if err != nil {
		panic(fmt.Sprintf("slack ts %q passed validation but has no seconds: %v", ts, err))
	}
	return time.Unix(n, 0)
}

func tapLine(id identity, posted, permalink, label string) string {
	who := fmt.Sprintf("Slack tap from %s, your operator, on your status card posted %s (%s)", id.name(), posted, permalink)
	switch label {
	case cardAckLabel:
		return who + ": Ack. They have seen the card; no action is needed."
	case cardPauseLabel:
		return who + ": Pause. Start no new lanes, applies, deploys, or PRs; let in-flight work settle, then post a needs_you status card " +
			"naming what is paused and what resuming would start."
	}
	return fmt.Sprintf("%s: they chose %q. Act on it as operator steering; nothing waits on this tap, and a later card may have superseded it.",
		who, label)
}

func dmWatches(ctx context.Context, db *sql.DB, channelID, threadTS string) ([]watch, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, subject_id, owner_agent_id, kind, channel_id, thread_ts FROM watches
		WHERE kind = 'dm' AND status = 'active' AND channel_id = ? AND thread_ts = ? ORDER BY id`, channelID, threadTS)
	if err != nil {
		return nil, fmt.Errorf("find status thread %s/%s: %w", channelID, threadTS, err)
	}
	var watches []watch
	for rows.Next() {
		var w watch
		if err := rows.Scan(&w.ID, &w.SubjectID, &w.Owner, &w.Kind, &w.ChannelID, &w.ThreadTS); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan status thread watch: %w", err)
		}
		watches = append(watches, w)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close status thread watches: %w", err)
	}
	return watches, rows.Err()
}

func (p *poller) cardAction(ctx context.Context, id identity, a cardActionPush) error {
	if err := a.check(id); err != nil {
		p.log.Printf("card action %s: dropped: %v", a.ActionID, err)
		return nil
	}
	label, ok := a.label()
	if !ok {
		p.log.Printf("card action %q: dropped: not a status-card button", a.ActionID)
		return nil
	}
	db := p.plane.DB()
	watches, err := dmWatches(ctx, db, a.Channel, a.ThreadTS)
	if err != nil {
		return err
	}
	if len(watches) == 0 {
		p.log.Printf("card action %s on %s/%s: dropped: no session holds that status thread", a.ActionID, a.Channel, a.ThreadTS)
		return nil
	}
	permalink := id.permalink(a.Channel, a.MessageTS, a.ThreadTS)
	line := tapLine(id, slackTime(a.MessageTS).In(pacific).Format(cardTimeLayout), permalink, label)
	for _, w := range watches {
		msg := actionPayload{
			WatchID: w.ID, ChannelID: a.Channel, ThreadTS: a.ThreadTS, TS: a.MessageTS, ActionTS: a.ActionTS, Label: label, Permalink: permalink,
		}
		if err := p.deliver(ctx, db, w, delivery{
			base:   eventAction,
			origin: event.OriginHuman,
			dedup:  fmt.Sprintf("slack-action:%s:%s:%s", a.Channel, a.MessageTS, a.ActionTS),
			line:   line,
			payload: func(typ string) []byte {
				msg.Type = typ
				return mustJSON(msg)
			},
		}); err != nil {
			return err
		}
	}
	if err := p.confirmTap(ctx, id, a, label); err != nil {
		p.log.Printf("card action %s on %s: delivered, but the card still shows its buttons: %v", a.ActionID, a.MessageTS, err)
	}
	return nil
}

func (p *poller) confirmTap(ctx context.Context, id identity, a cardActionPush, label string) error {
	api := p.newAPI(id.Token)
	msgs, _, _, err := api.GetConversationRepliesContext(ctx, &slack.GetConversationRepliesParameters{
		ChannelID: a.Channel, Timestamp: a.ThreadTS, Oldest: a.MessageTS, Latest: a.MessageTS, Inclusive: true,
	})
	if err != nil {
		return fmt.Errorf("read the tapped card: %w", err)
	}
	for _, m := range msgs {
		if m.Timestamp != a.MessageTS {
			continue
		}
		blocks := tappedBlocks(m.Blocks.BlockSet, label, p.now())
		if blocks == nil {
			return nil
		}
		return updateMessage(ctx, api, a.Channel, a.MessageTS, m.Text, blocks)
	}
	return fmt.Errorf("card %s is gone from its thread", a.MessageTS)
}

func tappedBlocks(blocks []slack.Block, label string, at time.Time) []slack.Block {
	out := make([]slack.Block, 0, len(blocks))
	tapped := false
	for _, b := range blocks {
		if b.ID() != cardActionsBlock {
			out = append(out, b)
			continue
		}
		tapped = true
		out = append(out, slack.NewContextBlock(cardActionsBlock+"-tapped",
			slack.NewTextBlockObject(slack.MarkdownType, "✓ *"+label+"* · "+at.In(pacific).Format(cardTimeLayout), false, false)))
	}
	if !tapped {
		return nil
	}
	return out
}
