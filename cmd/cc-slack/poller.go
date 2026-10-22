package main

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/slack-go/slack"

	"github.com/yasyf/cc-interact/agent"
	"github.com/yasyf/cc-interact/event"
)

const (
	minBackoff     = 10 * time.Second
	maxBackoff     = 60 * time.Second
	jitterFraction = 0.2
	errorRetry     = 10 * time.Second

	methodReplies  = "conversations.replies"
	methodHistory  = "conversations.history"
	nudgeEventType = "cc_slack_message"
)

var routedSubtypes = map[string]bool{
	"":                 true,
	"bot_message":      true,
	"thread_broadcast": true,
	"file_share":       true,
	"me_message":       true,
}

type plane interface {
	DB() *sql.DB
	Append(ctx context.Context, e *event.Event) (int64, error)
	Direct(ctx context.Context, subjectID, agentID, origin, text string) (agent.Directive, error)
}

type poller struct {
	plane     plane
	newAPI    func(token string) slackAPI
	alive     func(pid int) bool
	now       func() time.Time
	cache     *readCache
	replay    *replayCursor
	shortcuts *shortcutHandler
	cards     *liveCards
	wakeCh    chan struct{}
	reconcile atomic.Bool
	paused    map[string]time.Time
	log       *log.Logger

	mu      sync.Mutex
	marks   uint64
	pending map[cursorKey]uint64
	since   map[cursorKey]string
}

type messageNudge struct {
	Channel  string `json:"channel"`
	TS       string `json:"ts"`
	ThreadTS string `json:"thread_ts"`
	Subtype  string `json:"subtype"`
	Seq      string `json:"seq"`
}

type watch struct {
	ID        int64
	SubjectID string
	Owner     string
	Kind      string
	ChannelID string
	ThreadTS  string
}

type cursorKey struct {
	channelID string
	threadTS  string
}

type cursorGroup struct {
	key      cursorKey
	root     bool
	since    string
	watches  []watch
	sessions []watch
	covering []watch
}

type cursorRow struct {
	oldest     string
	nextPollAt int64
	backoffS   int64
}

func (p *poller) wake() {
	select {
	case p.wakeCh <- struct{}{}:
	default:
	}
}

func (p *poller) reconcileAll() {
	p.reconcile.Store(true)
	p.wake()
}

func (p *poller) nudge(keys ...cursorKey) {
	p.mark(keys...)
	p.wake()
}

func (p *poller) mark(keys ...cursorKey) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.markLocked(keys...)
}

func (p *poller) markLocked(keys ...cursorKey) {
	for _, key := range keys {
		p.marks++
		p.pending[key] = p.marks
	}
}

func (p *poller) nudged(n messageNudge) {
	if !routedSubtypes[n.Subtype] {
		return
	}
	p.mu.Lock()
	threaded := n.ThreadTS != "" && n.ThreadTS != n.TS
	if threaded {
		key := cursorKey{n.Channel, n.ThreadTS}
		if floor, ok := p.since[key]; !ok || tsLess(justBefore(n.TS), floor) {
			p.since[key] = justBefore(n.TS)
		}
		p.markLocked(key)
	}
	if !threaded || n.Subtype == "thread_broadcast" {
		p.markLocked(cursorKey{n.Channel, ""})
	}
	p.mu.Unlock()
	p.wake()
}

func (p *poller) consumed(key cursorKey, floor string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.since[key] == floor {
		delete(p.since, key)
	}
}

func (p *poller) isPending(key cursorKey) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.pending[key]
	return ok
}

func (p *poller) settle(key cursorKey) {
	p.mu.Lock()
	delete(p.pending, key)
	p.mu.Unlock()
}

func (p *poller) pendingMarks() (map[cursorKey]uint64, map[cursorKey]string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return maps.Clone(p.pending), maps.Clone(p.since)
}

func (p *poller) keepWatched(groups []cursorGroup, seen map[cursorKey]uint64, all bool) {
	watched := make(map[cursorKey]bool, len(groups))
	for _, g := range groups {
		watched[g.key] = true
	}
	p.mu.Lock()
	for key, mark := range seen {
		if !watched[key] && p.pending[key] == mark {
			delete(p.pending, key)
			delete(p.since, key)
		}
	}
	p.mu.Unlock()
	if all {
		p.mark(slices.Collect(maps.Keys(watched))...)
	}
}

func (p *poller) run(ctx context.Context) {
	for {
		next, err := p.tick(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			p.log.Printf("poll: %v", err)
			next = p.now().Add(errorRetry)
		}
		var (
			timer *time.Timer
			due   <-chan time.Time
		)
		if !next.IsZero() {
			timer = time.NewTimer(max(next.Sub(p.now()), 0))
			due = timer.C
		}
		select {
		case <-ctx.Done():
		case <-p.wakeCh:
		case <-due:
		}
		if timer != nil {
			timer.Stop()
		}
		if ctx.Err() != nil {
			return
		}
	}
}

func (p *poller) tick(ctx context.Context) (time.Time, error) {
	db := p.plane.DB()
	id, err := loadIdentity(ctx, db)
	if errors.Is(err, errNoIdentity) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	seen, since := p.pendingMarks()
	watches, err := p.liveWatches(ctx, db)
	if err != nil {
		return time.Time{}, err
	}
	groups := groupWatches(watches, id.DMChannelID, since)
	p.keepWatched(groups, seen, p.reconcile.Swap(false))
	api := p.newAPI(id.Token)
	now := p.now()
	var next time.Time
	for _, g := range groups {
		if !p.isPending(g.key) {
			continue
		}
		c, err := groupCursor(ctx, db, g)
		if err != nil {
			return time.Time{}, err
		}
		if due := p.dueAt(g.key, c); due.After(now) {
			next = earliest(next, due)
			continue
		}
		p.settle(g.key)
		c, err = p.poll(ctx, db, api, id, g, c)
		if err != nil {
			p.mark(g.key)
			return time.Time{}, err
		}
		if p.isPending(g.key) {
			next = earliest(next, p.dueAt(g.key, c))
		}
	}
	seq := p.replay.latest()
	if !p.drained() {
		return next, nil
	}
	return next, p.replay.save(seq)
}

func (p *poller) drained() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.pending) == 0 && !p.reconcile.Load()
}

func (p *poller) dueAt(key cursorKey, c cursorRow) time.Time {
	due := time.UnixMilli(c.nextPollAt)
	if until := p.paused[methodFor(key)]; until.After(due) {
		return until
	}
	return due
}

func earliest(a, b time.Time) time.Time {
	if a.IsZero() || b.Before(a) {
		return b
	}
	return a
}

func methodFor(key cursorKey) string {
	if key.threadTS != "" {
		return methodReplies
	}
	return methodHistory
}

func (p *poller) liveWatches(ctx context.Context, db *sql.DB) ([]watch, error) {
	rows, err := db.QueryContext(ctx, `SELECT w.id, w.subject_id, w.owner_agent_id, w.kind, w.channel_id, w.thread_ts, s.claude_pid
		FROM watches w JOIN subjects s ON s.id = w.subject_id WHERE w.status = 'active' ORDER BY w.id`)
	if err != nil {
		return nil, fmt.Errorf("load watches: %w", err)
	}
	var live, dead []watch
	for rows.Next() {
		var (
			w   watch
			pid int
		)
		if err := rows.Scan(&w.ID, &w.SubjectID, &w.Owner, &w.Kind, &w.ChannelID, &w.ThreadTS, &pid); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan watch: %w", err)
		}
		if pid == 0 || !p.alive(pid) {
			dead = append(dead, w)
			continue
		}
		live = append(live, w)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close watches: %w", err)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate watches: %w", err)
	}
	for _, w := range dead {
		if _, err := db.ExecContext(ctx, `UPDATE watches SET status = 'orphaned' WHERE id = ?`, w.ID); err != nil {
			return nil, fmt.Errorf("orphan watch %d: %w", w.ID, err)
		}
	}
	return live, nil
}

func groupWatches(watches []watch, dmChannelID string, since map[cursorKey]string) []cursorGroup {
	var groups []cursorGroup
	index := map[cursorKey]int{}
	group := func(key cursorKey) *cursorGroup {
		i, ok := index[key]
		if !ok {
			i = len(groups)
			index[key] = i
			groups = append(groups, cursorGroup{key: key})
		}
		return &groups[i]
	}
	channels := map[string][]watch{}
	for _, w := range watches {
		g := group(cursorKey{w.ChannelID, w.ThreadTS})
		g.watches = append(g.watches, w)
		if w.Kind == kindChannel {
			channels[w.ChannelID] = append(channels[w.ChannelID], w)
		}
	}
	for _, key := range slices.SortedFunc(maps.Keys(since), compareKeys) {
		_, own := index[key]
		if !own && len(channels[key.channelID]) == 0 {
			continue
		}
		g := group(key)
		g.since = since[key]
		for _, w := range channels[key.channelID] {
			if !slices.ContainsFunc(g.watches, func(o watch) bool { return o.SubjectID == w.SubjectID && o.Owner == w.Owner }) {
				g.covering = append(g.covering, w)
			}
		}
	}
	root := group(cursorKey{dmChannelID, ""})
	root.root = true
	for _, w := range watches {
		if w.Kind == kindDM {
			root.sessions = append(root.sessions, w)
		}
	}
	return groups
}

func compareKeys(a, b cursorKey) int {
	return cmp.Or(cmp.Compare(a.channelID, b.channelID), cmp.Compare(a.threadTS, b.threadTS))
}

func justBefore(ts string) string {
	sec, micro := splitTS(ts)
	if micro == 0 {
		return fmt.Sprintf("%d.%06d", sec-1, 999999)
	}
	return fmt.Sprintf("%d.%06d", sec, micro-1)
}

func groupCursor(ctx context.Context, db *sql.DB, g cursorGroup) (cursorRow, error) {
	if !g.covered() {
		return loadCursor(ctx, db, g.key)
	}
	if _, err := db.ExecContext(ctx, `INSERT OR IGNORE INTO cursors(channel_id, thread_ts, oldest_ts, next_poll_at) VALUES (?, ?, ?, 0)`,
		g.key.channelID, g.key.threadTS, g.since); err != nil {
		return cursorRow{}, fmt.Errorf("claim reply cursor %s/%s: %w", g.key.channelID, g.key.threadTS, err)
	}
	c, err := loadCursor(ctx, db, g.key)
	if err == nil && tsLess(c.oldest, g.since) {
		c.oldest = g.since
	}
	return c, err
}

func loadCursor(ctx context.Context, db *sql.DB, key cursorKey) (cursorRow, error) {
	var c cursorRow
	err := db.QueryRowContext(ctx, `SELECT oldest_ts, next_poll_at, backoff_s FROM cursors
		WHERE channel_id = ? AND thread_ts = ?`, key.channelID, key.threadTS).Scan(&c.oldest, &c.nextPollAt, &c.backoffS)
	if err != nil {
		return cursorRow{}, fmt.Errorf("load cursor %s/%s: %w", key.channelID, key.threadTS, err)
	}
	return c, nil
}

func saveCursor(ctx context.Context, db *sql.DB, key cursorKey, c cursorRow) error {
	_, err := db.ExecContext(ctx, `UPDATE cursors SET oldest_ts = ?, next_poll_at = ?, backoff_s = ?
		WHERE channel_id = ? AND thread_ts = ?`, c.oldest, c.nextPollAt, c.backoffS, key.channelID, key.threadTS)
	if err != nil {
		return fmt.Errorf("save cursor %s/%s: %w", key.channelID, key.threadTS, err)
	}
	return nil
}

func (p *poller) poll(ctx context.Context, db *sql.DB, api slackAPI, id identity, g cursorGroup, c cursorRow) (cursorRow, error) {
	var (
		msgs []slack.Message
		err  error
	)
	if g.key.threadTS != "" {
		observe := p.cacheObserver(ctx, g.key)
		msgs, err = fetchReplies(ctx, api, g.key.channelID, g.key.threadTS, c.oldest)
		observe(c.oldest, msgs, err)
	} else {
		msgs, err = fetchHistory(ctx, api, g.key.channelID, c.oldest, pageLimit)
	}
	now := p.now()
	if err != nil {
		return p.pollFailed(ctx, db, g, c, now, err)
	}
	latest := c.oldest
	for _, m := range msgs {
		if !tsLess(c.oldest, m.Timestamp) {
			continue
		}
		latest = m.Timestamp
		if g.root && isShortcutMessage(id, m) {
			if err := p.shortcuts.fromDM(ctx, id, m); err != nil {
				return c, err
			}
			continue
		}
		if g.idle() {
			continue
		}
		route, err := p.routable(ctx, db, g, m)
		if err != nil {
			return c, err
		}
		if !route {
			continue
		}
		if err := p.routeAll(ctx, db, api, id, slices.Concat(g.watches, g.covering), m); err != nil {
			return c, err
		}
		if len(g.sessions) > 0 && operatorRoot(id, m) {
			if err := p.routeRoot(ctx, db, api, id, g.sessions, m); err != nil {
				return c, err
			}
		}
	}
	c.oldest = latest
	c.backoffS = 0
	c.nextPollAt = now.UnixMilli()
	if err := saveCursor(ctx, db, g.key, c); err != nil {
		return c, err
	}
	p.consumed(g.key, g.since)
	return c, nil
}

func (p *poller) cacheObserver(ctx context.Context, key cursorKey) func(since string, msgs []slack.Message, fetchErr error) {
	gen, err := p.cache.generation(ctx, key.channelID, key.threadTS)
	return func(since string, msgs []slack.Message, fetchErr error) {
		if err == nil && fetchErr == nil {
			err = p.cache.observe(ctx, key.channelID, key.threadTS, since, gen, msgs)
		}
		if err != nil {
			p.log.Printf("cache %s/%s: %v", key.channelID, key.threadTS, err)
		}
	}
}

func (g cursorGroup) idle() bool {
	return len(g.watches) == 0 && len(g.sessions) == 0 && len(g.covering) == 0
}

func (g cursorGroup) covered() bool {
	return len(g.watches) == 0 && len(g.covering) > 0
}

func (p *poller) routeAll(ctx context.Context, db *sql.DB, api slackAPI, id identity, watches []watch, m slack.Message) error {
	for _, w := range watches {
		if w.Kind == kindDM && m.User != id.UserID {
			continue
		}
		if err := p.deliver(ctx, db, w, p.messageDelivery(ctx, api, id, w, m)); err != nil {
			return err
		}
	}
	return nil
}

func operatorRoot(id identity, m slack.Message) bool {
	return m.User == id.UserID && (m.ThreadTimestamp == "" || m.ThreadTimestamp == m.Timestamp)
}

func (p *poller) routable(ctx context.Context, db *sql.DB, g cursorGroup, m slack.Message) (bool, error) {
	if !routedSubtypes[m.SubType] || m.Timestamp == g.key.threadTS {
		return false, nil
	}
	own, err := ownPost(ctx, db, g.key.channelID, m.Timestamp)
	return !own, err
}

func (p *poller) pollFailed(ctx context.Context, db *sql.DB, g cursorGroup, c cursorRow, now time.Time, err error) (cursorRow, error) {
	var rate *slack.RateLimitedError
	switch {
	case errors.As(err, &rate):
		p.paused[methodFor(g.key)] = now.Add(rate.RetryAfter)
		c.backoffS = int64(min(maxBackoff, max(minBackoff, 2*time.Duration(c.backoffS)*time.Second)) / time.Second)
		c.nextPollAt = now.Add(max(rate.RetryAfter, time.Duration(c.backoffS)*time.Second)).UnixMilli()
	case fatalWatchCodes[errorCode(err)] && !g.root:
		p.consumed(g.key, g.since)
		if g.covered() {
			p.log.Printf("poll %s/%s for a channel watch: %v", g.key.channelID, g.key.threadTS, err)
			return c, nil
		}
		return c, p.failWatches(ctx, db, g, err)
	case fatalWatchCodes[errorCode(err)]:
		if err := p.failWatches(ctx, db, g, err); err != nil {
			return c, err
		}
		p.log.Printf("poll DM %s: %v", g.key.channelID, err)
		c.backoffS = int64(maxBackoff / time.Second)
		c.nextPollAt = now.Add(maxBackoff).UnixMilli()
	default:
		p.log.Printf("poll %s/%s: %v", g.key.channelID, g.key.threadTS, err)
		c.backoffS = int64(min(maxBackoff, max(minBackoff, 2*time.Duration(c.backoffS)*time.Second)) / time.Second)
		c.nextPollAt = now.Add(time.Duration(c.backoffS) * time.Second).UnixMilli()
	}
	p.mark(g.key)
	return c, saveCursor(ctx, db, g.key, c)
}

func (p *poller) failWatches(ctx context.Context, db *sql.DB, g cursorGroup, cause error) error {
	text := fallback(actionWatch, cause)
	for _, w := range append(g.watches, g.sessions...) {
		if _, err := db.ExecContext(ctx, `UPDATE watches SET status = 'error', error = ? WHERE id = ?`, text, w.ID); err != nil {
			return fmt.Errorf("fail watch %d: %w", w.ID, err)
		}
		if err := p.deliver(ctx, db, w, watchErrorDelivery(w, text)); err != nil {
			return err
		}
	}
	return nil
}
