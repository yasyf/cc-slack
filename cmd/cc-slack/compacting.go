package main

import (
	"bytes"
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/slack-go/slack"
	"github.com/spf13/cobra"

	"github.com/yasyf/cc-interact/agent"
	"github.com/yasyf/cc-interact/cmd"
	"github.com/yasyf/cc-interact/daemon"
	"github.com/yasyf/cc-interact/event"
)

const opPreCompact daemon.Op = "pre-compact"

const (
	compactionAck          = "Got it, back in a minute."
	compactionAckEventType = "cc_slack_compaction_ack"
	compactionAckWait      = 8 * time.Second
)

const transcriptTailChunk = 64 << 10

var (
	compactBoundary = []byte(`"subtype":"compact_boundary"`)
	assistantRecord = []byte(`"type":"assistant"`)
)

type preCompactBody struct {
	Transcript string `json:"transcript"`
	Trigger    string `json:"trigger"`
	Context    int64  `json:"context"`
	Point      int64  `json:"point"`
}

type transcriptTail struct {
	model   string
	context int64
	point   int64
}

type preCompactResult struct {
	Compacting string `json:"compacting"`
}

type transcriptMark struct {
	path   string
	offset int64
}

type heldAck struct {
	m       slack.Message
	due     time.Time
	watches []watch
	marks   []transcriptMark
}

type ackedPayload struct {
	Type      string `json:"type"`
	WatchID   int64  `json:"watch_id"`
	ChannelID string `json:"channel_id"`
	ThreadTS  string `json:"thread_ts"`
	TS        string `json:"ts"`
	Acked     string `json:"acked"`
}

func (p *poller) startCompacting(subjectID string, mark transcriptMark) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.compact[subjectID] = mark
}

func (p *poller) endCompacting(subjectID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, was := p.compact[subjectID]
	delete(p.compact, subjectID)
	return was
}

func (p *poller) settleCompacting(subjectID string, mark transcriptMark) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.compact[subjectID] == mark {
		delete(p.compact, subjectID)
	}
}

func (p *poller) compacting(watches []watch) []watch {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.DeleteFunc(slices.Clone(watches), func(w watch) bool {
		_, on := p.compact[w.SubjectID]
		return w.Owner != agent.TopLevel || !on
	})
}

func (p *poller) holdAck(ctx context.Context, db *sql.DB, watches []watch, m slack.Message) {
	compacting := p.compacting(watches)
	fresh, err := p.newToAny(ctx, db, compacting, m)
	if err != nil {
		p.log.Printf("hold an ack for %s while a compaction runs: %v", m.Timestamp, err)
	}
	if !fresh {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	h := heldAck{m: m, due: p.now().Add(compactionAckWait), watches: compacting}
	for _, w := range compacting {
		h.marks = append(h.marks, p.compact[w.SubjectID])
	}
	p.held = append(p.held, h)
}

func markTranscript(path string) (transcriptMark, error) {
	if path == "" {
		return transcriptMark{}, errors.New("its PreCompact hook named no transcript")
	}
	info, err := os.Stat(path)
	if err != nil {
		return transcriptMark{}, err
	}
	return transcriptMark{path: path, offset: info.Size()}, nil
}

func (p *poller) ackHeld(ctx context.Context, db *sql.DB, api slackAPI, id identity) time.Time {
	now := p.now()
	var (
		due  []heldAck
		next time.Time
	)
	p.mu.Lock()
	p.held = slices.DeleteFunc(p.held, func(h heldAck) bool {
		if h.due.After(now) {
			next = earliest(next, h.due)
			return false
		}
		due = append(due, h)
		return true
	})
	p.mu.Unlock()
	for _, h := range due {
		p.ackUnseen(ctx, db, api, id, h)
	}
	return next
}

func (p *poller) ackUnseen(ctx context.Context, db *sql.DB, api slackAPI, id identity, h heldAck) {
	for i, mark := range h.marks {
		subject := h.watches[i].SubjectID
		written, err := mark.written()
		if err != nil {
			p.log.Printf("no ack for %s: read the transcript of session subject %s: %v", h.m.Timestamp, subject, err)
			return
		}
		if bytes.Contains(written, compactBoundary) {
			p.settleCompacting(subject, mark)
			p.log.Printf("no ack for %s: session subject %s finished compacting", h.m.Timestamp, subject)
			return
		}
		if bytes.Contains(written, []byte(h.m.Timestamp)) {
			p.log.Printf("no ack for %s: it is in the transcript of session subject %s", h.m.Timestamp, subject)
			return
		}
	}
	marked := slack.MsgOptionMetadata(slack.SlackMetadata{EventType: compactionAckEventType, EventPayload: map[string]any{"user_id": id.UserID}})
	_, ts, err := postMessage(ctx, api, db, p.log, id, id.DMChannelID, cmp.Or(h.m.ThreadTimestamp, h.m.Timestamp), compactionAck, nil, nil, marked)
	if err != nil {
		p.log.Printf("ack %s: %v", h.m.Timestamp, err)
		return
	}
	p.log.Printf("acked %s with %s: it was not in its session's transcript %s after it arrived while the session compacted", h.m.Timestamp, ts, compactionAckWait)
	for _, w := range h.watches {
		if err := p.deliver(ctx, db, w, ackedDelivery(w, h.m, ts)); err != nil {
			p.log.Printf("tell subject %s that %s is acked: %v", w.SubjectID, h.m.Timestamp, err)
		}
	}
}

func (t transcriptMark) written() ([]byte, error) {
	f, err := os.Open(t.path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return io.ReadAll(io.NewSectionReader(f, t.offset, math.MaxInt64-t.offset))
}

func ackedDelivery(w watch, m slack.Message, acked string) delivery {
	msg := ackedPayload{WatchID: w.ID, ChannelID: w.ChannelID, ThreadTS: cmp.Or(m.ThreadTimestamp, m.Timestamp), TS: m.Timestamp, Acked: acked}
	return delivery{
		base:   eventAcked,
		origin: event.OriginSystem,
		dedup:  fmt.Sprintf("slack-acked:%s:%s:%s", w.Owner, w.ChannelID, m.Timestamp),
		line: fmt.Sprintf("cc-slack acked your operator's message ts %s in its thread (ts %s), since this session had not picked it up %s after it arrived, so do not ack it again.",
			m.Timestamp, acked, compactionAckWait),
		payload: func(typ string) []byte {
			msg.Type = typ
			return mustJSON(msg)
		},
	}
}

func compactionAckPost(m slack.Message) bool {
	return m.Metadata.EventType == compactionAckEventType
}

func (a *app) handlePreCompact(hc daemon.HandlerCtx) daemon.Reply {
	var b preCompactBody
	if len(hc.Env.Body) > 0 {
		if err := json.Unmarshal(hc.Env.Body, &b); err != nil {
			return fail(fmt.Sprintf("bad %s body: %v", hc.Env.Op, err))
		}
	}
	sub, found, err := hc.Subjects.Find(hc.Ctx, hc.Window, hc.Scope)
	if err != nil {
		return fail(err.Error())
	}
	if !found {
		return ok(preCompactResult{})
	}
	if b.Trigger != "manual" && b.Context*10 < b.Point*9 {
		a.poller.log.Printf("session %s PreCompact at %d of %d tokens: a background agent compacts, no ack", hc.Env.Session, b.Context, b.Point)
		return ok(preCompactResult{})
	}
	mark, err := markTranscript(b.Transcript)
	if err != nil {
		a.poller.log.Printf("session %s PreCompact at %d of %d tokens (%s), with no transcript to hold an ack on: %v", hc.Env.Session, b.Context, b.Point, b.Trigger, err)
		return ok(preCompactResult{})
	}
	a.poller.startCompacting(sub.ID, mark)
	a.poller.log.Printf("session %s is compacting at %d of %d tokens (%s): until a compact_boundary record lands in its transcript, an operator DM message not in it %s after it arrives gets an ack", hc.Env.Session, b.Context, b.Point, b.Trigger, compactionAckWait)
	return ok(preCompactResult{Compacting: sub.ID})
}

func preCompactCmd(d cmd.Deps) *cobra.Command {
	return &cobra.Command{
		Use:    "pre-compact",
		Hidden: true,
		Short:  "PreCompact hook: when the session's transcript stands at its compaction point, the daemon acks each operator DM message not in it 8 seconds after it arrived, until the transcript records the compaction",
		Args:   cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			in, err := readHookInput(c)
			if err != nil {
				return err
			}
			body := preCompactBody{Transcript: in.TranscriptPath, Trigger: in.Trigger}
			if tail, err := readTranscriptTail(in.TranscriptPath); err == nil {
				body.Context, body.Point = tail.context, compactionPoint(tail, os.Getenv)
			}
			_, err = invoke(c.Context(), d, in.SessionID, cwdOr(in.Cwd), opPreCompact, mustJSON(body))
			return err
		},
	}
}

func readTranscriptTail(path string) (transcriptTail, error) {
	var tail transcriptTail
	f, err := os.Open(path)
	if err != nil {
		return tail, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return tail, err
	}
	var carry []byte
	for end := info.Size(); end > 0; {
		start := max(0, end-transcriptTailChunk)
		buf := make([]byte, end-start, end-start+int64(len(carry)))
		if _, err := f.ReadAt(buf, start); err != nil {
			return tail, err
		}
		fresh := len(buf)
		buf = append(buf, carry...)
		for {
			i := bytes.LastIndexByte(buf[:fresh], '\n')
			if i < 0 {
				break
			}
			if tail.read(buf[i+1:]) {
				return tail, nil
			}
			buf, fresh = buf[:i], i
		}
		carry, end = buf, start
	}
	tail.read(carry)
	return tail, nil
}

func (t *transcriptTail) read(line []byte) bool {
	if bytes.Contains(line, compactBoundary) {
		var r struct {
			Type     string `json:"type"`
			Subtype  string `json:"subtype"`
			Metadata struct {
				Trigger   string `json:"trigger"`
				PreTokens int64  `json:"preTokens"`
			} `json:"compactMetadata"`
		}
		if json.Unmarshal(line, &r) != nil || r.Type != "system" || r.Subtype != "compact_boundary" {
			return false
		}
		if r.Metadata.Trigger == "auto" {
			t.point = r.Metadata.PreTokens
		}
		return true
	}
	if t.model != "" || !bytes.Contains(line, assistantRecord) {
		return false
	}
	var r struct {
		Type        string `json:"type"`
		IsSidechain bool   `json:"isSidechain"`
		Message     struct {
			Model string `json:"model"`
			Usage struct {
				Input         int64 `json:"input_tokens"`
				CacheCreation int64 `json:"cache_creation_input_tokens"`
				CacheRead     int64 `json:"cache_read_input_tokens"`
			} `json:"usage"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &r) != nil || r.Type != "assistant" || r.IsSidechain {
		return false
	}
	if u := r.Message.Usage; u.Input+u.CacheCreation+u.CacheRead > 0 {
		t.model, t.context = r.Message.Model, u.Input+u.CacheCreation+u.CacheRead
	}
	return false
}

func compactionPoint(tail transcriptTail, getenv func(string) string) int64 {
	window := autoCompactWindow(tail.model, getenv)
	if tail.point == 0 || window == 0 {
		return max(tail.point, window)
	}
	return min(tail.point, window)
}

func autoCompactWindow(model string, getenv func(string) string) int64 {
	if w, ok := parseCompactWindow(getenv("CLAUDE_CODE_AUTO_COMPACT_WINDOW")); ok {
		return w
	}
	dir := getenv("CLAUDE_CONFIG_DIR")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return 0
		}
		dir = filepath.Join(home, ".claude")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		return 0
	}
	var s struct {
		AutoCompactWindow json.RawMessage `json:"autoCompactWindow"`
		ModelSettings     map[string]struct {
			AutoCompactWindow json.RawMessage `json:"autoCompactWindow"`
		} `json:"modelSettings"`
	}
	if json.Unmarshal(raw, &s) != nil {
		return 0
	}
	setting := s.AutoCompactWindow
	if m := s.ModelSettings[model].AutoCompactWindow; m != nil {
		setting = m
	}
	var window int64
	if json.Unmarshal(setting, &window) != nil {
		return 0
	}
	return window
}

func parseCompactWindow(v string) (int64, bool) {
	v = strings.TrimSpace(v)
	scale := 1.0
	if v != "" {
		switch v[len(v)-1] {
		case 'k', 'K':
			scale, v = 1e3, v[:len(v)-1]
		case 'm', 'M':
			scale, v = 1e6, v[:len(v)-1]
		}
	}
	n, err := strconv.ParseFloat(v, 64)
	if err != nil || !(n*scale >= 1) {
		return 0, false
	}
	return int64(math.Round(min(max(n*scale, 1e5), 1e6))), true
}
