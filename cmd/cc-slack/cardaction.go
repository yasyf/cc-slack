package main

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/slack-go/slack"

	"github.com/yasyf/cc-interact/event"
)

const (
	cardActionEventType = "cc_slack_action"
	eventAction         = "slack.action"
)

var (
	permissionCard = regexp.MustCompile(`^slack:([A-Z0-9]+):(\d+\.\d+)?:[0-9a-f]{12}$`)
	headerEmoji    = regexp.MustCompile(`^:[a-z0-9_+-]+:\s*`)
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
	Typed     bool   `json:"-"`
}

// cardPress says how his answer reached a card: the Slack action or message it came from, and whether he pressed the button, typed its number in the thread, wrote in the card's thread, or answered another card it copies.
type cardPress struct {
	ActionTS string
	Source   string
}

func (a cardActionPush) press() cardPress {
	if a.Typed {
		return cardPress{a.ActionTS, "typed its number in the thread"}
	}
	return cardPress{a.ActionTS, "pressed the button"}
}

type actionPayload struct {
	Type        string   `json:"type"`
	WatchID     int64    `json:"watch_id"`
	ChannelID   string   `json:"channel_id"`
	ThreadTS    string   `json:"thread_ts"`
	TS          string   `json:"ts"`
	ActionTS    string   `json:"action_ts"`
	Label       string   `json:"label"`
	ID          string   `json:"id,omitempty"`
	Waits       bool     `json:"waits,omitempty"`
	Other       bool     `json:"other,omitempty"`
	Question    string   `json:"question,omitempty"`
	Options     []string `json:"options,omitempty"`
	Via         string   `json:"via,omitempty"`
	Capture     string   `json:"capture,omitempty"`
	NotesLabel  string   `json:"notes_label,omitempty"`
	PosterEnded bool     `json:"poster_ended,omitempty"`
	Undo        bool     `json:"undo,omitempty"`
	Recorded    bool     `json:"recorded,omitempty"`
	Permalink   string   `json:"permalink"`
}

type askTap struct {
	Fold     bool     `json:"-"`
	ID       string   `json:"id"`
	Label    string   `json:"label"`
	Expires  int64    `json:"exp,omitempty"`
	Question string   `json:"q,omitempty"`
	Options  []string `json:"o,omitempty"`
	Waits    bool     `json:"-"`
	Explain  bool     `json:"-"`
	Other    bool     `json:"-"`
	Row      bool     `json:"-"`
	Undo     bool     `json:"-"`
}

type tapQueue struct {
	mu     sync.Mutex
	queued map[string][]func()
	busy   sync.WaitGroup
}

func (q *tapQueue) run(card string, tap func()) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if taps, ok := q.queued[card]; ok {
		q.queued[card] = append(taps, tap)
		return
	}
	q.queued[card] = []func(){tap}
	q.busy.Go(func() { q.drain(card) })
}

func (q *tapQueue) drain(card string) {
	for {
		q.mu.Lock()
		taps := q.queued[card]
		if len(taps) == 0 {
			delete(q.queued, card)
			q.mu.Unlock()
			return
		}
		q.queued[card] = taps[1:]
		q.mu.Unlock()
		taps[0]()
	}
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
	undo      string
	press     cardPress
	records   bool
	settled   bool
	withdrawn bool
}

var (
	errNoStatusSession = errors.New("no session on " + hostName() + " posted that message")
	errCardMoved       = errors.New("the card was redrawn with another question after this answer was drawn")
	errUndoClosed      = errors.New("the ten minutes to undo this answer had passed")
	errNoStoredCard    = errors.New("no stored card")
	errTapNotRecorded  = errors.New("the card command did not record it, so the card offers its answers again")
)

func hostName() string {
	name, _ := os.Hostname()
	return name
}

type cardAnswerResult struct {
	Answered  bool      `json:"answered"`
	Already   bool      `json:"already_answered,omitempty"`
	Label     string    `json:"label"`
	Other     bool      `json:"other,omitempty"`
	At        time.Time `json:"at"`
	UndoUntil time.Time `json:"undo_until,omitzero"`
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
	case a.ActionID == cardFoldAction:
		return askTap{Label: cardExplainLabel, Fold: true}, true
	case strings.HasPrefix(a.ActionID, cardUndoAction):
		return askTap{ID: strings.TrimPrefix(a.ActionID, cardUndoAction), Label: cardUndoLabel, Undo: true}, true
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

func tapLine(who string, k askTap, capture string, recorded bool) string {
	if k.Explain {
		return fmt.Sprintf("%s: they asked you to explain the waiting ask %q more. Reply with `cc-present reply --block %q --body <answer>` when the card is on a cc-present board, "+
			"otherwise with slack_dm_status text and about %q; either way the reply lands in the card's own thread, so it calls the card this card and never links it, "+
			"since a link there draws the card again under the reply. "+
			"Say the question in plain words and what each answer would do. The lane that asked stays stopped until they tap an answer.",
			who, k.ID, k.ID, k.ID)
	}
	if k.Waits && recorded {
		return fmt.Sprintf("%s: they answered the waiting ask %q with %q. Your card command already posted this answer as the one record the lane that asked reads: "+
			"post no record or capture of it.", who, k.ID, k.Label)
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

func (p *poller) dmWatches(ctx context.Context, db *sql.DB, channelID, threadTS string) ([]watch, error) {
	bound, err := p.threads.bound(ctx, channelID, []string{threadTS})
	if err != nil {
		return nil, err
	}
	return dmSubscriptions(ctx, db, channelID, bound[threadTS])
}

func (p *poller) cardListeners(ctx context.Context, db *sql.DB, channelID, threadTS string) ([]watch, error) {
	watches, err := p.dmWatches(ctx, db, channelID, threadTS)
	if err != nil || len(watches) > 0 {
		return watches, err
	}
	w, held, err := dmHolderWatch(ctx, db, channelID)
	if err != nil || !held {
		return nil, err
	}
	return []watch{w}, nil
}

func (p *poller) threadPosters(ctx context.Context, channelID, threadTS string) ([]watch, error) {
	bound, err := p.threads.bound(ctx, channelID, []string{threadTS})
	if err != nil {
		return nil, err
	}
	posters := make([]watch, 0, len(bound[threadTS]))
	for _, subjectID := range bound[threadTS] {
		posters = append(posters, watch{SubjectID: subjectID, Kind: kindDM, ChannelID: channelID, ThreadTS: threadTS})
	}
	return posters, nil
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
	if err := p.cards.stillAsks(a.Channel, a.ThreadTS, k.ID, k.Question); err != nil {
		return err
	}
	undo := ""
	if p.undoable(id, a.ref(), k) {
		undo = tapKey(a)
	}
	res, err := p.answerWaits(ctx, id, waitsAnswer{
		card: a.ref(), tap: k, other: k.Other, at: at, actionTS: a.ActionTS, source: who, where: "Slack card " + permalink,
		dedup: tapKey(a), undo: undo, press: a.press(), records: true,
	})
	if err == nil && res.Answered && !k.Other {
		if !p.answerCopies(ctx, id, a.ref(), k.Label, false, at, undo) {
			p.recordPRTap(ctx, id, a.ref(), k.ID, k.Label, a.press())
		}
	}
	return err
}

func (p *poller) recordPRTap(ctx context.Context, id identity, card cardRef, waits, label string, press cardPress) {
	pr, ok := p.cards.waitingPR(card.channel, card.thread, waits)
	if !ok || len(id.Config.PRLinks.CardCommand) == 0 {
		return
	}
	if _, err := runCardCommand(ctx, id.Config.PRLinks.CardCommand, map[string]any{"answer": newPRAnswer(pr, waits, label, press, p.cards.heads(card))}); err != nil {
		p.log.Printf("waiting card %s: %q stays with the sessions alone, since the card command did not take it: %v", waits, label, err)
	}
}

type prAnswer struct {
	ActionTS string            `json:"action_ts"`
	Heads    map[string]string `json:"heads"`
	Label    string            `json:"label"`
	Number   int               `json:"number"`
	Repo     string            `json:"repo"`
	Source   string            `json:"source"`
	Waits    string            `json:"waits"`
}

func newPRAnswer(pr refID, waits, label string, press cardPress, heads map[string]string) prAnswer {
	return prAnswer{Repo: pr.Owner + "/" + pr.Repo, Number: pr.Number, Waits: waits, Label: label, ActionTS: press.ActionTS, Source: press.Source, Heads: heads}
}

type owedTap struct {
	Card     string          `json:"card"`
	Row      bool            `json:"row,omitempty"`
	Answer   prAnswer        `json:"answer"`
	Question *questionAnswer `json:"question,omitempty"`
	Line     string          `json:"line,omitempty"`
	Due      time.Time       `json:"due"`
}

type questionAnswer struct {
	ActionTS  string    `json:"action_ts"`
	Label     string    `json:"label"`
	Other     bool      `json:"other"`
	Source    string    `json:"source"`
	UndoUntil time.Time `json:"undo_until,omitzero"`
	Undone    bool      `json:"undone"`
	URL       string    `json:"url"`
	Waits     string    `json:"waits"`
}

type tapRecord int

const (
	tapUnrecorded tapRecord = iota
	tapRecording
	tapRecorded
	tapRefused
)

func (p *poller) applyPRTap(ctx context.Context, id identity, card cardRef, tap owedTap) bool {
	_, err := runCardCommand(ctx, id.Config.PRLinks.CardCommand, map[string]any{"answer": tap.Answer})
	switch {
	case err == nil:
		p.cards.oweTap(card.channel, card.thread, tap.Answer.Waits, nil)
		return true
	case errors.Is(err, errCardCommandUnfinished):
		tap.Due = p.now().Add(prCardPatience)
		p.cards.oweTap(card.channel, card.thread, tap.Answer.Waits, &tap)
		p.wake()
		p.log.Printf("PR card %s: %q not applied yet, applied again at %s: %v", tap.Answer.Waits, tap.Answer.Label, tap.Due.Format(time.RFC3339), err)
		return true
	}
	p.cards.release(card.channel, card.thread, tap.Answer.Waits)
	p.log.Printf("PR card %s: %q not applied: %v", tap.Answer.Waits, tap.Answer.Label, err)
	if tap.Row || p.cards.markReply(card.channel, card.thread, card.ts, func(m *liveMessage) { m.Tapped = "" }) {
		if _, err := p.cards.redraw(ctx, p.newAPI(id.Token), card.channel, card.thread, card.ts, p.now()); err != nil {
			p.log.Printf("PR card %s: the card still shows %q: %v", tap.Answer.Waits, tap.Answer.Label, err)
		}
	}
	if pr, ok := p.cards.prCard(card.channel, card.thread, tap.Answer.Waits); ok && !tap.Row {
		p.recard(ctx, id, card, pr)
	}
	return false
}

func (p *poller) retryTaps(ctx context.Context, id identity) time.Time {
	due, next := p.cards.owedTaps(p.now(), 2*prCardPatience)
	for _, owed := range due {
		p.taps.run(owed.card.channel+"/"+owed.card.ts, func() {
			if owed.tap.Question == nil {
				p.applyPRTap(context.WithoutCancel(ctx), id, owed.card, owed.tap)
				return
			}
			if p.applyQuestionTap(context.WithoutCancel(ctx), id, owed.card, owed.tap) == tapUnrecorded {
				p.refuseTap(context.WithoutCancel(ctx), id, owed.card, owed.tap.Question.Waits, owed.tap.Question.Label, errors.New("the card command no longer takes it"))
			}
		})
	}
	return next
}

func (p *poller) recordQuestionTap(ctx context.Context, id identity, card cardRef, k askTap, undone bool, press cardPress) (tapRecord, error) {
	if !p.recordsQuestions(id, card, k.ID) {
		return tapUnrecorded, nil
	}
	return p.runQuestionTap(ctx, id, newQuestionAnswer(id, card, k, undone, press))
}

func (p *poller) recordsQuestions(id identity, card cardRef, askID string) bool {
	_, pr := p.cards.waitingPR(card.channel, card.thread, askID)
	return !pr && len(id.Config.PRLinks.CardCommand) > 0
}

func newQuestionAnswer(id identity, card cardRef, k askTap, undone bool, press cardPress) questionAnswer {
	return questionAnswer{
		Waits: k.ID, Label: k.Label, Other: k.Other, Undone: undone, URL: id.permalink(card.channel, card.ts, card.thread),
		ActionTS: press.ActionTS, Source: press.Source,
	}
}

func (p *poller) runQuestionTap(ctx context.Context, id identity, answer questionAnswer) (tapRecord, error) {
	out, err := runCardCommand(ctx, id.Config.PRLinks.CardCommand, map[string]any{"answer": answer})
	if errors.Is(err, errCardCommandUnfinished) {
		return tapRecording, err
	}
	if err != nil {
		return tapRefused, err
	}
	var said struct {
		OK  bool   `json:"ok"`
		Why string `json:"why"`
	}
	if err := json.Unmarshal(out, &said); err != nil {
		return tapRefused, fmt.Errorf("the card command's reply %q: %w", strings.TrimSpace(string(out)), err)
	}
	if !said.OK {
		return tapUnrecorded, errors.New(said.Why)
	}
	return tapRecorded, nil
}

func (p *poller) recordTap(ctx context.Context, id identity, card cardRef, k askTap, press cardPress, at, undoUntil time.Time, answered string) tapRecord {
	if !p.recordsQuestions(id, card, k.ID) {
		return tapUnrecorded
	}
	answer := newQuestionAnswer(id, card, k, false, press)
	answer.UndoUntil = undoUntil
	recording := "⏳ *" + mrkdwnEscapes.Replace(clipRunes(k.Label, cardMaxDetailLen)) + "* · recording since " + clock(at, p.now())
	if err := p.tapDrawn(card, at, p.markAnswer(ctx, id, card, k.ID, recording)); err != nil {
		p.log.Printf("waiting card %s: %q is being recorded, but the card still shows its buttons: %v", k.ID, k.Label, err)
	}
	return p.applyQuestionTap(ctx, id, card, owedTap{Card: card.ts, Question: &answer, Line: answered})
}

func (p *poller) applyQuestionTap(ctx context.Context, id identity, card cardRef, tap owedTap) tapRecord {
	answer := *tap.Question
	rec, err := p.runQuestionTap(ctx, id, answer)
	switch rec {
	case tapRecorded:
		p.cards.oweTap(card.channel, card.thread, answer.Waits, nil)
		if err := p.markAnswer(ctx, id, card, answer.Waits, tap.Line); err != nil {
			p.log.Printf("waiting card %s: %q is recorded, but the card still shows it recording: %v", answer.Waits, answer.Label, err)
		}
		p.react(ctx, id, card, cardAnsweredReaction)
		p.cards.answerDelivered(card.channel, card.thread, answer.Waits)
	case tapRecording:
		tap.Due = p.now().Add(prCardPatience)
		p.cards.oweTap(card.channel, card.thread, answer.Waits, &tap)
		p.wake()
		p.log.Printf("waiting card %s: %q not recorded yet, recorded again at %s: %v", answer.Waits, answer.Label, tap.Due.Format(time.RFC3339), err)
	case tapRefused:
		p.refuseTap(ctx, id, card, answer.Waits, answer.Label, err)
	case tapUnrecorded:
		p.log.Printf("waiting card %s: %q stays with the sessions alone, since the card command does not record it: %v", answer.Waits, answer.Label, err)
	}
	return rec
}

func (p *poller) redrawOffer(ctx context.Context, id identity, card cardRef, askID string) {
	if !p.recordsQuestions(id, card, askID) || !p.cards.markReply(card.channel, card.thread, card.ts, func(m *liveMessage) { m.Tapped = "" }) {
		return
	}
	if _, err := p.cards.redraw(ctx, p.newAPI(id.Token), card.channel, card.thread, card.ts, p.now()); err != nil {
		p.log.Printf("card %s/%s: its answers are offered again, but the card does not show them: %v", card.thread, card.ts, err)
	}
}

func (p *poller) refuseTap(ctx context.Context, id identity, card cardRef, askID, label string, why error) {
	p.cards.release(card.channel, card.thread, askID)
	p.cards.markReply(card.channel, card.thread, card.ts, func(m *liveMessage) { m.Tapped = "" })
	p.log.Printf("waiting card %s: %q not recorded, so the card offers its answers again: %v", askID, label, why)
	line := "⚠️ *" + mrkdwnEscapes.Replace(clipRunes(label, cardMaxDetailLen)) + "* was not recorded: " +
		mrkdwnEscapes.Replace(clipRunes(why.Error(), cardMaxReasonLen)) + ". Tap again, or reply here to say it."
	if err := p.markHint(ctx, id, card, line); err != nil {
		p.log.Printf("waiting card %s: its answers are offered again, but the card does not show it: %v", askID, err)
	}
}

func (p *poller) cardAction(ctx context.Context, id identity, a cardActionPush) error {
	if err := a.check(id); err != nil {
		p.log.Printf("card action %s: dropped: %v", a.ActionID, err)
		return nil
	}
	card, err := p.cards.located(a)
	at := slackTime(a.ActionTS)
	k, ok := card.tap()
	if !ok {
		p.log.Printf("card action %q: dropped: not a status-card button", a.ActionID)
		p.noteTap(ctx, id, a.ref(), card.ref(), at, "⚠️ *That button no longer does anything.* Reply in this thread to say what you wanted.")
		return nil
	}
	if err == nil {
		err = p.takeTap(ctx, id, card, k)
	}
	if errors.Is(err, errNoStatusSession) && !k.Undo {
		err = p.handOn(ctx, id, card, k, at)
	}
	if why, dropped := droppedBecause(err); dropped {
		p.log.Printf("card action %s on %s/%s: dropped: %v", a.ActionID, a.Channel, a.ThreadTS, err)
		p.noteTap(ctx, id, a.ref(), card.ref(), at, "⚠️ *"+mrkdwnEscapes.Replace(k.Label)+"* reached no one: "+why+" Reply in this thread to say it.")
		err = nil
	}
	if errors.Is(err, errTapNotRecorded) {
		p.log.Printf("card action %s on %s/%s: %v", a.ActionID, a.Channel, a.ThreadTS, err)
		err = nil
	}
	p.readBack(ctx, id, a, card, k, err)
	return err
}

func (p *poller) takeTap(ctx context.Context, id identity, a cardActionPush, k askTap) error {
	if done, err := p.unfold(ctx, id, a, k); done {
		return err
	}
	return p.applyTap(ctx, id, a, k)
}

func tapKey(a cardActionPush) string {
	return fmt.Sprintf("slack-action:%s:%s:%s", a.Channel, a.MessageTS, a.ActionTS)
}

func (p *poller) handOn(ctx context.Context, id identity, a cardActionPush, k askTap, at time.Time) error {
	if k.Explain {
		switch held, err := p.tellHolder(ctx, id, a, k, id.permalink(a.Channel, a.MessageTS, a.ThreadTS)); {
		case err != nil:
			return err
		case !held:
			return fmt.Errorf("%w, and no session holds this DM", errNoStatusSession)
		}
		return p.tapDrawn(a.ref(), at, p.markHint(ctx, id, a.ref(), askedForMore(at, p.now())))
	}
	label := mrkdwnEscapes.Replace(k.Label)
	undo := ""
	if k.ID != "" {
		if p.undoable(id, a.ref(), k) {
			undo = tapKey(a)
		}
		won, prior, err := p.cards.claim(a.Channel, a.ThreadTS, k.ID, waitsAnswer{card: a.ref(), tap: k, other: k.Other, at: at, dedup: tapKey(a), undo: undo}.recorded(p.now()))
		if err != nil {
			return err
		}
		if !won {
			p.log.Printf("ask %s: %q dropped: already answered with %q", k.ID, k.Label, prior.Label)
			return nil
		}
	}
	if err := p.handOver(ctx, id, a, k, "✓ *"+label+"* · answered "+clock(at, p.now())); err != nil {
		return err
	}
	if k.ID != "" {
		db := p.plane.DB()
		ended, err := p.threadPosters(ctx, a.Channel, a.ThreadTS)
		if err != nil {
			p.log.Printf("waiting ask %s: the board was not told: %v", k.ID, err)
		}
		p.forwardAnswer(ctx, db, ended, boardAnswer{Waits: k.ID, Label: k.Label, Other: k.Other, At: at, Permalink: id.permalink(a.Channel, a.MessageTS, a.ThreadTS)})
	}
	if k.ID != "" && !k.Other && !p.answerCopies(ctx, id, a.ref(), k.Label, false, at, undo) {
		p.recordPRTap(ctx, id, a.ref(), k.ID, k.Label, a.press())
	}
	return nil
}

func (p *poller) handOver(ctx context.Context, id identity, a cardActionPush, k askTap, answered string) error {
	held, err := p.tellHolder(ctx, id, a, k, id.permalink(a.Channel, a.MessageTS, a.ThreadTS))
	if err != nil || !held {
		if k.ID != "" {
			p.cards.release(a.Channel, a.ThreadTS, k.ID)
		}
		if err == nil {
			err = errors.New("no session holds this DM and the card command did not record it")
		}
		return fmt.Errorf("%w, and no one else was told: %w", errNoStatusSession, err)
	}
	p.cards.answerDelivered(a.Channel, a.ThreadTS, k.ID)
	if err := p.markAnswer(ctx, id, a.ref(), k.ID, answered+" · sent to the session holding this DM"); err != nil {
		p.log.Printf("card action %s on %s: sent on, but the card still shows its buttons: %v", a.ActionID, a.MessageTS, err)
	}
	p.react(ctx, id, a.ref(), cardAnsweredReaction)
	return nil
}

func (p *poller) tellHolder(ctx context.Context, id identity, a cardActionPush, k askTap, permalink string) (bool, error) {
	db := p.plane.DB()
	w, held, err := dmHolderWatch(ctx, db, a.Channel)
	if err != nil || !held {
		return false, err
	}
	who := cardSource(id, tapHow(k), slackTime(a.MessageTS).In(pacific).Format(cardTimeLayout), permalink)
	capture := captureLine(id, k, slackTime(a.ActionTS), "Slack card "+permalink)
	line := tapLine(who, k, capture, false) +
		" The session that posted the card has ended, so this comes to you as the session holding the DM: route it to the lane it is for."
	msg := actionPayload{
		WatchID: w.ID, ChannelID: a.Channel, ThreadTS: a.ThreadTS, TS: a.MessageTS, ActionTS: a.ActionTS, Label: k.Label, ID: k.ID,
		Waits: k.Waits, Other: k.Other, Question: k.Question, Options: k.Options, PosterEnded: true, Permalink: permalink,
	}
	if k.Waits && !k.Explain {
		msg.Capture, msg.NotesLabel = capture, slackLabel(k)
	}
	if err := p.deliver(ctx, db, w, delivery{
		base:   eventAction,
		origin: event.OriginHuman,
		dedup:  tapKey(a),
		line:   line,
		payload: func(typ string) []byte {
			msg.Type = typ
			return mustJSON(msg)
		},
	}); err != nil {
		return false, err
	}
	if k.ID != "" {
		return true, nil
	}
	return true, p.owed.owe(ctx, owedReply{ThreadTS: a.ThreadTS, TS: a.ActionTS, Text: "Tapped " + k.Label + " on " + cmp.Or(k.Question, "a card"), Permalink: permalink})
}

func droppedBecause(err error) (string, bool) {
	switch {
	case errors.Is(err, errNoStatusSession):
		return "the session that posted this card has ended.", true
	case errors.Is(err, errCardMoved):
		return "this card now asks something else.", true
	case errors.Is(err, errUndoClosed):
		return "the ten minutes to undo this answer had passed, so it stands.", true
	case errors.Is(err, errNoStoredCard):
		return "cc-slack no longer holds the card this answers, so the answer was not kept.", true
	}
	return "", false
}

func (p *poller) noteTap(ctx context.Context, id identity, tapped, card cardRef, at time.Time, line string) {
	p.cards.markReply(card.channel, card.thread, card.ts, func(m *liveMessage) { m.Hint = line })
	for _, r := range slices.Compact([]cardRef{card, tapped}) {
		if err := p.tapDrawn(r, at, p.drawNote(ctx, id, r, line)); err != nil {
			p.log.Printf("card %s/%s: its tap note is not drawn: %v", r.thread, r.ts, err)
		}
	}
}

func (p *poller) drawNote(ctx context.Context, id identity, r cardRef, line string) error {
	api := p.newAPI(id.Token)
	post, err := messageAt(ctx, api, r.channel, r.thread, r.ts)
	if err != nil {
		return err
	}
	return updateMessage(ctx, api, r.channel, r.ts, post.Text, notedBlocks(post.Blocks.BlockSet, line))
}

func notedBlocks(blocks []slack.Block, line string) []slack.Block {
	kept := slices.DeleteFunc(slices.Clone(blocks), func(b slack.Block) bool { return b.ID() == cardTapNoteBlock || b.ID() == cardWaitBlock })
	return append(kept, mrkdwn(cardTapNoteBlock, line))
}

func (p *poller) readBack(ctx context.Context, id identity, tapped, card cardActionPush, k askTap, tapErr error) {
	api := p.newAPI(id.Token)
	r := tapped.ref()
	post, err := messageAt(ctx, api, r.channel, r.thread, r.ts)
	if err != nil {
		p.log.Printf("card %s/%s: its tap cannot be read back: %v", r.thread, r.ts, err)
		return
	}
	if tapShows(post, tapped.ActionID, slackTime(tapped.ActionTS)) {
		return
	}
	if ans, done := p.cards.answer(card.Channel, card.ThreadTS, k.ID); k.ID != "" && done && post.Text == cardCopyLead {
		p.answerCopy(ctx, id, api, card.ref(), r, post, ans)
		return
	}
	line := p.unshownTap(card, k, tapErr)
	p.log.Printf("card %s/%s: tap read back unchanged: %s", r.thread, r.ts, line)
	if _, _, err := api.PostMessageContext(ctx, r.channel, id.postOptions(line, r.thread)...); err != nil {
		p.log.Printf("card %s/%s: its unchanged tap is not reported in the thread: %v", r.thread, r.ts, err)
	}
}

func (p *poller) answerCopy(ctx context.Context, id identity, api slackAPI, card, copied cardRef, post slack.Message, ans askAnswer) {
	if p.cards.copyIn(copied.channel, copied.thread, card.ts) == "" {
		p.cards.rememberCopy(copied.channel, copied.thread, liveCardCopy{Card: card.ts, TS: copied.ts}, p.now())
	}
	line := "✓ *" + mrkdwnEscapes.Replace(clipRunes(ans.Label, cardMaxDetailLen)) + "* · answered " + clock(ans.At, p.now())
	if err := markCopy(ctx, api, id, card, copied, post, line, p.cards.undoing(card)); err != nil {
		p.log.Printf("card copy %s stays as drawn after its answer: %v", copied.ts, err)
	}
}

func (p *poller) unshownTap(a cardActionPush, k askTap, tapErr error) string {
	line := "⚠️ Your tap on *" + mrkdwnEscapes.Replace(k.Label) + "* did not change this message"
	if ans, done := p.cards.answer(a.Channel, a.ThreadTS, k.ID); k.ID != "" && done {
		return line + ", but the answer on record is *" + mrkdwnEscapes.Replace(ans.Label) + "*."
	}
	if tapErr != nil || k.ID != "" {
		return line + " and was not taken. Tap again, or reply here to say it."
	}
	return line + ", but it was taken."
}

func tapShows(post slack.Message, actionID string, at time.Time) bool {
	if post.Edited != nil && !slackTime(post.Edited.Timestamp).Before(at) {
		return true
	}
	return !slices.ContainsFunc(post.Blocks.BlockSet, func(b slack.Block) bool { return offersAction(b, actionID) })
}

func offersAction(block slack.Block, actionID string) bool {
	switch b := block.(type) {
	case *slack.ActionBlock:
		return slices.ContainsFunc(b.Elements.ElementSet, func(e slack.BlockElement) bool {
			button, isButton := e.(*slack.ButtonBlockElement)
			return isButton && button.ActionID == actionID
		})
	case *slack.SectionBlock:
		return b.Accessory != nil && b.Accessory.ButtonElement != nil && b.Accessory.ButtonElement.ActionID == actionID
	}
	return false
}

func (p *poller) unfold(ctx context.Context, id identity, a cardActionPush, k askTap) (bool, error) {
	if !k.Fold && !k.Explain {
		return false, nil
	}
	shown, err := p.showFold(ctx, id, a.ref())
	return shown || k.Fold || err != nil, err
}

func (p *poller) applyTap(ctx context.Context, id identity, a cardActionPush, k askTap) error {
	if k.Undo {
		return p.undoTap(ctx, id, a, k)
	}
	if k.expired(slackTime(a.ActionTS)) {
		p.log.Printf("card action %s on %s: dropped: the ask expired", a.ActionID, a.MessageTS)
		if err := p.markAsk(ctx, id, a, k); err != nil {
			p.log.Printf("card action %s on %s: the card still offers the expired ask: %v", a.ActionID, a.MessageTS, err)
		}
		return nil
	}
	if ans, done := p.cards.answer(a.Channel, a.ThreadTS, k.ID); k.ID != "" && done {
		if ans.Undelivered != tapKey(a) {
			p.log.Printf("card action %s on %s: dropped: the ask was already answered", a.ActionID, a.MessageTS)
			return p.repeatTap(ctx, id, a, k, ans)
		}
		p.log.Printf("card action %s on %s: its answer was recorded but never delivered, so it is delivered now", a.ActionID, a.MessageTS)
		p.cards.release(a.Channel, a.ThreadTS, k.ID)
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
	watches, err := p.dmWatches(ctx, db, a.Channel, a.ThreadTS)
	if err != nil {
		return err
	}
	if len(watches) == 0 {
		return errNoStatusSession
	}
	switch {
	case k.Explain:
		err = p.markHint(ctx, id, a.ref(), askedForMore(tappedAt, p.now()))
	case k.ID != "":
		err = p.markAsk(ctx, id, a, k)
	default:
		err = p.markOption(ctx, id, a.ref(), "✓ *"+k.Label+"* · "+p.now().In(pacific).Format(cardTimeLayout))
	}
	if p.tapDrawn(a.ref(), tappedAt, err) != nil {
		p.log.Printf("card action %s on %s: the card still shows its buttons: %v", a.ActionID, a.MessageTS, err)
	}
	line := tapLine(who, k, "", false)
	for _, w := range watches {
		msg := actionPayload{
			WatchID: w.ID, ChannelID: a.Channel, ThreadTS: a.ThreadTS, TS: a.MessageTS, ActionTS: a.ActionTS, Label: k.Label, ID: k.ID,
			Waits: k.Waits, Question: k.Question, Options: k.Options, Permalink: permalink,
		}
		if err := p.deliver(ctx, db, w, delivery{
			base:   eventAction,
			origin: event.OriginHuman,
			dedup:  tapKey(a),
			line:   line,
			payload: func(typ string) []byte {
				msg.Type = typ
				return mustJSON(msg)
			},
		}); err != nil {
			if k.ID != "" && !k.Explain {
				p.cards.release(a.Channel, a.ThreadTS, k.ID)
			}
			return err
		}
	}
	p.cards.answerDelivered(a.Channel, a.ThreadTS, k.ID)
	return nil
}

func (p *poller) numberedTap(id identity, m slack.Message) (cardActionPush, bool) {
	if !id.fromOperator(m) || m.SubType != "" || m.ThreadTimestamp == "" || m.ThreadTimestamp == m.Timestamp {
		return cardActionPush{}, false
	}
	ts, pick, ok := p.cards.numberedPick(id.DMChannelID, m.ThreadTimestamp, strings.TrimSpace(m.Text))
	return cardActionPush{User: m.User, Channel: id.DMChannelID, MessageTS: ts, ThreadTS: m.ThreadTimestamp,
		ActionID: pick.actionID, Value: pick.value, ActionTS: m.Timestamp, Typed: true}, ok
}

func (p *poller) tapPRCard(ctx context.Context, id identity, a cardActionPush, k askTap, pr refID, at time.Time) error {
	card := offeredCard{ref: a.ref(), waits: k.ID, pr: pr}
	if p.answerPRCard(ctx, id, card, k.Label, at, "✓ *"+mrkdwnEscapes.Replace(k.Label)+"* · answered "+clock(at, p.now()), false, a.press()) {
		p.answerCopies(ctx, id, a.ref(), k.Label, true, at, "")
	}
	return nil
}

func (p *poller) answerPRCard(ctx context.Context, id identity, c offeredCard, label string, at time.Time, line string, applied bool, press cardPress) bool {
	won, prior, err := p.cards.claim(c.ref.channel, c.ref.thread, c.waits, askAnswer{At: at, Label: label})
	switch {
	case err != nil:
		p.log.Printf("PR card %s: %q dropped: %v", c.waits, label, err)
		return applied
	case !won:
		p.log.Printf("PR card %s: %q dropped: already answered with %q", c.waits, label, prior.Label)
		return applied
	}
	if err := p.tapDrawn(c.ref, at, p.markAnswer(ctx, id, c.ref, c.waits, line)); err != nil {
		p.log.Printf("PR card %s: %q taken, but the card still shows its buttons: %v", c.waits, label, err)
	}
	if applied {
		return true
	}
	return p.applyPRTap(ctx, id, c.ref, owedTap{Card: c.ref.ts, Answer: newPRAnswer(c.pr, c.waits, label, press, p.cards.heads(c.ref))})
}

func (p *poller) answerCopies(ctx context.Context, id identity, tapped cardRef, label string, applied bool, at time.Time, undo string) bool {
	prs, picks := p.cards.otherOffers(tapped)
	if len(id.Config.PRLinks.CardCommand) == 0 {
		prs = nil
	}
	link := id.permalink(tapped.channel, tapped.ts, tapped.thread)
	line := "✓ *" + mrkdwnEscapes.Replace(label) + "* · answered " + clock(at, p.now()) + " on " + slackLink(link, "the other card")
	for _, c := range prs {
		if applied = p.answerPRCard(ctx, id, c, label, at, line, applied, cardPress{tapped.ts, "answered " + link}); !applied {
			break
		}
	}
	for _, c := range picks {
		copied := waitsAnswer{
			card: c.ref, tap: askTap{ID: c.waits, Label: label, Question: c.question, Options: c.options, Waits: true}, at: at,
			source: cardSource(id, "Slack tap", at.In(pacific).Format(cardTimeLayout), link), where: "Slack card " + link,
			dedup: fmt.Sprintf("slack-copy:%s:%s:%s", c.ref.channel, c.ref.ts, c.waits), undo: undo,
		}
		_, err := p.answerWaits(ctx, id, copied)
		if errors.Is(err, errNoStatusSession) {
			copied.settled, copied.via = true, link
			_, err = p.answerWaits(ctx, id, copied)
		}
		if err != nil {
			p.log.Printf("waiting ask %s stays open after the tap on %s: %v", c.waits, tapped.ts, err)
		}
	}
	return applied
}

func (p *poller) tapRow(ctx context.Context, id identity, a cardActionPush, k askTap, at time.Time, permalink string) error {
	if pr, ok := p.cards.prRow(a.Channel, a.ThreadTS, k.ID); ok && len(id.Config.PRLinks.CardCommand) > 0 {
		return p.tapPRRow(ctx, id, a, k, pr, at)
	}
	db := p.plane.DB()
	watches, err := p.dmWatches(ctx, db, a.Channel, a.ThreadTS)
	if err != nil {
		return err
	}
	if len(watches) == 0 {
		return errNoStatusSession
	}
	if _, _, err := p.cards.claim(a.Channel, a.ThreadTS, k.ID, askAnswer{At: at, Label: k.Label}); err != nil {
		return err
	}
	if err := p.tapDrawn(a.ref(), at, p.drawTap(ctx, id, a.ref(), cardRowBlock+k.ID, "✓ *"+mrkdwnEscapes.Replace(k.Label)+"* · answered "+clock(at, p.now()), true)); err != nil {
		p.log.Printf("row %s on %s: %q taken, but the card still shows its buttons: %v", k.ID, a.MessageTS, k.Label, err)
	}
	if err := p.decideOnBoard(ctx, db, watches, boardDecision{Row: k.ID, Label: k.Label, At: at, Permalink: permalink}); err != nil {
		p.cards.release(a.Channel, a.ThreadTS, k.ID)
		p.log.Printf("row %s on %s: %q did not reach the board: %v", k.ID, a.MessageTS, k.Label, err)
		if _, err := p.cards.redraw(ctx, p.newAPI(id.Token), a.Channel, a.ThreadTS, a.MessageTS, p.now()); err != nil {
			p.log.Printf("row %s: the card still shows %q: %v", k.ID, k.Label, err)
		}
		return p.confirmTap(ctx, id, a.ref(), cardRowBlock+k.ID,
			"⚠️ *"+mrkdwnEscapes.Replace(k.Label)+"* did not reach the board: "+mrkdwnEscapes.Replace(clipRunes(err.Error(), cardMaxDetailLen)))
	}
	return nil
}

func (p *poller) tapPRRow(ctx context.Context, id identity, a cardActionPush, k askTap, pr refID, at time.Time) error {
	won, prior, err := p.cards.claim(a.Channel, a.ThreadTS, k.ID, askAnswer{At: at, Label: k.Label})
	if err != nil {
		return err
	}
	if !won {
		p.log.Printf("PR row %s: %q dropped: already answered with %q", k.ID, k.Label, prior.Label)
		return nil
	}
	if err := p.tapDrawn(a.ref(), at, p.drawTap(ctx, id, a.ref(), cardRowBlock+k.ID, "✓ *"+mrkdwnEscapes.Replace(k.Label)+"* · answered "+clock(at, p.now()), true)); err != nil {
		p.log.Printf("PR row %s: %q taken, but the card still shows its buttons: %v", k.ID, k.Label, err)
	}
	p.applyPRTap(ctx, id, a.ref(), owedTap{Card: a.MessageTS, Row: true, Answer: newPRAnswer(pr, k.ID, k.Label, a.press(), p.cards.heads(a.ref()))})
	return nil
}

func (p *poller) repeatTap(ctx context.Context, id identity, a cardActionPush, k askTap, ans askAnswer) error {
	tapped := slackTime(a.ActionTS)
	if !k.Waits {
		if err := p.tapDrawn(a.ref(), tapped, p.markAsk(ctx, id, a, k)); err != nil {
			p.log.Printf("card action %s on %s: the card still offers the answered ask: %v", a.ActionID, a.MessageTS, err)
		}
		return nil
	}
	if ans.Drawn {
		return nil
	}
	err := p.tapDrawn(a.ref(), tapped, p.drawTap(ctx, id, a.ref(), cardActionsBlock, "✓ *"+mrkdwnEscapes.Replace(clipRunes(ans.Label, cardMaxDetailLen))+"* · answered "+clock(ans.At, p.now()), true))
	if err == nil {
		p.cards.answerDrawn(a.Channel, a.ThreadTS, k.ID)
	}
	return err
}

func (w waitsAnswer) recorded(now time.Time) askAnswer {
	ans := askAnswer{At: w.at, Label: w.tap.Label, Other: w.other, Undelivered: w.dedup, Withdrawn: w.withdrawn, Tap: w.undo}
	if w.withdrawn || w.settled {
		ans.Undelivered = ""
	}
	if until := w.at.Add(cardUndoWindow); w.undo != "" && now.Before(until) {
		ans.UndoUntil = until
	}
	return ans
}

func (p *poller) answerWaits(ctx context.Context, id identity, w waitsAnswer) (cardAnswerResult, error) {
	won, prior, err := p.cards.claim(w.card.channel, w.card.thread, w.tap.ID, w.recorded(p.now()))
	if err != nil {
		return cardAnswerResult{}, err
	}
	if !won {
		p.log.Printf("waiting ask %s: %q dropped: already answered with %q", w.tap.ID, w.tap.Label, prior.Label)
		return cardAnswerResult{Already: true, Label: prior.Label, Other: prior.Other, At: prior.At}, nil
	}
	if w.withdrawn {
		return p.withdrawWaits(ctx, id, w)
	}
	if w.settled {
		return p.settleWaits(ctx, id, w)
	}
	answered := w.answeredLine(p.now())
	rec := p.recordWaits(ctx, id, w, answered)
	switch rec {
	case tapRecording:
		return cardAnswerResult{Answered: true, Label: w.tap.Label, Other: w.other, At: w.at}, nil
	case tapRefused:
		return cardAnswerResult{}, fmt.Errorf("waiting ask %s: %q: %w", w.tap.ID, w.tap.Label, errTapNotRecorded)
	}
	db := p.plane.DB()
	watches, err := p.waitsWatches(ctx, db, id, w, rec)
	if err != nil {
		return cardAnswerResult{}, err
	}
	if rec == tapUnrecorded {
		p.drawAnswered(ctx, id, w, answered)
	}
	permalink := id.permalink(w.card.channel, w.card.ts, w.card.thread)
	if err := p.deliverWaits(ctx, db, id, w, watches, permalink, rec == tapRecorded); err != nil {
		return cardAnswerResult{}, err
	}
	p.cards.answerDelivered(w.card.channel, w.card.thread, w.tap.ID)
	if w.via == "" {
		p.forwardAnswer(ctx, db, watches, boardAnswer{Waits: w.tap.ID, Label: w.tap.Label, Other: w.other, At: w.at, Permalink: permalink})
	}
	p.answerSuperseded(ctx, id, w)
	return cardAnswerResult{Answered: true, Label: w.tap.Label, Other: w.other, At: w.at}, nil
}

func (p *poller) withdrawWaits(ctx context.Context, id identity, w waitsAnswer) (cardAnswerResult, error) {
	reason := clipRunes(mrkdwnEscapes.Replace(w.tap.Label), cardMaxReasonLen) + " · " + clock(w.at, p.now())
	line := "*Withdrawn:* " + reason
	if ref, err := parsePermalink(w.via); err == nil && ref.ts != "" && ref.ts != w.card.ts {
		line = "*Replaced:* " + reason + " · " + slackLink(w.via, "what replaced it")
	}
	if err := p.tapDrawn(w.card, w.at, p.markAnswer(ctx, id, w.card, w.tap.ID, line)); err != nil {
		p.cards.release(w.card.channel, w.card.thread, w.tap.ID)
		return cardAnswerResult{}, err
	}
	p.react(ctx, id, w.card, cardWithdrawnReaction)
	return cardAnswerResult{Answered: true, Label: w.tap.Label, Other: true, At: w.at}, nil
}

func (p *poller) settleWaits(ctx context.Context, id identity, w waitsAnswer) (cardAnswerResult, error) {
	line := "✓ " + slackLink(w.via, clipRunes(w.tap.Label, cardMaxDetailLen)) + " · " + clock(w.at, p.now())
	if err := p.tapDrawn(w.card, w.at, p.markAnswer(ctx, id, w.card, w.tap.ID, line)); err != nil {
		p.cards.release(w.card.channel, w.card.thread, w.tap.ID)
		return cardAnswerResult{}, err
	}
	p.react(ctx, id, w.card, cardAnsweredReaction)
	p.answerSuperseded(ctx, id, w)
	return cardAnswerResult{Answered: true, Label: w.tap.Label, Other: true, At: w.at}, nil
}

func (w waitsAnswer) answeredLine(now time.Time) string {
	answer := "✓ *" + mrkdwnEscapes.Replace(w.tap.Label) + "* · answered "
	if w.other {
		answer = "✓ *Other:* " + mrkdwnEscapes.Replace(clipRunes(w.tap.Label, cardMaxDetailLen)) + " · answered "
	}
	if w.via != "" {
		answer += "on " + slackLink(w.via, w.viaName) + " "
	}
	return answer + clock(w.at, now)
}

func (p *poller) recordWaits(ctx context.Context, id identity, w waitsAnswer, answered string) tapRecord {
	if !w.records {
		return tapUnrecorded
	}
	undoUntil := time.Time{}
	if w.via != "" {
		undoUntil = w.undoUntil()
	}
	return p.recordTap(ctx, id, w.card, w.tap, w.press, w.at, undoUntil, answered)
}

func (p *poller) waitsWatches(ctx context.Context, db *sql.DB, id identity, w waitsAnswer, rec tapRecord) ([]watch, error) {
	watches, err := p.dmWatches(ctx, db, w.card.channel, w.card.thread)
	if rec != tapUnrecorded {
		if err != nil {
			p.log.Printf("waiting ask %s: recorded, but its posting session is not told: %v", w.tap.ID, err)
		}
		return watches, nil
	}
	if err == nil && len(watches) == 0 {
		err = fmt.Errorf("%w: %s/%s", errNoStatusSession, w.card.channel, w.card.thread)
	}
	if err != nil {
		p.cards.release(w.card.channel, w.card.thread, w.tap.ID)
		p.redrawOffer(ctx, id, w.card, w.tap.ID)
		return nil, err
	}
	return watches, nil
}

func (p *poller) drawAnswered(ctx context.Context, id identity, w waitsAnswer, answered string) {
	if err := p.tapDrawn(w.card, w.at, p.markAnswer(ctx, id, w.card, w.tap.ID, answered)); err != nil {
		p.log.Printf("waiting ask %s on %s: answered, but the card still shows its buttons: %v", w.tap.ID, w.card.ts, err)
	}
	p.react(ctx, id, w.card, cardAnsweredReaction)
}

func (p *poller) deliverWaits(ctx context.Context, db *sql.DB, id identity, w waitsAnswer, watches []watch, permalink string, recorded bool) error {
	capture, notesLabel := "", ""
	if !recorded {
		capture, notesLabel = captureLine(id, w.tap, w.at, w.where), slackLabel(w.tap)
	}
	line := tapLine(w.source, w.tap, capture, recorded)
	for _, watch := range watches {
		msg := actionPayload{
			WatchID: watch.ID, ChannelID: w.card.channel, ThreadTS: w.card.thread, TS: w.card.ts, ActionTS: w.actionTS, Label: w.tap.Label, ID: w.tap.ID,
			Waits: true, Other: w.other, Question: w.tap.Question, Options: w.tap.Options, Via: w.via,
			Capture: capture, NotesLabel: notesLabel, Recorded: recorded, Permalink: permalink,
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
			return fmt.Errorf("waiting ask %s keeps its answer %q, but delivering it to watch %d failed: %w", w.tap.ID, w.tap.Label, watch.ID, err)
		}
	}
	return nil
}

const (
	surfaceDashboard      = "dashboard"
	cardAnsweredReaction  = "white_check_mark"
	cardNoLongerWaits     = "This PR no longer waits on your tap."
	cardWithdrawnReaction = "x"
)

func unmarkWithdrawn(ctx context.Context, api slackAPI, r cardRef) error {
	if err := api.RemoveReactionContext(ctx, cardWithdrawnReaction, slack.NewRefToMessage(r.channel, r.ts)); err != nil && errorCode(err) != "no_reaction" {
		return err
	}
	return nil
}

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

func askedAgain(title, card string) string {
	return "↪ *" + mrkdwnEscapes.Replace(title) + "* is asked again as a card with buttons: " + slackLink(card, "answer it there")
}

func (p *poller) undoable(id identity, card cardRef, k askTap) bool {
	if permissionAsk(k.ID) {
		return false
	}
	if len(id.Config.PRLinks.CardCommand) == 0 || k.Other {
		return true
	}
	prs, _ := p.cards.otherOffers(card)
	_, carriesPR := p.cards.waitingPR(card.channel, card.thread, k.ID)
	return !carriesPR && len(prs) == 0
}

func (p *poller) undoTap(ctx context.Context, id identity, a cardActionPush, k askTap) error {
	ans, done := p.cards.answer(a.Channel, a.ThreadTS, k.ID)
	if !done {
		p.log.Printf("card action %s on %s: dropped: the answer was already undone", a.ActionID, a.MessageTS)
		return nil
	}
	if at := slackTime(a.ActionTS); ans.Tap == "" || !at.Before(ans.UndoUntil) {
		p.cards.redrawClosedUndos(ctx, id, p.cards.closeUndos(at), p.now())
		return errUndoClosed
	}
	return p.undoAnswer(ctx, id, a.ref(), ans, a.press(), "Slack tap")
}

func (p *poller) reopenAnswer(ctx context.Context, id identity, card string) error {
	link, err := parsePermalink(card)
	if err != nil {
		return err
	}
	ref, ok := p.cards.cardAt(link.channelID, link.ts)
	if !ok || ref.channel != id.DMChannelID {
		return fmt.Errorf("no card at %s in your DM", card)
	}
	waits := p.cards.undoing(ref)
	if waits == "" {
		return fmt.Errorf("card %s shows no answer to take back: %w", card, errUndoClosed)
	}
	ans, _ := p.cards.answer(ref.channel, ref.thread, waits)
	return p.undoAnswer(ctx, id, ref, ans, cardPress{formatTS(p.now()), "took the answer back off Slack"}, "Undo")
}

func (p *poller) undoAnswer(ctx context.Context, id identity, card cardRef, ans askAnswer, press cardPress, how string) error {
	db := p.plane.DB()
	listeners, err := p.cardListeners(ctx, db, card.channel, card.thread)
	if err != nil {
		return err
	}
	if len(listeners) == 0 && len(id.Config.PRLinks.CardCommand) == 0 {
		return errNoStatusSession
	}
	var errs []error
	for _, u := range p.cards.undo(card.channel, ans.Tap) {
		if err := p.reopen(ctx, id, db, press, u, how, fmt.Sprintf("they undid their answer %q", u.answer.Label)); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (p *poller) reopenReply(ctx context.Context, id identity, card, reply string) error {
	cardLink, err := parsePermalink(card)
	if err != nil {
		return err
	}
	replyLink, err := parsePermalink(reply)
	if err != nil {
		return err
	}
	ref, ok := p.cards.cardAt(cardLink.channelID, cardLink.ts)
	if !ok || ref.channel != id.DMChannelID {
		return fmt.Errorf("no card at %s in your DM", card)
	}
	msgs, err := fetchReplies(ctx, p.newAPI(id.Token), ref.channel, ref.thread, justBefore(replyLink.ts))
	if err != nil {
		return err
	}
	for i, m := range msgs {
		msgs[i] = dashboardWords(id, m)
	}
	at := slices.IndexFunc(msgs, func(m slack.Message) bool {
		return replyLink.channelID == ref.channel && m.Timestamp == replyLink.ts && m.Timestamp != ref.thread && id.fromOperator(m)
	})
	if at < 0 {
		return fmt.Errorf("%s is no reply of %s's in the thread of card %s", reply, id.name(), card)
	}
	words := strings.TrimSpace(msgs[at].Text)
	undone := p.cards.undoReply(ref, words)
	if len(undone) == 0 {
		return fmt.Errorf("card %s is not answered with the words of %s, %q", card, reply, words)
	}
	db := p.plane.DB()
	var errs []error
	for _, u := range undone {
		if err := p.reopen(ctx, id, db, cardPress{replyLink.ts, "wrote in the card's thread"}, u, "reply in the card's thread",
			fmt.Sprintf("their reply %q was taken as their answer", words)); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (p *poller) reopen(ctx context.Context, id identity, db *sql.DB, press cardPress, u undoneAnswer, how, why string) error {
	api := p.newAPI(id.Token)
	if _, err := p.cards.redraw(ctx, api, u.card.channel, u.card.thread, u.card.ts, p.now()); err != nil {
		p.log.Printf("card %s: its answer %q is undone, but the card still shows it: %v", u.card.ts, u.answer.Label, err)
	}
	if err := p.cards.redrawCopies(ctx, id, u.card); err != nil {
		p.log.Printf("card %s: its answer %q is undone, but its copies still show it: %v", u.card.ts, u.answer.Label, err)
	}
	if err := api.RemoveReactionContext(ctx, cardAnsweredReaction, slack.NewRefToMessage(u.card.channel, u.card.ts)); err != nil && errorCode(err) != "no_reaction" {
		p.log.Printf("card %s: its answer is undone, but the %s reaction stays: %v", u.card.ts, cardAnsweredReaction, err)
	}
	permalink := id.permalink(u.card.channel, u.card.ts, u.card.thread)
	p.noteSuperseded(ctx, id, u.supersedes, askedAgain(u.question, permalink))
	watches, err := p.cardListeners(ctx, db, u.card.channel, u.card.thread)
	if err != nil {
		return err
	}
	who := cardSource(id, how, slackTime(u.card.ts).In(pacific).Format(cardTimeLayout), permalink)
	rec, err := p.recordQuestionTap(ctx, id, u.card, askTap{ID: u.waits, Label: u.answer.Label, Other: u.answer.Other}, true, press)
	if err != nil {
		p.log.Printf("waiting card %s: the undo of %q stays with the sessions alone, since the card command did not take it: %v", u.waits, u.answer.Label, err)
	}
	recorded := rec == tapRecorded
	line := fmt.Sprintf("%s: %s to the waiting ask %q, so treat it as never given. Tell the lane that asked to stop or redirect "+
		"whatever it started on that answer and to supersede anything recorded from it; the card waits for their answer again.", who, why, u.waits)
	if recorded {
		line = fmt.Sprintf("%s: %s to the waiting ask %q, so treat it as never given. Your card command already posted the undo as the one record "+
			"the lane that asked reads: post no record of it; the card waits for their answer again.", who, why, u.waits)
	}
	for _, w := range watches {
		msg := actionPayload{
			WatchID: w.ID, ChannelID: u.card.channel, ThreadTS: u.card.thread, TS: u.card.ts, ActionTS: press.ActionTS, Label: u.answer.Label, ID: u.waits,
			Waits: true, Other: u.answer.Other, Question: u.question, Undo: true, Recorded: recorded, Permalink: permalink,
		}
		if err := p.deliver(ctx, db, w, delivery{
			base:   eventAction,
			origin: event.OriginHuman,
			dedup:  fmt.Sprintf("slack-undo:%s:%s:%s", u.card.channel, u.card.ts, press.ActionTS),
			line:   line,
			payload: func(typ string) []byte {
				msg.Type = typ
				return mustJSON(msg)
			},
		}); err != nil {
			return fmt.Errorf("waiting ask %s is open again, but telling watch %d its answer %q was undone failed: %w", u.waits, w.ID, u.answer.Label, err)
		}
	}
	return nil
}

func (p *poller) answerElsewhere(ctx context.Context, id identity, named, card, label string, other, settled, withdrawn bool, via, from string) (cardAnswerResult, error) {
	link, err := parsePermalink(card)
	if err != nil {
		return cardAnswerResult{}, err
	}
	ref, ok := p.cards.cardAt(link.channelID, link.ts)
	if !ok || ref.channel != id.DMChannelID {
		return cardAnswerResult{}, fmt.Errorf("no waiting card %q at %s in your DM", named, card)
	}
	thread := ref.thread
	kind, surface, name := answeredFrom(from, via)
	if waits, m, ok := p.cards.prCardAt(id.DMChannelID, thread, ref.ts); ok {
		switch {
		case withdrawn && m.Tapped != "":
			return cardAnswerResult{}, fmt.Errorf("PR card %s no longer waits on a tap: it shows %q", card, m.Tapped)
		case withdrawn:
			return p.withdrawPRCard(ctx, id, offeredCard{ref: cardRef{id.DMChannelID, thread, m.TS}, waits: waits, question: m.Card.Title}, label, via)
		case !other && !settled:
			return p.answerPRCardElsewhere(ctx, id, cardRef{id.DMChannelID, thread, m.TS}, waits, m, label, via, name)
		}
	}
	waits, m, ok := p.cards.waitingAt(id.DMChannelID, thread, ref.ts)
	if !ok {
		return cardAnswerResult{}, fmt.Errorf("no waiting card %q at %s in your DM", named, card)
	}
	answers := m.Card.answers()
	if !other && !withdrawn && !slices.Contains(answers, label) {
		return cardAnswerResult{}, fmt.Errorf("%q is not one of waiting card %q's answers %q; pass other for a write-in", label, waits, answers)
	}
	at := p.now()
	permalink := id.permalink(id.DMChannelID, m.TS, thread)
	answer := waitsAnswer{
		card:      cardRef{id.DMChannelID, thread, m.TS},
		tap:       askTap{ID: waits, Label: label, Question: m.Card.Title, Options: answers, Waits: true},
		other:     other,
		at:        at,
		source:    fmt.Sprintf("%s from %s, your operator, on %s for your status card posted %s (%s)", kind, id.name(), via, m.At.In(pacific).Format(cardTimeLayout), permalink),
		where:     surface + " " + via + ", which answers Slack card " + permalink,
		via:       via,
		viaName:   name,
		dedup:     fmt.Sprintf("slack-answer:%s:%s:%s", id.DMChannelID, thread, waits),
		press:     cardPress{formatTS(at), "answered on " + name},
		records:   true,
		settled:   settled,
		withdrawn: withdrawn,
	}
	answer.undo = p.dashboardUndoKey(id, answer, from)
	res, err := p.answerWaits(ctx, id, answer)
	if errors.Is(err, errNoStatusSession) {
		answer.settled = true
		res, err = p.answerWaits(ctx, id, answer)
	}
	if err == nil && res.Answered && !other && !withdrawn {
		answered := cardRef{id.DMChannelID, thread, m.TS}
		if !p.answerCopies(ctx, id, answered, label, false, at, answer.undo) {
			p.recordPRTap(ctx, id, answered, waits, label, cardPress{"", "answered " + via})
		}
		res.UndoUntil = answer.undoUntil()
	}
	return res, err
}

func (p *poller) dashboardUndoKey(id identity, a waitsAnswer, from string) string {
	if from != surfaceDashboard || a.other || a.withdrawn || !p.undoable(id, a.card, a.tap) {
		return ""
	}
	return a.dedup
}

func (w waitsAnswer) undoUntil() time.Time {
	if w.undo == "" {
		return time.Time{}
	}
	return w.at.Add(cardUndoWindow)
}

func (p *poller) withdrawPRCards(ctx context.Context, id identity, pr refID, reason string) ([]string, error) {
	withdrawn := []string{}
	for _, c := range p.cards.openPRCards(pr) {
		permalink := id.permalink(c.ref.channel, c.ref.ts, c.ref.thread)
		res, err := p.withdrawPRCard(ctx, id, c, reason, "")
		if err != nil {
			return nil, fmt.Errorf("PR card %s still waits on a tap: %w", permalink, err)
		}
		if res.Answered {
			withdrawn = append(withdrawn, permalink)
		}
	}
	return withdrawn, nil
}

func (p *poller) withdrawPRCard(ctx context.Context, id identity, c offeredCard, reason, via string) (cardAnswerResult, error) {
	return p.answerWaits(ctx, id, waitsAnswer{card: c.ref, tap: askTap{ID: c.waits, Label: reason, Question: c.question, Waits: true}, at: p.now(), via: via, withdrawn: true})
}

func answeredFrom(from, via string) (kind, surface, name string) {
	if from == surfaceDashboard {
		return "Dashboard answer", "the drive dashboard", "the dashboard"
	}
	if ref, err := parsePermalink(via); err == nil && ref.ts != "" {
		return "Answer relayed", "Slack message", "Slack"
	}
	return "Board answer", "the cc-present board", "the board"
}

func (p *poller) answerPRCardElsewhere(ctx context.Context, id identity, r cardRef, waits string, m liveMessage, label, via, name string) (cardAnswerResult, error) {
	permalink := id.permalink(r.channel, r.ts, r.thread)
	if answers := m.Card.answers(); !slices.Contains(answers, label) {
		return cardAnswerResult{}, fmt.Errorf("%q is not one of PR card %q's answers %q", label, waits, answers)
	}
	if prior, done := p.cards.answer(r.channel, r.thread, waits); done {
		return cardAnswerResult{Already: true, Label: prior.Label, Other: prior.Other, At: prior.At}, nil
	}
	if len(id.Config.PRLinks.CardCommand) == 0 {
		return cardAnswerResult{}, fmt.Errorf("PR card %s cannot take an answer: [pr_links] card_command is not set", permalink)
	}
	at := p.now()
	card := offeredCard{ref: r, waits: waits, pr: m.Card.refIDs()[0]}
	line := "✓ *" + mrkdwnEscapes.Replace(label) + "* · answered on " + slackLink(via, name) + " " + clock(at, p.now())
	if !p.answerPRCard(ctx, id, card, label, at, line, false, cardPress{"", "answered on " + via}) {
		return cardAnswerResult{}, fmt.Errorf("PR card %s did not take %q; the daemon log says why", permalink, label)
	}
	p.answerCopies(ctx, id, r, label, true, at, "")
	return cardAnswerResult{Answered: true, Label: label, At: at}, nil
}

func (p *poller) showFold(ctx context.Context, id identity, r cardRef) (bool, error) {
	pages := p.cards.fold(r.channel, r.thread, r.ts, p.now())
	if len(pages) == 0 {
		return false, nil
	}
	api := p.newAPI(id.Token)
	for _, page := range pages {
		if err := checkBlocks(page); err != nil {
			return true, err
		}
		if _, _, err := api.PostMessageContext(ctx, r.channel, id.postOptions(cardExplainLabel, r.thread, slack.MsgOptionBlocks(page...))...); err != nil {
			return true, fmt.Errorf("post the rest of card %s: %w", r.ts, err)
		}
	}
	p.cards.markReply(r.channel, r.thread, r.ts, func(m *liveMessage) { m.Unfolded = true })
	_, err := p.cards.redraw(ctx, api, r.channel, r.thread, r.ts, p.now())
	return true, err
}

func (p *poller) markOption(ctx context.Context, id identity, r cardRef, line string) error {
	defer p.cards.markCopies(ctx, id, r, line)
	return p.drawTap(ctx, id, r, cardActionsBlock, line, p.cards.markReply(r.channel, r.thread, r.ts, func(m *liveMessage) { m.Tapped = line }))
}

func (p *poller) markAnswer(ctx context.Context, id identity, r cardRef, askID, line string) error {
	err := p.markOption(ctx, id, r, line)
	if err == nil {
		p.cards.answerDrawn(r.channel, r.thread, askID)
	}
	return err
}

func askedForMore(at, now time.Time) string {
	return "🔎 *You asked for more at " + clock(at, now) + ".* An explanation follows in this thread."
}

func (p *poller) markHint(ctx context.Context, id identity, r cardRef, line string) error {
	return p.drawTap(ctx, id, r, cardWaitBlock, line, p.cards.markReply(r.channel, r.thread, r.ts, func(m *liveMessage) { m.Hint = line }))
}

func (p *poller) drawTap(ctx context.Context, id identity, r cardRef, blockID, line string, tracked bool) error {
	if tracked {
		redrawn, err := p.cards.redraw(ctx, p.newAPI(id.Token), r.channel, r.thread, r.ts, p.now())
		if redrawn[r.ts] {
			return err
		}
		if err != nil {
			p.log.Printf("card %s: its redraw failed, so the tap is drawn on the post as Slack holds it: %v", r.ts, err)
		}
	}
	return p.confirmTap(ctx, id, r, blockID, line)
}

func (p *poller) redrawWithFreshPRLines(ctx context.Context, id identity, r cardRef) {
	if len(id.Config.PRLinks.CardCommand) == 0 {
		return
	}
	var asked []refID
	p.cards.markReply(r.channel, r.thread, r.ts, func(m *liveMessage) {
		for _, pr := range m.Card.allPRs() {
			if ref, ok := parseRef(pr.URL); ok {
				asked = append(asked, ref)
			}
		}
	})
	if len(asked) == 0 {
		return
	}
	provided, _, err := askPRCards(ctx, id, asked, true)
	if err != nil {
		p.log.Printf("card %s keeps the PR lines it was drawn with: %v", r.ts, err)
		return
	}
	changed := false
	p.cards.markReply(r.channel, r.thread, r.ts, func(m *liveMessage) {
		for _, pr := range m.Card.allPRs() {
			ref, _ := parseRef(pr.URL)
			at := slices.IndexFunc(provided, func(c providedCard) bool {
				return c.Number == ref.Number && strings.EqualFold(c.Repo, ref.Owner+"/"+ref.Repo) && len(c.Card.PRs) > 0
			})
			if at < 0 {
				continue
			}
			now := provided[at].Card.PRs[0]
			fresh := *pr
			fresh.Summary, fresh.Review, fresh.Surprise, fresh.Plan, fresh.Refresher, fresh.Blocks, fresh.Lands = now.Summary, now.Review, now.Surprise, now.Plan, now.Refresher, now.Blocks, now.Lands
			changed = changed || !reflect.DeepEqual(fresh, *pr)
			*pr = fresh
		}
	})
	if !changed {
		return
	}
	if _, err := p.cards.redraw(ctx, p.newAPI(id.Token), r.channel, r.thread, r.ts, p.now()); err != nil {
		p.log.Printf("card %s: its redraw with the PR lines as they stand now failed: %v", r.ts, err)
	}
}

func (p *poller) tapDrawn(r cardRef, tapped time.Time, err error) error {
	if err == nil {
		p.log.Printf("card %s/%s: tap drawn %s after it", r.thread, r.ts, p.now().Sub(tapped).Round(time.Second))
	}
	return err
}

func (p *poller) markAsk(ctx context.Context, id identity, a cardActionPush, k askTap) error {
	at := slackTime(a.ActionTS)
	if !k.expired(at) {
		if _, _, err := p.cards.claim(a.Channel, a.ThreadTS, k.ID, askAnswer{At: at, Label: k.Label, Undelivered: tapKey(a)}); err != nil {
			return err
		}
	}
	line := "✓ *" + mrkdwnEscapes.Replace(k.Label) + "* · answered " + clock(at, p.now())
	if k.expired(at) {
		line = "⌛ *" + mrkdwnEscapes.Replace(k.Label) + "* · expired " + clock(time.Unix(k.Expires, 0), p.now())
	}
	return p.drawTap(ctx, id, a.ref(), cardAskBlock+k.ID, line, true)
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
		text, blocks := m.Text, tappedBlocks(m.Blocks.BlockSet, blockID, line)
		if blockID == cardActionsBlock {
			text, blocks = settledMessage(m.Blocks.BlockSet, line)
		}
		if blocks == nil {
			return nil
		}
		return updateMessage(ctx, api, r.channel, r.ts, text, blocks)
	}
	return fmt.Errorf("card %s is gone from its thread", r.ts)
}

func settledMessage(blocks []slack.Block, line string) (string, []slack.Block) {
	if !slices.ContainsFunc(blocks, func(b slack.Block) bool { return b.ID() == cardActionsBlock }) {
		return "", nil
	}
	title := ""
	settled := make([]slack.Block, 0, len(blocks))
	for _, block := range blocks {
		switch b := block.(type) {
		case *slack.HeaderBlock:
			title = cmp.Or(headerEmoji.ReplaceAllString(b.Text.Text, ""), b.Text.Text)
			settled = append(settled, header(title))
		case *slack.SectionBlock:
			if strings.HasPrefix(b.BlockID, cardAskBlock) && b.Accessory != nil {
				settled = append(settled, b)
			}
		}
	}
	return title, append(settled, mrkdwn(cardTappedBlock+cardActionsBlock, line))
}

func tappedBlocks(blocks []slack.Block, blockID, line string) []slack.Block {
	out := make([]slack.Block, 0, len(blocks))
	tapped := false
	for _, b := range blocks {
		if blockID != cardWaitBlock && strings.HasSuffix(b.ID(), cardWaitBlock) {
			continue
		}
		if b.ID() != blockID && b.ID() != cardTappedBlock+blockID {
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
