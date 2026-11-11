package main

import (
	"bytes"
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"slices"
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

type preCompactBody struct {
	Transcript string `json:"transcript"`
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

func (p *poller) startCompacting(subjectID, transcript string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.compact[subjectID] = transcript
}

func (p *poller) endCompacting(subjectID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, was := p.compact[subjectID]
	delete(p.compact, subjectID)
	return was
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
		h.marks = append(h.marks, p.markTranscript(w.SubjectID, p.compact[w.SubjectID]))
	}
	p.held = append(p.held, h)
}

func (p *poller) markTranscript(subjectID, path string) transcriptMark {
	if path == "" {
		return transcriptMark{}
	}
	info, err := os.Stat(path)
	if err != nil {
		p.log.Printf("read the transcript of session subject %s: %v", subjectID, err)
		return transcriptMark{}
	}
	return transcriptMark{path: path, offset: info.Size()}
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
		logged, err := mark.holds(h.m.Timestamp)
		if err != nil {
			p.log.Printf("read the transcript of session subject %s for %s: %v", h.watches[i].SubjectID, h.m.Timestamp, err)
			continue
		}
		if logged {
			p.log.Printf("no ack for %s: it is in the transcript of session subject %s", h.m.Timestamp, h.watches[i].SubjectID)
			return
		}
	}
	marked := slack.MsgOptionMetadata(slack.SlackMetadata{EventType: compactionAckEventType, EventPayload: map[string]any{"user_id": id.UserID}})
	_, ts, err := postMessage(ctx, api, db, p.log, id, id.DMChannelID, cmp.Or(h.m.ThreadTimestamp, h.m.Timestamp), compactionAck, nil, nil, marked)
	if err != nil {
		p.log.Printf("ack %s: %v", h.m.Timestamp, err)
		return
	}
	p.log.Printf("acked %s with %s: it was not in its session's transcript %s after it arrived while the session or one of its agents compacted", h.m.Timestamp, ts, compactionAckWait)
	for _, w := range h.watches {
		if err := p.deliver(ctx, db, w, ackedDelivery(w, h.m, ts)); err != nil {
			p.log.Printf("tell subject %s that %s is acked: %v", w.SubjectID, h.m.Timestamp, err)
		}
	}
}

func (t transcriptMark) holds(ts string) (bool, error) {
	if t.path == "" {
		return false, nil
	}
	f, err := os.Open(t.path)
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	written, err := io.ReadAll(io.NewSectionReader(f, t.offset, math.MaxInt64-t.offset))
	if err != nil {
		return false, err
	}
	return bytes.Contains(written, []byte(ts)), nil
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
	sub, found, err := hc.Subjects.Find(hc.Ctx, hc.Window, hc.Scope)
	if err != nil {
		return fail(err.Error())
	}
	if !found {
		return ok(preCompactResult{})
	}
	var b preCompactBody
	if err := json.Unmarshal(hc.Env.Body, &b); err != nil {
		return fail(fmt.Sprintf("bad %s body: %v", hc.Env.Op, err))
	}
	a.poller.startCompacting(sub.ID, b.Transcript)
	a.poller.log.Printf("session %s or one of its agents is compacting: an operator DM message not in its transcript %s after it arrives gets an ack", hc.Env.Session, compactionAckWait)
	return ok(preCompactResult{Compacting: sub.ID})
}

func preCompactCmd(d cmd.Deps) *cobra.Command {
	return &cobra.Command{
		Use:    "pre-compact",
		Hidden: true,
		Short:  "PreCompact hook: until this window starts again, the daemon acks each operator DM message not in the session's transcript 8 seconds after it arrived",
		Args:   cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			in, err := readHookInput(c)
			if err != nil {
				return err
			}
			_, err = invoke(c.Context(), d, in.SessionID, cwdOr(in.Cwd), opPreCompact, mustJSON(preCompactBody{Transcript: in.TranscriptPath}))
			return err
		},
	}
}
