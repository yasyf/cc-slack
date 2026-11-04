package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
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

var permissionCard = regexp.MustCompile(`^slack:([A-Z0-9]+):(\d+\.\d+)?:[0-9a-f]{12}$`)

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
	Other     bool     `json:"other,omitempty"`
	Question  string   `json:"question,omitempty"`
	Options   []string `json:"options,omitempty"`
	Capture   string   `json:"capture,omitempty"`
	Via       string   `json:"via,omitempty"`
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
	Other    bool     `json:"-"`
	Row      bool     `json:"-"`
}

type cardRef struct {
	channel, thread, ts string
}

type waitsAnswer struct {
	card      cardRef
	tap       askTap
	other     bool
	at        time.Time
	actionTS  string
	source    string
	where     string
	via       string
	viaName   string
	dedup     string
	settled   bool
	withdrawn bool
}

var errNoStatusSession = errors.New("no session on " + hostName() + " posted that message")

func hostName() string {
	name, _ := os.Hostname()
	return name
}

type cardAnswerResult struct {
	Answered bool      `json:"answered"`
	Label    string    `json:"label"`
	Other    bool      `json:"other,omitempty"`
	At       time.Time `json:"at"`
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
		k, ok := decodeTap(a.Value, strings.TrimPrefix(a.ActionID, cardAskAction))
		return k, ok && k.Label != ""
	case strings.HasPrefix(a.ActionID, cardWaitAction):
		_, waits, _ := strings.Cut(strings.TrimPrefix(a.ActionID, cardWaitAction), ":")
		k, ok := decodeTap(a.Value, waits)
		k.Waits = true
		return k, ok && k.Label != ""
	case strings.HasPrefix(a.ActionID, cardRowAction):
		_, row, _ := strings.Cut(strings.TrimPrefix(a.ActionID, cardRowAction), ":")
		k, ok := decodeTap(a.Value, row)
		k.Row = true
		return k, ok && k.Label != ""
	case strings.HasPrefix(a.ActionID, cardWroteAction):
		k, ok := decodeTap(a.Value, strings.TrimPrefix(a.ActionID, cardWroteAction))
		k.Label, k.Waits, k.Other = strings.TrimSpace(k.Label), true, true
		return k, ok && k.Label != ""
	case strings.HasPrefix(a.ActionID, cardExplainAction):
		k, ok := decodeTap(a.Value, strings.TrimPrefix(a.ActionID, cardExplainAction))
		k.Waits, k.Explain = true, true
		return k, ok && k.Label == cardExplainLabel
	}
	return askTap{}, false
}

func decodeTap(value, id string) (askTap, bool) {
	var k askTap
	if err := json.Unmarshal([]byte(value), &k); err != nil || k.ID != id {
		return askTap{}, false
	}
	return k, true
}

func (a cardActionPush) ref() cardRef { return cardRef{a.Channel, a.ThreadTS, a.MessageTS} }

func (a cardActionPush) check(id identity) error {
	if a.User != id.UserID || a.Channel != id.DMChannelID {
		return fmt.Errorf("tap by %s in %s is not %s's own DM", a.User, a.Channel, id.UserID)
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

func captureLine(id identity, k askTap, at time.Time, where string) string {
	return fmt.Sprintf("%s answered %q with %q at %s on %s", id.name(), k.Question, k.Label, at.UTC().Format(time.RFC3339), where)
}

func cardSource(id identity, how, posted, permalink string) string {
	return fmt.Sprintf("%s from %s, your operator, on your status card posted %s (%s)", how, id.name(), posted, permalink)
}

func tapLine(who, permalink string, k askTap, capture string) string {
	if k.Explain {
		return fmt.Sprintf("%s: they asked you to explain the waiting ask %q more. Reply with `cc-present reply --block %q --body <answer>` when the card is on a cc-present board, "+
			"otherwise with slack_dm_status text and about %q; either way the reply opens with a link back to <%s|this card>. "+
			"Say the question in plain words and what each answer would do. The lane that asked stays stopped until they tap an answer.",
			who, k.ID, k.ID, k.ID, permalink)
	}
	if k.Waits {
		record := "record the capture in cc-notes"
		if label := slackLabel(k); label != "" {
			record += " with the label " + label
		}
		return fmt.Sprintf("%s: they answered the waiting ask %q with %q. The lane that asked is stopped on this answer: hand it to that lane now, "+
			"and %s: %s", who, k.ID, k.Label, record, capture)
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

func slackLabel(k askTap) string {
	place := permissionCard.FindStringSubmatch(k.ID)
	if place == nil || k.Other {
		return ""
	}
	prefix := map[string]string{"Yes": "slack-grant:", "No": "slack-revoke:"}[k.Label]
	switch {
	case prefix == "":
		return ""
	case place[2] == "":
		return prefix + place[1]
	}
	return prefix + place[1] + "/" + place[2]
}

func dmWatches(ctx context.Context, db *sql.DB, channelID, threadTS string) ([]watch, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, subject_id, owner_agent_id, kind, channel_id, thread_ts FROM watches
		WHERE kind = 'dm' AND status = 'active' AND channel_id = ? AND thread_ts = ? ORDER BY id`, channelID, threadTS)
	if err != nil {
		return nil, fmt.Errorf("find DM message watches %s/%s: %w", channelID, threadTS, err)
	}
	var watches []watch
	for rows.Next() {
		var w watch
		if err := rows.Scan(&w.ID, &w.SubjectID, &w.Owner, &w.Kind, &w.ChannelID, &w.ThreadTS); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan DM message watch: %w", err)
		}
		watches = append(watches, w)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close DM message watches: %w", err)
	}
	return watches, rows.Err()
}

func tapHow(k askTap) string {
	if k.Other {
		return "Slack answer written in"
	}
	return "Slack tap"
}

func (p *poller) tapWaits(ctx context.Context, id identity, a cardActionPush, k askTap, at time.Time, who, permalink string) error {
	if pr, ok := p.cards.prCard(a.Channel, a.ThreadTS, k.ID); ok && !k.Other && len(id.Config.PRLinks.CardCommand) > 0 {
		return p.tapPRCard(ctx, id, a, k, pr, at)
	}
	if !p.cards.asks(a.Channel, a.ThreadTS, k.ID, k.Question) {
		p.log.Printf("card action %s on %s: dropped: the card was redrawn with another question after this answer was drawn", a.ActionID, a.MessageTS)
		return nil
	}
	_, err := p.answerWaits(ctx, id, waitsAnswer{
		card: a.ref(), tap: k, other: k.Other, at: at, actionTS: a.ActionTS, source: who, where: "Slack card " + permalink,
		dedup: fmt.Sprintf("slack-action:%s:%s:%s", a.Channel, a.MessageTS, a.ActionTS),
	})
	if errors.Is(err, errNoStatusSession) {
		p.log.Printf("card action %s on %s/%s: dropped: %v", a.ActionID, a.Channel, a.ThreadTS, err)
		return nil
	}
	return err
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
	tappedAt := slackTime(a.ActionTS)
	permalink := id.permalink(a.Channel, a.MessageTS, a.ThreadTS)
	who := cardSource(id, tapHow(k), slackTime(a.MessageTS).In(pacific).Format(cardTimeLayout), permalink)
	if k.Waits && !k.Explain {
		return p.tapWaits(ctx, id, a, k, tappedAt, who, permalink)
	}
	if k.Row {
		return p.tapRow(ctx, id, a, k, tappedAt, permalink)
	}
	db := p.plane.DB()
	watches, err := dmWatches(ctx, db, a.Channel, a.ThreadTS)
	if err != nil {
		return err
	}
	if len(watches) == 0 {
		p.log.Printf("card action %s on %s/%s: dropped: %v", a.ActionID, a.Channel, a.ThreadTS, errNoStatusSession)
		return nil
	}
	line := tapLine(who, permalink, k, "")
	for _, w := range watches {
		msg := actionPayload{
			WatchID: w.ID, ChannelID: a.Channel, ThreadTS: a.ThreadTS, TS: a.MessageTS, ActionTS: a.ActionTS, Label: k.Label, ID: k.ID,
			Waits: k.Waits, Question: k.Question, Options: k.Options, Permalink: permalink,
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
		err = p.markHint(ctx, id, a.ref(), "🔎 *You asked for more at "+clock(tappedAt, p.now())+".* An explanation follows in this thread. "+
			"The lane that asked is still stopped until you tap an answer.")
	case k.ID != "":
		err = p.markAsk(ctx, id, a, k)
	default:
		err = p.markOption(ctx, id, a.ref(), "✓ *"+k.Label+"* · "+p.now().In(pacific).Format(cardTimeLayout))
	}
	if err != nil {
		p.log.Printf("card action %s on %s: delivered, but the card still shows its buttons: %v", a.ActionID, a.MessageTS, err)
	}
	return nil
}

func (p *poller) numberedTap(id identity, m slack.Message) (cardActionPush, bool) {
	if !id.fromOperator(m) || m.SubType != "" || m.ThreadTimestamp == "" || m.ThreadTimestamp == m.Timestamp {
		return cardActionPush{}, false
	}
	ts, pick, ok := p.cards.numberedPick(id.DMChannelID, m.ThreadTimestamp, strings.TrimSpace(m.Text))
	return cardActionPush{User: m.User, Channel: id.DMChannelID, MessageTS: ts, ThreadTS: m.ThreadTimestamp,
		ActionID: pick.actionID, Value: pick.value, ActionTS: m.Timestamp}, ok
}

func (p *poller) tapPRCard(ctx context.Context, id identity, a cardActionPush, k askTap, pr refID, at time.Time) error {
	won, prior := p.cards.claim(a.Channel, a.ThreadTS, k.ID, askAnswer{At: at, Label: k.Label})
	if !won {
		p.log.Printf("PR card %s: %q dropped: already answered with %q", k.ID, k.Label, prior.Label)
		return nil
	}
	answer := map[string]any{"repo": pr.Owner + "/" + pr.Repo, "number": pr.Number, "waits": k.ID, "label": k.Label}
	if _, err := runCardCommand(ctx, id.Config.PRLinks.CardCommand, map[string]any{"answer": answer}); err != nil {
		p.cards.release(a.Channel, a.ThreadTS, k.ID)
		p.log.Printf("PR card %s: %q not applied, so the card keeps its buttons: %v", k.ID, k.Label, err)
		return nil
	}
	if err := p.markOption(ctx, id, a.ref(), "✓ *"+mrkdwnEscapes.Replace(k.Label)+"* · answered "+clock(at, p.now())); err != nil {
		p.log.Printf("PR card %s: %q applied, but the card still shows its buttons: %v", k.ID, k.Label, err)
	}
	return nil
}

func (p *poller) tapRow(ctx context.Context, id identity, a cardActionPush, k askTap, at time.Time, permalink string) error {
	db := p.plane.DB()
	watches, err := dmWatches(ctx, db, a.Channel, a.ThreadTS)
	if err != nil {
		return err
	}
	if err := p.decideOnBoard(ctx, db, watches, boardDecision{Row: k.ID, Label: k.Label, At: at, Permalink: permalink}); err != nil {
		p.log.Printf("row %s on %s: %q did not reach the board: %v", k.ID, a.MessageTS, k.Label, err)
		return p.confirmTap(ctx, id, a.ref(), cardRowBlock+k.ID,
			"⚠️ *"+mrkdwnEscapes.Replace(k.Label)+"* did not reach the board: "+mrkdwnEscapes.Replace(clipRunes(err.Error(), cardMaxDetailLen)))
	}
	p.cards.claim(a.Channel, a.ThreadTS, k.ID, askAnswer{At: at, Label: k.Label})
	_, err = p.cards.redraw(ctx, p.newAPI(id.Token), a.Channel, a.ThreadTS, a.MessageTS, p.now())
	return err
}

func (p *poller) answerWaits(ctx context.Context, id identity, w waitsAnswer) (cardAnswerResult, error) {
	won, prior := p.cards.claim(w.card.channel, w.card.thread, w.tap.ID, askAnswer{At: w.at, Label: w.tap.Label, Other: w.other})
	if !won {
		p.log.Printf("waiting ask %s: %q dropped: already answered with %q", w.tap.ID, w.tap.Label, prior.Label)
		return cardAnswerResult{Label: prior.Label, Other: prior.Other, At: prior.At}, nil
	}
	if w.withdrawn {
		line := "*Withdrawn:* " + mrkdwnEscapes.Replace(clipRunes(w.tap.Label, cardMaxDetailLen)) + " · " + clock(w.at, p.now())
		if err := p.markOption(ctx, id, w.card, line); err != nil {
			p.cards.release(w.card.channel, w.card.thread, w.tap.ID)
			return cardAnswerResult{}, err
		}
		p.react(ctx, id, w.card, cardWithdrawnReaction)
		return cardAnswerResult{Answered: true, Label: w.tap.Label, Other: true, At: w.at}, nil
	}
	if w.settled {
		line := "✓ " + slackLink(w.via, clipRunes(w.tap.Label, cardMaxDetailLen)) + " · " + clock(w.at, p.now())
		if err := p.markOption(ctx, id, w.card, line); err != nil {
			p.cards.release(w.card.channel, w.card.thread, w.tap.ID)
			return cardAnswerResult{}, err
		}
		p.react(ctx, id, w.card, cardAnsweredReaction)
		p.answerSuperseded(ctx, id, w)
		return cardAnswerResult{Answered: true, Label: w.tap.Label, Other: true, At: w.at}, nil
	}
	db := p.plane.DB()
	watches, err := dmWatches(ctx, db, w.card.channel, w.card.thread)
	if err == nil && len(watches) == 0 {
		err = fmt.Errorf("%w: %s/%s", errNoStatusSession, w.card.channel, w.card.thread)
	}
	if err != nil {
		p.cards.release(w.card.channel, w.card.thread, w.tap.ID)
		return cardAnswerResult{}, err
	}
	permalink := id.permalink(w.card.channel, w.card.ts, w.card.thread)
	capture := captureLine(id, w.tap, w.at, w.where)
	line := tapLine(w.source, permalink, w.tap, capture)
	for _, watch := range watches {
		msg := actionPayload{
			WatchID: watch.ID, ChannelID: w.card.channel, ThreadTS: w.card.thread, TS: w.card.ts, ActionTS: w.actionTS, Label: w.tap.Label, ID: w.tap.ID,
			Waits: true, Other: w.other, Question: w.tap.Question, Options: w.tap.Options, Capture: capture, Via: w.via, Permalink: permalink,
		}
		if err := p.deliver(ctx, db, watch, delivery{
			base:   eventAction,
			origin: event.OriginHuman,
			dedup:  w.dedup,
			line:   line,
			payload: func(typ string) []byte {
				msg.Type = typ
				return mustJSON(msg)
			},
		}); err != nil {
			return cardAnswerResult{}, fmt.Errorf("waiting ask %s keeps its answer %q, but delivering it to watch %d failed: %w", w.tap.ID, w.tap.Label, watch.ID, err)
		}
	}
	answer := "✓ *" + mrkdwnEscapes.Replace(w.tap.Label) + "* · answered "
	if w.other {
		answer = "✓ *Other:* " + mrkdwnEscapes.Replace(clipRunes(w.tap.Label, cardMaxDetailLen)) + " · answered "
	}
	if w.via != "" {
		answer += "on " + slackLink(w.via, w.viaName) + " "
	}
	if err := p.markOption(ctx, id, w.card, answer+clock(w.at, p.now())); err != nil {
		p.log.Printf("waiting ask %s on %s: answered, but the card still shows its buttons: %v", w.tap.ID, w.card.ts, err)
	}
	p.react(ctx, id, w.card, cardAnsweredReaction)
	if w.via == "" {
		p.forwardAnswer(ctx, db, watches, boardAnswer{Waits: w.tap.ID, Label: w.tap.Label, Other: w.other, At: w.at, Permalink: permalink})
	}
	p.answerSuperseded(ctx, id, w)
	return cardAnswerResult{Answered: true, Label: w.tap.Label, Other: w.other, At: w.at}, nil
}

const (
	cardAnsweredReaction  = "white_check_mark"
	cardWithdrawnReaction = "x"
)

func (p *poller) react(ctx context.Context, id identity, r cardRef, name string) {
	if err := p.newAPI(id.Token).AddReactionContext(ctx, name, slack.NewRefToMessage(r.channel, r.ts)); err != nil && errorCode(err) != "already_reacted" {
		p.log.Printf("card %s/%s: edited, but the %s reaction failed: %v", r.channel, r.ts, name, err)
	}
}

func (p *poller) answerSuperseded(ctx context.Context, id identity, w waitsAnswer) {
	card := slackLink(id.permalink(w.card.channel, w.card.ts, w.card.thread), "the card")
	p.noteSuperseded(ctx, id, p.cards.supersedes(w.card.channel, w.card.thread, w.card.ts),
		"✓ *"+mrkdwnEscapes.Replace(w.tap.Question)+"* · answered *"+mrkdwnEscapes.Replace(clipRunes(w.tap.Label, cardMaxDetailLen))+"* "+clock(w.at, p.now())+" on "+card)
}

func (p *poller) noteSuperseded(ctx context.Context, id identity, posts []string, line string) {
	api := p.newAPI(id.Token)
	for _, post := range posts {
		ref, _ := parsePermalink(post)
		if err := updateMessage(ctx, api, ref.channelID, ref.ts, line, nil); err != nil {
			p.log.Printf("superseded post %s not edited: %v", post, err)
		}
	}
}

func (p *poller) answerElsewhere(ctx context.Context, id identity, waits, card, label string, other, settled, withdrawn bool, via string) (cardAnswerResult, error) {
	thread, m, ok := p.cards.waiting(id.DMChannelID, waits, func(thread, ts string) bool { return id.permalink(id.DMChannelID, ts, thread) == card })
	if !ok {
		return cardAnswerResult{}, fmt.Errorf("no waiting card %q at %s in your DM", waits, card)
	}
	answers := m.Card.answers()
	if !other && !withdrawn && !slices.Contains(answers, label) {
		return cardAnswerResult{}, fmt.Errorf("%q is not one of waiting card %q's answers %q; pass other for a write-in", label, waits, answers)
	}
	at := p.now()
	permalink := id.permalink(id.DMChannelID, m.TS, thread)
	kind, surface, name := "Board answer", "the cc-present board", "the board"
	if ref, err := parsePermalink(via); err == nil && ref.ts != "" {
		kind, surface, name = "Answer relayed", "Slack message", "Slack"
	}
	return p.answerWaits(ctx, id, waitsAnswer{
		card:      cardRef{id.DMChannelID, thread, m.TS},
		tap:       askTap{ID: waits, Label: label, Question: m.Card.Title, Options: answers, Waits: true},
		other:     other,
		at:        at,
		source:    fmt.Sprintf("%s from %s, your operator, on %s for your status card posted %s (%s)", kind, id.name(), via, m.At.In(pacific).Format(cardTimeLayout), permalink),
		where:     surface + " " + via + ", which answers Slack card " + permalink,
		via:       via,
		viaName:   name,
		dedup:     fmt.Sprintf("slack-answer:%s:%s:%s", id.DMChannelID, thread, waits),
		settled:   settled,
		withdrawn: withdrawn,
	})
}

func (p *poller) markOption(ctx context.Context, id identity, r cardRef, line string) error {
	if !p.cards.markReply(r.channel, r.thread, r.ts, func(m *liveMessage) { m.Tapped = line }) {
		return p.confirmTap(ctx, id, r, cardActionsBlock, line)
	}
	_, err := p.cards.redraw(ctx, p.newAPI(id.Token), r.channel, r.thread, r.ts, p.now())
	return err
}

func (p *poller) markHint(ctx context.Context, id identity, r cardRef, line string) error {
	if !p.cards.markReply(r.channel, r.thread, r.ts, func(m *liveMessage) { m.Hint = line }) {
		return p.confirmTap(ctx, id, r, cardWaitBlock, line)
	}
	_, err := p.cards.redraw(ctx, p.newAPI(id.Token), r.channel, r.thread, r.ts, p.now())
	return err
}

func (p *poller) markAsk(ctx context.Context, id identity, a cardActionPush, k askTap) error {
	at := slackTime(a.ActionTS)
	if !k.expired(at) {
		p.cards.claim(a.Channel, a.ThreadTS, k.ID, askAnswer{At: at, Label: k.Label})
	}
	redrawn, err := p.cards.redraw(ctx, p.newAPI(id.Token), a.Channel, a.ThreadTS, a.MessageTS, p.now())
	if err != nil || redrawn[a.MessageTS] {
		return err
	}
	line := "✓ *" + mrkdwnEscapes.Replace(k.Label) + "* · answered " + clock(at, p.now())
	if k.expired(at) {
		line = "⌛ *" + mrkdwnEscapes.Replace(k.Label) + "* · expired " + clock(time.Unix(k.Expires, 0), p.now())
	}
	return p.confirmTap(ctx, id, a.ref(), cardAskBlock+k.ID, line)
}

func (p *poller) confirmTap(ctx context.Context, id identity, r cardRef, blockID, line string) error {
	api := p.newAPI(id.Token)
	msgs, _, _, err := api.GetConversationRepliesContext(ctx, &slack.GetConversationRepliesParameters{
		ChannelID: r.channel, Timestamp: r.thread, Oldest: r.ts, Latest: r.ts, Inclusive: true,
	})
	if err != nil {
		return fmt.Errorf("read the tapped card: %w", err)
	}
	for _, m := range msgs {
		if m.Timestamp != r.ts {
			continue
		}
		blocks := tappedBlocks(m.Blocks.BlockSet, blockID, line)
		if blocks == nil {
			return nil
		}
		return updateMessage(ctx, api, r.channel, r.ts, m.Text, blocks)
	}
	return fmt.Errorf("card %s is gone from its thread", r.ts)
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
