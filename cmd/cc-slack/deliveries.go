package main

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/slack-go/slack"

	"github.com/yasyf/cc-interact/agent"
)

const (
	sweepEvery       = 30 * time.Second
	redeliverAfter   = 60 * time.Second
	inFlightAfter    = 3 * time.Minute
	ownerMessageTTL  = 24 * time.Hour
	maxRedeliveries  = 3
	inFlightReaction = "hourglass_flowing_sand"
	drainGrace       = 15 * time.Second
)

var queueOperation = []byte(`"type":"queue-operation"`)

type transcriptScan struct {
	seen   bool
	queued bool
	next   int64
}

type ownerMessage struct {
	channelID     string
	ts            string
	message       slack.Message
	pickedAt      time.Time
	holder        string
	transcript    string
	transcriptAt  int64
	confirmedAt   int64
	redelivered   int
	redeliveredAt time.Time
	reactedAt     int64
}

func (h *deliveryHolds) recordOwnerMessage(ctx context.Context, channelID string, m slack.Message, at time.Time, holder string, mark transcriptMark) error {
	raw, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("encode owner message %s: %w", m.Timestamp, err)
	}
	if _, err := h.st.DB().ExecContext(ctx, `INSERT OR IGNORE INTO owner_messages(channel_id, ts, message, picked_at, holder, transcript, transcript_at)
VALUES (?, ?, ?, ?, ?, ?, ?)`, channelID, m.Timestamp, string(raw), at.UnixMilli(), holder, mark.path, mark.offset); err != nil {
		return fmt.Errorf("record owner message %s: %w", m.Timestamp, err)
	}
	return nil
}

func (h *deliveryHolds) ownerMessages(ctx context.Context) ([]ownerMessage, error) {
	rows, err := h.st.DB().QueryContext(ctx, `SELECT channel_id, ts, message, picked_at, holder, transcript, transcript_at, confirmed_at, redelivered, redelivered_at, reacted_at
FROM owner_messages ORDER BY ts`)
	if err != nil {
		return nil, fmt.Errorf("list owner messages: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []ownerMessage
	for rows.Next() {
		var (
			o                   ownerMessage
			raw                 string
			picked, redelivered int64
		)
		if err := rows.Scan(&o.channelID, &o.ts, &raw, &picked, &o.holder, &o.transcript, &o.transcriptAt, &o.confirmedAt, &o.redelivered, &redelivered, &o.reactedAt); err != nil {
			return nil, fmt.Errorf("scan owner message: %w", err)
		}
		if err := json.Unmarshal([]byte(raw), &o.message); err != nil {
			return nil, fmt.Errorf("decode owner message %s: %w", o.ts, err)
		}
		o.pickedAt = time.UnixMilli(picked)
		if redelivered != 0 {
			o.redeliveredAt = time.UnixMilli(redelivered)
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

func (h *deliveryHolds) settleOwnerMessage(ctx context.Context, o ownerMessage) error {
	if _, err := h.st.DB().ExecContext(ctx, `DELETE FROM owner_messages WHERE channel_id = ? AND ts = ?`, o.channelID, o.ts); err != nil {
		return fmt.Errorf("settle owner message %s: %w", o.ts, err)
	}
	return nil
}

func (h *deliveryHolds) updateOwnerMessage(ctx context.Context, o ownerMessage) error {
	redelivered := int64(0)
	if !o.redeliveredAt.IsZero() {
		redelivered = o.redeliveredAt.UnixMilli()
	}
	if _, err := h.st.DB().ExecContext(ctx, `UPDATE owner_messages SET holder = ?, transcript = ?, transcript_at = ?, confirmed_at = ?, redelivered = ?, redelivered_at = ?, reacted_at = ?
WHERE channel_id = ? AND ts = ?`, o.holder, o.transcript, o.transcriptAt, o.confirmedAt, o.redelivered, redelivered, o.reactedAt, o.channelID, o.ts); err != nil {
		return fmt.Errorf("update owner message %s: %w", o.ts, err)
	}
	return nil
}

type storedHold struct {
	Channel string        `json:"channel"`
	Message slack.Message `json:"message"`
	Watches []watch       `json:"watches"`
	Marks   []storedMark  `json:"marks"`
}

type storedMark struct {
	Path   string `json:"path"`
	Offset int64  `json:"offset"`
}

func (h *deliveryHolds) holdAck(ctx context.Context, held heldAck) error {
	s := storedHold{Channel: held.channel, Message: held.m, Watches: held.watches}
	for _, m := range held.marks {
		s.Marks = append(s.Marks, storedMark{Path: m.path, Offset: m.offset})
	}
	if _, err := h.st.DB().ExecContext(ctx, `INSERT OR REPLACE INTO held_acks(channel_id, ts, due_at, held) VALUES (?, ?, ?, ?)`,
		held.channel, held.m.Timestamp, held.due.UnixMilli(), string(mustJSON(s))); err != nil {
		return fmt.Errorf("hold the ack of %s: %w", held.m.Timestamp, err)
	}
	return nil
}

func (h *deliveryHolds) dueAcks(ctx context.Context, now time.Time) ([]heldAck, time.Time, error) {
	db := h.st.DB()
	var next time.Time
	var after sql.NullInt64
	if err := db.QueryRowContext(ctx, `SELECT MIN(due_at) FROM held_acks WHERE due_at > ?`, now.UnixMilli()).Scan(&after); err != nil {
		return nil, next, fmt.Errorf("find the next held ack: %w", err)
	}
	if after.Valid {
		next = time.UnixMilli(after.Int64)
	}
	rows, err := db.QueryContext(ctx, `SELECT held, due_at FROM held_acks WHERE due_at <= ? ORDER BY due_at`, now.UnixMilli())
	if err != nil {
		return nil, next, fmt.Errorf("read due held acks: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var due []heldAck
	for rows.Next() {
		var (
			raw string
			at  int64
			s   storedHold
		)
		if err := rows.Scan(&raw, &at); err != nil {
			return nil, next, fmt.Errorf("scan a held ack: %w", err)
		}
		if err := json.Unmarshal([]byte(raw), &s); err != nil {
			return nil, next, fmt.Errorf("decode a held ack: %w", err)
		}
		held := heldAck{channel: s.Channel, m: s.Message, due: time.UnixMilli(at), watches: s.Watches}
		for _, m := range s.Marks {
			held.marks = append(held.marks, transcriptMark{path: m.Path, offset: m.Offset})
		}
		due = append(due, held)
	}
	return due, next, rows.Err()
}

func (h *deliveryHolds) dropAck(ctx context.Context, held heldAck) error {
	if _, err := h.st.DB().ExecContext(ctx, `DELETE FROM held_acks WHERE channel_id = ? AND ts = ? AND due_at = ?`, held.channel, held.m.Timestamp, held.due.UnixMilli()); err != nil {
		return fmt.Errorf("drop the held ack of %s: %w", held.m.Timestamp, err)
	}
	return nil
}

func (h *deliveryHolds) saveCompaction(ctx context.Context, subjectID string, mark transcriptMark) error {
	_, err := h.st.DB().ExecContext(ctx, `INSERT OR REPLACE INTO compactions(subject_id, transcript, offset) VALUES (?, ?, ?)`, subjectID, mark.path, mark.offset)
	return err
}

func (h *deliveryHolds) dropCompaction(ctx context.Context, subjectID string) error {
	_, err := h.st.DB().ExecContext(ctx, `DELETE FROM compactions WHERE subject_id = ?`, subjectID)
	return err
}

func (h *deliveryHolds) compactions(ctx context.Context) (map[string]transcriptMark, error) {
	rows, err := h.st.DB().QueryContext(ctx, `SELECT subject_id, transcript, offset FROM compactions`)
	if err != nil {
		return nil, fmt.Errorf("load compactions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]transcriptMark{}
	for rows.Next() {
		var (
			subjectID string
			mark      transcriptMark
		)
		if err := rows.Scan(&subjectID, &mark.path, &mark.offset); err != nil {
			return nil, fmt.Errorf("scan a compaction: %w", err)
		}
		out[subjectID] = mark
	}
	return out, rows.Err()
}

func drainable(ctx context.Context, grace time.Duration) (context.Context, context.CancelFunc) {
	settled, cancel := context.WithCancel(context.WithoutCancel(ctx))
	stop := context.AfterFunc(ctx, func() {
		timer := time.AfterFunc(grace, cancel)
		context.AfterFunc(settled, func() { timer.Stop() })
	})
	return settled, func() {
		stop()
		cancel()
	}
}

func claudeConfigDir(getenv func(string) string) (string, error) {
	if dir := getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude"), nil
}

func sessionTranscript(ctx context.Context, db *sql.DB, subjectID string) (transcriptMark, error) {
	var session sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT session_id FROM subjects WHERE id = ?`, subjectID).Scan(&session); err != nil {
		return transcriptMark{}, fmt.Errorf("find the session of subject %s: %w", subjectID, err)
	}
	if session.String == "" {
		return transcriptMark{}, fmt.Errorf("subject %s names no session", subjectID)
	}
	dir, err := claudeConfigDir(os.Getenv)
	if err != nil {
		return transcriptMark{}, err
	}
	found, err := filepath.Glob(filepath.Join(dir, "projects", "*", session.String+".jsonl"))
	if err != nil {
		return transcriptMark{}, err
	}
	if len(found) == 0 {
		return transcriptMark{}, fmt.Errorf("no transcript for session %s under %s", session.String, filepath.Join(dir, "projects"))
	}
	return markTranscript(found[0])
}

func (t transcriptMark) scan(ts string) (transcriptScan, error) {
	scan := transcriptScan{next: t.offset}
	f, err := os.Open(t.path)
	if err != nil {
		return scan, err
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Seek(t.offset, io.SeekStart); err != nil {
		return scan, err
	}
	r := bufio.NewReaderSize(f, 1<<20)
	needle, at := []byte(ts), t.offset
	for {
		line, err := r.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			rest, rerr := r.ReadBytes('\n')
			line, err = append(bytes.Clone(line), rest...), rerr
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = nil
			}
			return scan, err
		}
		if bytes.Contains(line, needle) {
			if !bytes.Contains(line, queueOperation) {
				scan.seen = true
				return scan, nil
			}
			scan.queued = true
		}
		at += int64(len(line))
		if !scan.queued {
			scan.next = at
		}
	}
}

func (g cursorGroup) conversationHolder() watch {
	if g.owner.ID != 0 {
		return g.owner
	}
	return g.holder
}

func holderFor(watches []watch, id identity, channelID string) watch {
	if h := holderIn(watches, id.DMChannelID, channelID); h.ID != 0 {
		return h
	}
	return dmHolderIn(watches, id.DMChannelID)
}

func (p *poller) recordOwnerMessage(ctx context.Context, db *sql.DB, id identity, g cursorGroup, m slack.Message) error {
	if !id.fromOperator(m) || !routedSubtypes[m.SubType] || m.Timestamp == g.key.threadTS {
		return nil
	}
	holder := g.conversationHolder()
	var mark transcriptMark
	if holder.ID != 0 {
		var err error
		if mark, err = sessionTranscript(ctx, db, holder.SubjectID); err != nil {
			p.log.Printf("record %s/%s with no transcript to confirm it on: %v", g.key.channelID, m.Timestamp, err)
		}
	}
	p.sweepOpen = true
	return p.holds.recordOwnerMessage(ctx, g.key.channelID, m, p.now(), holder.SubjectID, mark)
}

func (p *poller) sweepOwnerMessages(ctx context.Context, db *sql.DB, api slackAPI, id identity, watches []watch) (time.Time, error) {
	now := p.now()
	if now.Before(p.swept.Add(sweepEvery)) {
		if !p.sweepOpen {
			return time.Time{}, nil
		}
		return p.swept.Add(sweepEvery), nil
	}
	p.swept = now
	open, err := p.holds.ownerMessages(ctx)
	p.sweepOpen = err == nil && len(open) > 0
	if !p.sweepOpen {
		return time.Time{}, err
	}
	for _, o := range open {
		if err := p.sweepOwnerMessage(ctx, db, api, id, watches, o, now); err != nil {
			p.log.Printf("owner message %s/%s: sweep: %v", o.channelID, o.ts, err)
		}
	}
	return now.Add(sweepEvery), nil
}

func (p *poller) sweepOwnerMessage(ctx context.Context, db *sql.DB, api slackAPI, id identity, watches []watch, o ownerMessage, now time.Time) error {
	owed, err := p.owed.owes(ctx, o.ts)
	if err != nil {
		return err
	}
	age := now.Sub(o.pickedAt)
	if owed && age >= redeliverAfter {
		if owed, err = p.unansweredInThread(ctx, api, id, o); err != nil {
			return err
		}
	}
	if !owed || age >= ownerMessageTTL {
		return p.settleOwnerMessage(ctx, api, o, owed)
	}
	queued := p.confirm(&o, now)
	if o.confirmedAt == 0 && age >= redeliverAfter && o.redelivered < maxRedeliveries && now.Sub(o.redeliveredAt) >= redeliverAfter {
		if err := p.redeliver(ctx, db, api, id, watches, &o, queued, now); err != nil {
			return err
		}
	}
	if age >= inFlightAfter && o.reactedAt == 0 && o.confirmedAt == 0 {
		p.markInFlight(ctx, api, &o, now)
	}
	return p.holds.updateOwnerMessage(ctx, o)
}

func (p *poller) unansweredInThread(ctx context.Context, api slackAPI, id identity, o ownerMessage) (bool, error) {
	thread := cmp.Or(o.message.ThreadTimestamp, o.ts)
	msgs, err := p.cache.replies(ctx, api, o.channelID, thread)
	if err != nil {
		return true, fmt.Errorf("read thread %s/%s for an answer to %s: %w", o.channelID, thread, o.ts, err)
	}
	if by, ok := answeredInThread(id, msgs, o.ts); ok {
		p.log.Printf("owner message %s/%s: answered by %s in its thread", o.channelID, o.ts, by)
		return false, p.owed.answer(ctx, o.ts)
	}
	return true, nil
}

var progressReactions = map[string]bool{pickupReaction: true, workingReaction: true, inFlightReaction: true}

func driveReaction(id identity, m slack.Message) (string, bool) {
	for _, r := range m.Reactions {
		if !progressReactions[r.Name] && slices.Contains(r.Users, id.BotUserID) {
			return r.Name, true
		}
	}
	return "", false
}

func answeredInThread(id identity, thread []slack.Message, ts string) (string, bool) {
	for _, m := range thread {
		if m.Timestamp == ts {
			if name, ok := driveReaction(id, m); ok {
				return ":" + name + ":", true
			}
			continue
		}
		if tsLess(ts, m.Timestamp) && id.fromClaude(m) && !typedOnDashboard(m) && m.Metadata.EventType != compactionAckEventType {
			return m.Timestamp, true
		}
	}
	return "", false
}

func (p *poller) settleOwnerMessage(ctx context.Context, api slackAPI, o ownerMessage, owed bool) error {
	if o.reactedAt != 0 {
		if err := api.RemoveReactionContext(ctx, inFlightReaction, slack.NewRefToMessage(o.channelID, o.ts)); err != nil && errorCode(err) != "no_reaction" {
			p.log.Printf("owner message %s/%s: take back %s: %v", o.channelID, o.ts, inFlightReaction, err)
		}
	}
	if owed {
		p.log.Printf("owner message %s/%s: still unanswered after %s; no longer tracking it", o.channelID, o.ts, ownerMessageTTL)
	} else {
		p.handled(ctx, api, o)
	}
	return p.holds.settleOwnerMessage(ctx, o)
}

func (p *poller) confirm(o *ownerMessage, now time.Time) (queued bool) {
	if o.confirmedAt != 0 || o.transcript == "" {
		return false
	}
	scan, err := transcriptMark{path: o.transcript, offset: o.transcriptAt}.scan(o.ts)
	if err != nil {
		p.log.Printf("owner message %s/%s: read the transcript %s: %v", o.channelID, o.ts, o.transcript, err)
	}
	o.transcriptAt = scan.next
	if scan.seen {
		o.confirmedAt = now.UnixMilli()
		p.log.Printf("owner message %s/%s: in the transcript of subject %s %s after it was picked up", o.channelID, o.ts, o.holder, now.Sub(o.pickedAt).Round(time.Second))
	}
	return scan.queued
}

func (p *poller) markInFlight(ctx context.Context, api slackAPI, o *ownerMessage, now time.Time) {
	switch err := api.AddReactionContext(ctx, inFlightReaction, slack.NewRefToMessage(o.channelID, o.ts)); {
	case err == nil || errorCode(err) == "already_reacted":
		o.reactedAt = now.UnixMilli()
		p.log.Printf("owner message %s/%s: unanswered %s after it was picked up, so it shows %s", o.channelID, o.ts, now.Sub(o.pickedAt).Round(time.Second), inFlightReaction)
	default:
		p.log.Printf("owner message %s/%s: show %s: %v", o.channelID, o.ts, inFlightReaction, err)
	}
}

func (p *poller) redeliver(ctx context.Context, db *sql.DB, api slackAPI, id identity, watches []watch, o *ownerMessage, queued bool, now time.Time) error {
	h := holderFor(watches, id, o.channelID)
	if h.ID == 0 {
		p.log.Printf("owner message %s/%s: unconfirmed %s after pickup and no session holds its conversation, so it waits for one", o.channelID, o.ts, now.Sub(o.pickedAt).Round(time.Second))
		return nil
	}
	if queued && h.SubjectID == o.holder && p.alive(h.PID) {
		return nil
	}
	if p.stillCompacting(ctx, h.SubjectID) {
		return nil
	}
	pending, err := untakenMessage(ctx, db, h.SubjectID, o.ts)
	if err != nil || pending {
		return err
	}
	if h.SubjectID != o.holder || o.transcript == "" {
		mark, err := sessionTranscript(ctx, db, h.SubjectID)
		if err != nil {
			p.log.Printf("owner message %s/%s: redeliver to subject %s with no transcript to confirm it on: %v", o.channelID, o.ts, h.SubjectID, err)
		}
		o.holder, o.transcript, o.transcriptAt = h.SubjectID, mark.path, mark.offset
	}
	takers, err := messageTakers(ctx, db, h.SubjectID, o.ts)
	if err != nil {
		return err
	}
	w := h
	if w.ChannelID != o.channelID {
		w.ChannelID, w.Relayed = o.channelID, true
	}
	mates, err := loadDriveMates(ctx, p.threads, dmHolderIn(watches, id.DMChannelID).SubjectID, []string{h.SubjectID})
	if err != nil {
		return err
	}
	d, err := p.messageDelivery(ctx, api, id, w, o.message, mates.heldBy(h), addressedToAgents)
	if err != nil {
		return err
	}
	o.redelivered++
	o.redeliveredAt = now
	d.dedup = fmt.Sprintf("slack-redelivered:%s:%s:%d", o.channelID, o.ts, o.redelivered)
	d.line = redeliveredLine(now.Sub(o.pickedAt), takers) + d.line
	d.payload = markedRedelivered(d.payload, o.redelivered, takers)
	if err := p.deliverDirect(ctx, w, d); err != nil {
		return err
	}
	p.log.Printf("owner message %s/%s: redelivered to subject %s (try %d): not in its transcript and unanswered %s after pickup; taken by %d other sessions",
		o.channelID, o.ts, h.SubjectID, o.redelivered, now.Sub(o.pickedAt).Round(time.Second), len(takers))
	return nil
}

func markedRedelivered(payload func(typ string) []byte, try int, takers []string) func(typ string) []byte {
	return func(typ string) []byte {
		var msg messagePayload
		if err := json.Unmarshal(payload(typ), &msg); err != nil {
			panic(err)
		}
		msg.Redelivered, msg.TakenBy = try, takers
		return mustJSON(msg)
	}
}

func redeliveredLine(age time.Duration, takers []string) string {
	took := "no session took it"
	if len(takers) > 0 {
		took = "session " + strings.Join(takers, ", ") + " took it"
	}
	return fmt.Sprintf("cc-slack delivers this again: it is not in this session's transcript and nobody has answered it %s after it arrived (%s). ", age.Round(time.Second), took)
}

func (p *poller) deliverDirect(ctx context.Context, w watch, d delivery) error {
	if w.Owner != agent.TopLevel {
		return fmt.Errorf("redeliver %s to agent %s: only a session's top level holds a conversation", d.dedup, w.Owner)
	}
	seq, err := p.appendEvent(ctx, w.SubjectID, d.origin, d.base, d.payload(d.base), d.dedup)
	if err != nil {
		return err
	}
	if _, err := p.plane.DirectEvent(ctx, w.SubjectID, d.origin, d.line, seq); err != nil {
		return fmt.Errorf("direct redelivery: %w", err)
	}
	return nil
}

func untakenMessage(ctx context.Context, db *sql.DB, subjectID, ts string) (bool, error) {
	var pending bool
	err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM directives d JOIN events e ON e.subject_id = d.subject_id AND e.seq = d.event_seq
WHERE d.subject_id = ? AND d.delivered_at IS NULL AND json_extract(e.payload, '$.ts') = ?)`, subjectID, ts).Scan(&pending)
	if err != nil {
		return false, fmt.Errorf("find an untaken delivery of %s: %w", ts, err)
	}
	return pending, nil
}

func messageTakers(ctx context.Context, db *sql.DB, except, ts string) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT DISTINCT COALESCE(s.session_id, s.id) FROM events e JOIN subjects s ON s.id = e.subject_id
WHERE e.subject_id <> ? AND e.type LIKE ? AND json_extract(e.payload, '$.ts') = ?`, except, eventMessage+"%", ts)
	if err != nil {
		return nil, fmt.Errorf("find who took %s: %w", ts, err)
	}
	defer func() { _ = rows.Close() }()
	var takers []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		takers = append(takers, cmp.Or(s, "?"))
	}
	return takers, rows.Err()
}

func (p *poller) handled(ctx context.Context, api slackAPI, o ownerMessage) {
	m, err := messageAt(ctx, api, o.channelID, cmp.Or(o.message.ThreadTimestamp, o.ts), o.ts)
	if err != nil {
		p.log.Printf("owner message %s/%s: read its reactions: %v", o.channelID, o.ts, err)
		return
	}
	if slices.ContainsFunc(m.Reactions, func(r slack.ItemReaction) bool { return r.Name == workingReaction }) {
		return
	}
	ref := slack.NewRefToMessage(o.channelID, o.ts)
	if err := api.RemoveReactionContext(ctx, pickupReaction, ref); err != nil && errorCode(err) != "no_reaction" {
		p.log.Printf("owner message %s/%s: take off %s: %v", o.channelID, o.ts, pickupReaction, err)
	}
	if err := api.AddReactionContext(ctx, doneReaction, ref); err != nil && errorCode(err) != "already_reacted" {
		p.log.Printf("owner message %s/%s: show %s: %v", o.channelID, o.ts, doneReaction, err)
	}
}
