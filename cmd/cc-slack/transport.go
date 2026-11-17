package main

import (
	"bufio"
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/slack-go/slack"
	"github.com/yasyf/cc-interact/store"
)

const (
	tapEventType      = "cc_slack_tap"
	reactionEventType = "cc_slack_reaction"
	deletedSubtype    = "message_deleted"
	transportKeep     = 3 * time.Hour
)

var transportSchema = store.Schema{DDL: `
CREATE TABLE transport_events (
  seq   INTEGER PRIMARY KEY AUTOINCREMENT,
  id    TEXT NOT NULL UNIQUE,
  body  TEXT NOT NULL,
  at    INTEGER NOT NULL,
  acked INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE transport_keys (
  key        TEXT PRIMARY KEY,
  channel_id TEXT NOT NULL,
  ts         TEXT NOT NULL,
  thread_ts  TEXT NOT NULL
);
`}

type transportConfig struct {
	Subscriber         bool `toml:"subscriber" json:"subscriber,omitempty"`
	NoHeardReaction    bool `toml:"no_heard_reaction" json:"no_heard_reaction,omitempty"`
	NoInFlightReaction bool `toml:"no_in_flight_reaction" json:"no_in_flight_reaction,omitempty"`
}

type transportEvent struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	User     string `json:"user"`
	Channel  string `json:"channel"`
	TS       string `json:"ts"`
	ThreadTS string `json:"thread_ts,omitempty"`
	Link     string `json:"link"`
	Text     string `json:"text,omitempty"`
	Previous string `json:"previous,omitempty"`
	Reaction string `json:"reaction,omitempty"`
	Added    bool   `json:"added,omitempty"`
	ActionID string `json:"action_id,omitempty"`
	Value    string `json:"value,omitempty"`
	ActionTS string `json:"action_ts,omitempty"`
}

type reactionPush struct {
	User     string `json:"user"`
	Reaction string `json:"reaction"`
	Channel  string `json:"channel"`
	TS       string `json:"ts"`
	ItemUser string `json:"item_user"`
	Added    bool   `json:"added"`
	EventTS  string `json:"event_ts"`
	Seq      string `json:"seq"`
}

type transport struct {
	st  *store.Store
	now func() time.Time
	log *log.Logger

	mu   sync.Mutex
	wake chan struct{}
	drop context.CancelFunc
}

func transportPath() string { return filepath.Join(appPaths().StateDir(), "transport.db") }

func transportSocket() string { return filepath.Join(appPaths().StateDir(), "transport.sock") }

func openTransport(ctx context.Context, path string) (*transport, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("transport dir: %w", err)
	}
	st, err := store.Open(ctx, path, transportSchema, store.WithUnsupportedSchema(store.ArchiveUnsupportedSchema))
	if err != nil {
		return nil, fmt.Errorf("open transport %s: %w", path, err)
	}
	return &transport{st: st, now: time.Now, log: log.New(os.Stderr, "["+binaryName+" transport] ", log.LstdFlags)}, nil
}

func (t *transport) Close() error { return t.st.Close() }

func (t *transport) keep(ctx context.Context, id identity, e transportEvent) error {
	if !id.Config.Transport.Subscriber || e.User != id.UserID {
		return nil
	}
	body, err := json.Marshal(e)
	if err != nil {
		return err
	}
	now := t.now()
	db := t.st.DB()
	if _, err := db.ExecContext(ctx, `DELETE FROM transport_events WHERE acked = 1 AND at < ?`, now.Add(-transportKeep).UnixMilli()); err != nil {
		return fmt.Errorf("transport: forget acked events: %w", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT OR IGNORE INTO transport_events(id, body, at) VALUES (?, ?, ?)`, e.ID, string(body), now.UnixMilli()); err != nil {
		return fmt.Errorf("transport: keep %s: %w", e.ID, err)
	}
	t.mu.Lock()
	if t.wake != nil {
		select {
		case t.wake <- struct{}{}:
		default:
		}
	}
	t.mu.Unlock()
	return nil
}

func (t *transport) ack(ctx context.Context, eventID string) error {
	_, err := t.st.DB().ExecContext(ctx, `UPDATE transport_events SET acked = 1 WHERE id = ?`, eventID)
	return err
}

func (t *transport) unacked(ctx context.Context, after int64) ([]int64, []string, error) {
	rows, err := t.st.DB().QueryContext(ctx, `SELECT seq, body FROM transport_events WHERE acked = 0 AND seq > ? ORDER BY seq`, after)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = rows.Close() }()
	var (
		seqs   []int64
		bodies []string
	)
	for rows.Next() {
		var (
			seq  int64
			body string
		)
		if err := rows.Scan(&seq, &body); err != nil {
			return nil, nil, err
		}
		seqs, bodies = append(seqs, seq), append(bodies, body)
	}
	return seqs, bodies, rows.Err()
}

func (t *transport) subscribe(ctx context.Context, conn net.Conn, in *bufio.Reader) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	wake := make(chan struct{}, 1)
	t.mu.Lock()
	if t.drop != nil {
		t.drop()
	}
	t.drop, t.wake = cancel, wake
	t.mu.Unlock()
	defer func() {
		t.mu.Lock()
		if t.wake == wake {
			t.drop, t.wake = nil, nil
		}
		t.mu.Unlock()
	}()
	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()
	go func() {
		defer cancel()
		for {
			line, err := in.ReadBytes('\n')
			if err != nil {
				return
			}
			var a struct {
				Ack string `json:"ack"`
			}
			if err := json.Unmarshal(line, &a); err != nil || a.Ack == "" {
				t.log.Printf("subscriber sent %q, not an ack; dropping it", clip(string(line)))
				return
			}
			if err := t.ack(ctx, a.Ack); err != nil {
				t.log.Printf("ack %s: %v", a.Ack, err)
				return
			}
		}
	}()
	if _, err := conn.Write([]byte(`{"ok":true}` + "\n")); err != nil {
		return err
	}
	var after int64
	for {
		seqs, bodies, err := t.unacked(ctx, after)
		if err != nil {
			return err
		}
		for i, body := range bodies {
			if _, err := conn.Write([]byte(body + "\n")); err != nil {
				return err
			}
			after = seqs[i]
		}
		select {
		case <-ctx.Done():
			return nil
		case <-wake:
		}
	}
}

func (t *transport) pushed(ctx context.Context, id identity, push socketPush) (string, error) {
	if push.EventType == reactionEventType {
		var r reactionPush
		if err := json.Unmarshal(push.EventPayload, &r); err != nil {
			return "", err
		}
		return r.Seq, t.keep(ctx, id, reactionEvent(id, r))
	}
	var a cardActionPush
	if err := json.Unmarshal(push.EventPayload, &a); err != nil {
		return "", err
	}
	return a.Seq, t.keep(ctx, id, tapEvent(id, a))
}

func (e transportEvent) linked(id identity) transportEvent {
	e.Link = id.permalink(e.Channel, e.TS, e.ThreadTS)
	return e
}

func heardEvent(id identity, channelID, threadTS string, m slack.Message) transportEvent {
	return transportEvent{
		ID: "message:" + channelID + "/" + m.Timestamp, Kind: "message", User: m.User,
		Channel: channelID, TS: m.Timestamp, ThreadTS: threadOf(threadTS, m.Timestamp), Text: m.Text,
	}.linked(id)
}

func nudgeEvent(id identity, n messageNudge) (transportEvent, bool) {
	if n.Message == nil {
		return transportEvent{}, false
	}
	switch n.Subtype {
	case editedSubtype:
		now, was := n.Message.SubMessage, n.Message.PreviousMessage
		if now == nil || was != nil && was.Text == now.Text {
			return transportEvent{}, false
		}
		e := transportEvent{
			ID: "edit:" + n.Channel + "/" + now.Timestamp + "/" + n.TS, Kind: "edit", User: now.User,
			Channel: n.Channel, TS: now.Timestamp, ThreadTS: threadOf(now.ThreadTimestamp, now.Timestamp), Text: now.Text,
		}
		if was != nil {
			e.Previous = was.Text
		}
		return e.linked(id), true
	case deletedSubtype:
		was := n.Message.PreviousMessage
		if was == nil {
			return transportEvent{}, false
		}
		ts := cmp.Or(n.Message.DeletedTimestamp, was.Timestamp)
		return transportEvent{
			ID: "delete:" + n.Channel + "/" + ts, Kind: "delete", User: was.User,
			Channel: n.Channel, TS: ts, ThreadTS: threadOf(was.ThreadTimestamp, ts), Previous: was.Text,
		}.linked(id), true
	}
	return transportEvent{}, false
}

func reactionEvent(id identity, r reactionPush) transportEvent {
	sign := map[bool]string{true: "+", false: "-"}[r.Added]
	return transportEvent{
		ID: "reaction:" + r.Channel + "/" + r.TS + "/" + r.Reaction + "/" + sign + "/" + r.EventTS, Kind: "reaction",
		User: r.User, Channel: r.Channel, TS: r.TS, Reaction: r.Reaction, Added: r.Added,
	}.linked(id)
}

func tapEvent(id identity, a cardActionPush) transportEvent {
	return transportEvent{
		ID: "tap:" + a.Channel + "/" + a.ActionTS + "/" + a.ActionID, Kind: "tap", User: a.User,
		Channel: a.Channel, TS: a.MessageTS, ThreadTS: threadOf(a.ThreadTS, a.MessageTS),
		ActionID: a.ActionID, Value: a.Value, ActionTS: a.ActionTS,
	}.linked(id)
}

func threadOf(threadTS, ts string) string {
	if threadTS == ts {
		return ""
	}
	return threadTS
}

type transportRequest struct {
	Op       string            `json:"op"`
	URL      string            `json:"url"`
	Channel  string            `json:"channel"`
	ThreadTS string            `json:"thread_ts"`
	TS       string            `json:"ts"`
	Text     string            `json:"text"`
	Blocks   []json.RawMessage `json:"blocks"`
	Sender   string            `json:"sender"`
	Key      string            `json:"key"`
	DryRun   bool              `json:"dry_run"`
	Status   string            `json:"status"`
	Reaction string            `json:"reaction"`
	Remove   bool              `json:"remove"`
}

type transportReply struct {
	OK        bool            `json:"ok"`
	Error     string          `json:"error,omitempty"`
	Channel   string          `json:"channel,omitempty"`
	TS        string          `json:"ts,omitempty"`
	ThreadTS  string          `json:"thread_ts,omitempty"`
	Permalink string          `json:"permalink,omitempty"`
	Sender    string          `json:"sender,omitempty"`
	Text      string          `json:"text,omitempty"`
	Blocks    json.RawMessage `json:"blocks,omitempty"`
	DryRun    bool            `json:"dry_run,omitempty"`
	Replayed  bool            `json:"replayed,omitempty"`
}

func (a *app) serveTransport(ctx context.Context) {
	path := transportSocket()
	_ = os.Remove(path)
	ln, err := (&net.ListenConfig{}).Listen(ctx, "unix", path)
	if err != nil {
		a.transport.log.Printf("listen %s: %v", path, err)
		return
	}
	if err := os.Chmod(path, 0o600); err != nil {
		a.transport.log.Printf("chmod %s: %v", path, err)
	}
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() == nil {
				a.transport.log.Printf("accept: %v", err)
			}
			return
		}
		go a.transportConn(ctx, conn)
	}
}

func (a *app) transportConn(ctx context.Context, conn net.Conn) {
	defer func() { _ = conn.Close() }()
	in := bufio.NewReader(conn)
	line, err := in.ReadBytes('\n')
	if err != nil {
		return
	}
	var req transportRequest
	reply := func(r transportReply) {
		body, _ := json.Marshal(r)
		_, _ = conn.Write(append(body, '\n'))
	}
	if err := json.Unmarshal(line, &req); err != nil {
		reply(transportReply{Error: "a transport request is one JSON object with an op: " + err.Error()})
		return
	}
	id, err := loadIdentity(ctx, a.poller.plane.DB())
	if err != nil {
		reply(transportReply{Error: err.Error()})
		return
	}
	if req.Op == "subscribe" {
		if !id.Config.Transport.Subscriber {
			reply(transportReply{Error: "[transport] subscriber is off in this login's config, so nothing is kept for a subscriber"})
			return
		}
		if err := a.transport.subscribe(ctx, conn, in); err != nil && ctx.Err() == nil {
			a.transport.log.Printf("subscriber: %v", err)
		}
		return
	}
	r, err := a.transportOp(ctx, id, req)
	if err != nil {
		reply(transportReply{Error: err.Error()})
		return
	}
	r.OK = true
	reply(r)
}

func (a *app) transportOp(ctx context.Context, id identity, req transportRequest) (transportReply, error) {
	if req.URL != "" {
		ref, err := parsePermalink(req.URL)
		if err != nil {
			return transportReply{}, err
		}
		req.Channel, req.TS, req.ThreadTS = ref.channelID, cmp.Or(req.TS, ref.ts), cmp.Or(req.ThreadTS, ref.threadTS)
	}
	api := a.newAPI(id.Token)
	channelID, err := conversationID(ctx, api, id, req.Channel)
	if err != nil {
		return transportReply{}, err
	}
	target := transportReply{Channel: channelID, ThreadTS: req.ThreadTS, TS: req.TS, DryRun: req.DryRun}
	switch req.Op {
	case "send", "reply":
		if req.Op == "reply" && req.ThreadTS == "" {
			return transportReply{}, errors.New("reply needs thread_ts")
		}
		return a.transportPost(ctx, api, id, channelID, req, target)
	case "edit":
		if req.TS == "" {
			return transportReply{}, errors.New("edit needs ts")
		}
		text, blocks, err := transportMessage(ctx, id, req)
		if err != nil {
			return transportReply{}, err
		}
		if target, err = transportShown(target, text, blocks); err != nil || req.DryRun {
			return target, err
		}
		return target, changeMessage(ctx, api, channelID, req.TS, text, blocks, slack.MsgOptionDisableLinkUnfurl())
	case "delete":
		if req.TS == "" {
			return transportReply{}, errors.New("delete needs ts")
		}
		if req.DryRun {
			return target, nil
		}
		_, _, err := api.DeleteMessageContext(ctx, channelID, req.TS)
		return target, err
	case "status":
		return target, transportStatus(ctx, api, id, channelID, req)
	case "react":
		return target, transportReact(ctx, api, channelID, req)
	}
	return transportReply{}, fmt.Errorf("transport has no op %q: subscribe, send, reply, edit, delete, status or react", req.Op)
}

func (a *app) transportPost(ctx context.Context, api slackAPI, id identity, channelID string, req transportRequest, target transportReply) (transportReply, error) {
	text, blocks, err := transportMessage(ctx, id, req)
	if err != nil {
		return transportReply{}, err
	}
	target.Sender = cmp.Or(req.Sender, id.username())
	if target, err = transportShown(target, text, blocks); err != nil || req.DryRun {
		return target, err
	}
	db := a.transport.st.DB()
	if req.Key != "" {
		err := db.QueryRowContext(ctx, `SELECT channel_id, ts, thread_ts FROM transport_keys WHERE key = ?`, req.Key).Scan(&target.Channel, &target.TS, &target.ThreadTS)
		if err == nil {
			target.Replayed, target.Permalink = true, id.permalink(target.Channel, target.TS, target.ThreadTS)
			return target, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return transportReply{}, err
		}
	}
	opts := []slack.MsgOption{
		textOption(text), slack.MsgOptionUsername(target.Sender), slack.MsgOptionIconURL(id.AvatarURL),
		slack.MsgOptionDisableLinkUnfurl(), id.authorMetadata(),
	}
	if len(blocks) > 0 {
		opts = append(opts, slack.MsgOptionBlocks(blocks...))
	}
	if req.ThreadTS != "" {
		opts = append(opts, slack.MsgOptionTS(req.ThreadTS))
	}
	ch, ts, err := api.PostMessageContext(ctx, channelID, opts...)
	if err != nil {
		return transportReply{}, err
	}
	postRecorder(ctx, a.poller.plane.DB(), a.transport.log)(ch, ts)
	target.Channel, target.TS, target.ThreadTS = ch, ts, cmp.Or(req.ThreadTS, ts)
	target.Permalink = id.permalink(ch, ts, req.ThreadTS)
	if req.Key != "" {
		if _, err := db.ExecContext(ctx, `INSERT OR IGNORE INTO transport_keys(key, channel_id, ts, thread_ts) VALUES (?, ?, ?, ?)`, req.Key, ch, ts, target.ThreadTS); err != nil {
			return target, fmt.Errorf("posted %s but did not keep its key, so a resend with %q posts again: %w", target.Permalink, req.Key, err)
		}
	}
	return target, nil
}

func transportStatus(ctx context.Context, api slackAPI, id identity, channelID string, req transportRequest) error {
	if req.ThreadTS == "" {
		return errors.New("status needs thread_ts")
	}
	if req.DryRun {
		return nil
	}
	if channelID != id.dm() {
		return agentSessionStatus(ctx, api, channelID, req.ThreadTS, req.Status)
	}
	if failed := setWorkingLine(ctx, api, id, channelID, req.ThreadTS, req.Status, nil); failed != "" {
		return errors.New("Slack drew no loader: " + failed)
	}
	return nil
}

func transportReact(ctx context.Context, api slackAPI, channelID string, req transportRequest) error {
	if req.TS == "" || req.Reaction == "" {
		return errors.New("react needs ts and reaction")
	}
	if req.DryRun {
		return nil
	}
	apply, settled := slackAPI.AddReactionContext, "already_reacted"
	if req.Remove {
		apply, settled = slackAPI.RemoveReactionContext, "no_reaction"
	}
	if err := apply(api, ctx, strings.Trim(req.Reaction, ":"), slack.NewRefToMessage(channelID, req.TS)); err != nil && errorCode(err) != settled {
		return err
	}
	return nil
}

func transportMessage(ctx context.Context, id identity, req transportRequest) (string, []slack.Block, error) {
	if strings.TrimSpace(req.Text) == "" {
		return "", nil, errors.New("text is required: Slack shows it in notifications and wherever blocks cannot render")
	}
	raw := req.Blocks
	if !req.DryRun {
		var err error
		if raw, err = uploadedPictures(ctx, id, raw); err != nil {
			return "", nil, err
		}
	}
	blocks, err := parseBlocks(raw)
	if err != nil {
		return "", nil, err
	}
	text, blocks := fitText(req.Text, fitHeaders(blocks))
	return text, blocks, checkBlocks(blocks)
}

func uploadedPictures(ctx context.Context, id identity, raw []json.RawMessage) ([]json.RawMessage, error) {
	out := slices.Clone(raw)
	for i, block := range raw {
		var image map[string]any
		if err := json.Unmarshal(block, &image); err != nil {
			return nil, err
		}
		path, _ := image["image_url"].(string)
		if image["type"] != "image" || !filepath.IsAbs(path) {
			continue
		}
		uploaded, err := uploadPictures(ctx, id, []string{path})
		if err != nil {
			return nil, err
		}
		image["image_url"] = id.Config.Pictures.link(uploaded[0].Key)
		if out[i], err = json.Marshal(image); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func transportShown(target transportReply, text string, blocks []slack.Block) (transportReply, error) {
	target.Text = explicitLinks(text)
	if len(blocks) == 0 {
		return target, nil
	}
	raw, err := json.Marshal(blocks)
	target.Blocks = raw
	return target, err
}
