package main

import (
	"context"
	"database/sql"
	"encoding/json"
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
	Type      string   `json:"type"`
	WatchID   int64    `json:"watch_id"`
	ChannelID string   `json:"channel_id"`
	ThreadTS  string   `json:"thread_ts"`
	TS        string   `json:"ts"`
	ActionTS  string   `json:"action_ts"`
	Label     string   `json:"label"`
	ID        string   `json:"id,omitempty"`
	Waits     bool     `json:"waits,omitempty"`
	Question  string   `json:"question,omitempty"`
	Options   []string `json:"options,omitempty"`
	Capture   string   `json:"capture,omitempty"`
	Permalink string   `json:"permalink"`
}

type askTap struct {
	ID       string   `json:"id"`
	Label    string   `json:"label"`
	Expires  int64    `json:"exp,omitempty"`
	Question string   `json:"q,omitempty"`
	Options  []string `json:"o,omitempty"`
	Waits    bool     `json:"-"`
	Explain  bool     `json:"-"`
}

func (k askTap) expired(now time.Time) bool {
	return k.Expires != 0 && !now.Before(time.Unix(k.Expires, 0))
}

func (a cardActionPush) tap() (askTap, bool) {
	switch {
	case a.ActionID == cardAckAction:
		return askTap{Label: cardAckLabel}, true
	case a.ActionID == cardPauseAction:
		return askTap{Label: cardPauseLabel}, true
	case strings.HasPrefix(a.ActionID, cardOptionAction) && a.Value != "":
		return askTap{Label: a.Value}, true
	case strings.HasPrefix(a.ActionID, cardAskAction):
		var k askTap
		if err := json.Unmarshal([]byte(a.Value), &k); err != nil || k.ID != strings.TrimPrefix(a.ActionID, cardAskAction) || k.Label == "" {
			return askTap{}, false
		}
		return k, true
	case strings.HasPrefix(a.ActionID, cardWaitAction):
		_, waits, _ := strings.Cut(strings.TrimPrefix(a.ActionID, cardWaitAction), ":")
		var k askTap
		if err := json.Unmarshal([]byte(a.Value), &k); err != nil || k.ID != waits || k.Label == "" {
			return askTap{}, false
		}
		k.Waits = true
		return k, true
	case strings.HasPrefix(a.ActionID, cardExplainAction):
		var k askTap
		if err := json.Unmarshal([]byte(a.Value), &k); err != nil || k.ID != strings.TrimPrefix(a.ActionID, cardExplainAction) || k.Label != cardExplainLabel {
			return askTap{}, false
		}
		k.Waits, k.Explain = true, true
		return k, true
	}
	return askTap{}, false
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

func captureLine(id identity, k askTap, at time.Time, permalink string) string {
	return fmt.Sprintf("%s answered %q with %q at %s on Slack card %s", id.name(), k.Question, k.Label, at.UTC().Format(time.RFC3339), permalink)
}

func tapLine(id identity, posted, permalink string, k askTap, capture string) string {
	who := fmt.Sprintf("Slack tap from %s, your operator, on your status card posted %s (%s)", id.name(), posted, permalink)
	if k.Explain {
		return fmt.Sprintf("%s: they asked you to explain the waiting ask %q more. Reply in that card's thread with the question in plain words "+
			"and what each answer would do. The lane that asked stays stopped until they tap an answer.", who, k.ID)
	}
	if k.Waits {
		return fmt.Sprintf("%s: they answered the waiting ask %q with %q. The lane that asked is stopped on this answer: hand it to that lane now, "+
			"and record the capture in cc-notes: %s", who, k.ID, k.Label, capture)
	}
	if k.ID != "" {
		return fmt.Sprintf("%s: they answered your ask %q with %q. Act on it as operator steering; a later card may have superseded it.",
			who, k.ID, k.Label)
	}
	switch label := k.Label; label {
	case cardAckLabel:
		return who + ": Ack. They have seen the card; no action is needed."
	case cardPauseLabel:
		return who + ": Pause. Start no new lanes, applies, deploys, or PRs; let in-flight work settle, then post a needs_you status card " +
			"naming what is paused and what resuming would start."
	}
	return fmt.Sprintf("%s: they chose %q. Act on it as operator steering; nothing waits on this tap, and a later card may have superseded it.",
		who, k.Label)
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
	k, ok := a.tap()
	if !ok {
		p.log.Printf("card action %q: dropped: not a status-card button", a.ActionID)
		return nil
	}
	if k.expired(slackTime(a.ActionTS)) {
		p.log.Printf("card action %s on %s: dropped: the ask expired", a.ActionID, a.MessageTS)
		if err := p.markAsk(ctx, id, a, k); err != nil {
			p.log.Printf("card action %s on %s: the card still offers the expired ask: %v", a.ActionID, a.MessageTS, err)
		}
		return nil
	}
	if k.ID != "" && p.cards.answered(a.Channel, a.ThreadTS, k.ID) {
		p.log.Printf("card action %s on %s: dropped: the ask was already answered", a.ActionID, a.MessageTS)
		if k.Waits {
			return nil
		}
		if err := p.markAsk(ctx, id, a, k); err != nil {
			p.log.Printf("card action %s on %s: the card still offers the answered ask: %v", a.ActionID, a.MessageTS, err)
		}
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
	tappedAt := slackTime(a.ActionTS)
	var capture string
	if k.Waits && !k.Explain {
		capture = captureLine(id, k, tappedAt, permalink)
	}
	line := tapLine(id, slackTime(a.MessageTS).In(pacific).Format(cardTimeLayout), permalink, k, capture)
	for _, w := range watches {
		msg := actionPayload{
			WatchID: w.ID, ChannelID: a.Channel, ThreadTS: a.ThreadTS, TS: a.MessageTS, ActionTS: a.ActionTS, Label: k.Label, ID: k.ID,
			Waits: k.Waits, Question: k.Question, Options: k.Options, Capture: capture, Permalink: permalink,
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
	switch {
	case k.Explain:
		err = p.markHint(ctx, id, a, "🔎 *You asked for more at "+clock(tappedAt, p.now())+".* An explanation follows in this thread. "+
			"The lane that asked is still stopped until you tap an answer.")
	case k.Waits:
		p.cards.answer(a.Channel, a.ThreadTS, k.ID, tappedAt)
		err = p.markOption(ctx, id, a, "✓ *"+mrkdwnEscapes.Replace(k.Label)+"* · answered "+clock(tappedAt, p.now()))
	case k.ID != "":
		err = p.markAsk(ctx, id, a, k)
	default:
		err = p.markOption(ctx, id, a, "✓ *"+k.Label+"* · "+p.now().In(pacific).Format(cardTimeLayout))
	}
	if err != nil {
		p.log.Printf("card action %s on %s: delivered, but the card still shows its buttons: %v", a.ActionID, a.MessageTS, err)
	}
	return nil
}

func (p *poller) markOption(ctx context.Context, id identity, a cardActionPush, line string) error {
	if !p.cards.markReply(a.Channel, a.ThreadTS, a.MessageTS, func(m *liveMessage) { m.Tapped = line }) {
		return p.confirmTap(ctx, id, a, cardActionsBlock, line)
	}
	_, err := p.cards.redraw(ctx, p.newAPI(id.Token), a.Channel, a.ThreadTS, a.MessageTS, p.now())
	return err
}

func (p *poller) markHint(ctx context.Context, id identity, a cardActionPush, line string) error {
	if !p.cards.markReply(a.Channel, a.ThreadTS, a.MessageTS, func(m *liveMessage) { m.Hint = line }) {
		return p.confirmTap(ctx, id, a, cardWaitBlock, line)
	}
	_, err := p.cards.redraw(ctx, p.newAPI(id.Token), a.Channel, a.ThreadTS, a.MessageTS, p.now())
	return err
}

func (p *poller) markAsk(ctx context.Context, id identity, a cardActionPush, k askTap) error {
	at := slackTime(a.ActionTS)
	if !k.expired(at) {
		p.cards.answer(a.Channel, a.ThreadTS, k.ID, at)
	}
	redrawn, err := p.cards.redraw(ctx, p.newAPI(id.Token), a.Channel, a.ThreadTS, a.MessageTS, p.now())
	if err != nil || redrawn[a.MessageTS] {
		return err
	}
	line := "✓ *" + mrkdwnEscapes.Replace(k.Label) + "* · answered " + clock(at, p.now())
	if k.expired(at) {
		line = "⌛ *" + mrkdwnEscapes.Replace(k.Label) + "* · expired " + clock(time.Unix(k.Expires, 0), p.now())
	}
	return p.confirmTap(ctx, id, a, cardAskBlock+k.ID, line)
}

func (p *poller) confirmTap(ctx context.Context, id identity, a cardActionPush, blockID, line string) error {
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
		blocks := tappedBlocks(m.Blocks.BlockSet, blockID, line)
		if blocks == nil {
			return nil
		}
		return updateMessage(ctx, api, a.Channel, a.MessageTS, m.Text, blocks)
	}
	return fmt.Errorf("card %s is gone from its thread", a.MessageTS)
}

func tappedBlocks(blocks []slack.Block, blockID, line string) []slack.Block {
	out := make([]slack.Block, 0, len(blocks))
	tapped := false
	for _, b := range blocks {
		if blockID != cardWaitBlock && strings.HasSuffix(b.ID(), cardWaitBlock) {
			continue
		}
		if b.ID() != blockID {
			out = append(out, b)
			continue
		}
		tapped = true
		out = append(out, mrkdwn(cardTappedBlock+blockID, line))
	}
	if !tapped {
		return nil
	}
	return out
}
