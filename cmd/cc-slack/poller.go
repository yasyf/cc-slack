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
	spawnllm "github.com/yasyf/spawnllm/go"
)

const (
	minBackoff       = 10 * time.Second
	maxBackoff       = 60 * time.Second
	jitterFraction   = 0.2
	errorRetry       = 10 * time.Second
	resweepEvery     = 10 * time.Minute
	resweepIdleAfter = 24 * time.Hour

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
	DirectEvent(ctx context.Context, subjectID, origin, text string, eventSeq int64) (agent.Directive, error)
}

type poller struct {
	recard        func(ctx context.Context, id identity, card cardRef, pr refID)
	decide        func(ctx context.Context, state any, questions map[string]spawnllm.Question, opts spawnllm.DecideOpts) (spawnllm.Decision, error)
	keepUser      func(ctx context.Context, id identity) time.Time
	plane         plane
	newAPI        func(token string) slackAPI
	alive         func(pid int) bool
	now           func() time.Time
	cache         *readCache
	owed          *owedReplies
	holds         *deliveryHolds
	feeds         *feedStore
	threads       *dmThreads
	replay        *replayCursor
	shortcuts     *shortcutHandler
	cards         *liveCards
	taps          tapQueue
	applyingMu    sync.Mutex
	applying      map[string]bool
	wakeCh        chan struct{}
	reconcile     atomic.Bool
	reconcileFull atomic.Bool
	resweepAt     time.Time
	paused        map[string]time.Time
	resweptAt     map[cursorKey]time.Time
	log           *log.Logger

	mu        sync.Mutex
	marks     uint64
	pending   map[cursorKey]uint64
	since     map[cursorKey]string
	compact   map[string]transcriptMark
	swept     time.Time
	sweepOpen bool
	pushed    []pushedMessage
	taken     map[string]bool
	edits     map[string]bool
	left      []string
}

type messageNudge struct {
	Channel  string         `json:"channel"`
	TS       string         `json:"ts"`
	ThreadTS string         `json:"thread_ts"`
	Subtype  string         `json:"subtype"`
	Seq      string         `json:"seq"`
	Message  *slack.Message `json:"message,omitempty"`
}

type watch struct {
	ID        int64
	SubjectID string
	Owner     string
	Kind      string
	ChannelID string
	ThreadTS  string
	PID       int
	Relayed   bool
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
	mates    driveMates
	holder   watch
	owner    watch
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

func (p *poller) leave(subjectIDs ...string) {
	p.mu.Lock()
	p.left = append(p.left, subjectIDs...)
	p.mu.Unlock()
	p.wake()
}

func (p *poller) takeLeft() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	left := p.left
	p.left = nil
	return left
}

func (p *poller) reconcileAll() {
	p.reconcileFull.Store(true)
	p.reconcile.Store(true)
	p.wake()
}

func (p *poller) resweep() {
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
	if n.Message != nil && n.Message.Timestamp == n.TS && routedSubtypes[n.Subtype] {
		p.mu.Lock()
		p.pushed = append(p.pushed, pushedMessage{key: nudgedKey(n), m: *n.Message})
		p.mu.Unlock()
	}
	if n.Subtype == editedSubtype {
		p.edited(n.Channel)
		p.wake()
		return
	}
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

func (p *poller) keepWatched(groups []cursorGroup, seen map[cursorKey]uint64) {
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
}

func (p *poller) reconcileOperators(ctx context.Context, db *sql.DB, api slackAPI, id identity, watches []watch, full bool) error {
	for _, channelID := range id.operatorChannelIDs() {
		if _, err := db.ExecContext(ctx, `INSERT OR IGNORE INTO cursors(channel_id, thread_ts, oldest_ts, next_poll_at) VALUES (?, '', ?, 0)`,
			channelID, formatTS(p.now())); err != nil {
			return fmt.Errorf("claim the cursor of %s: %w", channelID, err)
		}
		if err := p.markReconciled(ctx, db, api, channelID, watches, full); err != nil {
			return err
		}
	}
	return nil
}

func (p *poller) markReconciled(ctx context.Context, db *sql.DB, api slackAPI, dmChannelID string, watches []watch, full bool) error {
	dmDue, err := p.resweepDue(ctx, db, cursorKey{dmChannelID, ""}, full)
	if err != nil {
		return err
	}
	if dmDue {
		p.mark(cursorKey{dmChannelID, ""})
	}
	running, err := p.feeds.running(ctx)
	if err != nil {
		return err
	}
	p.edited(slices.Collect(maps.Keys(running))...)
	threads := map[string]bool{}
	var subscribers []string
	for _, w := range watches {
		switch {
		case w.Kind == kindDM:
			subscribers = append(subscribers, w.SubjectID)
		case w.ChannelID == dmChannelID && w.ThreadTS != "":
			threads[w.ThreadTS] = true
		default:
			key := cursorKey{w.ChannelID, w.ThreadTS}
			due, err := p.resweepDue(ctx, db, key, full)
			if err != nil {
				return err
			}
			if due {
				p.mark(key)
			}
		}
	}
	if !dmDue {
		return nil
	}
	return p.markDMThreads(ctx, db, api, dmChannelID, threads, subscribers)
}

func (p *poller) markDMThreads(ctx context.Context, db *sql.DB, api slackAPI, dmChannelID string, threads map[string]bool, subscribers []string) error {
	bound, err := p.threads.sessionThreads(ctx, dmChannelID, subscribers...)
	if err != nil {
		return err
	}
	oldest := ""
	for _, thread := range bound {
		threads[thread] = true
	}
	for thread := range threads {
		if oldest == "" || tsLess(thread, oldest) {
			oldest = thread
		}
	}
	if oldest == "" {
		return nil
	}
	msgs, err := fetchHistory(ctx, api, dmChannelID, justBefore(oldest), pageLimit)
	if err != nil {
		return fmt.Errorf("reconcile DM %s: %w", dmChannelID, err)
	}
	for _, m := range msgs {
		if !threads[m.Timestamp] || m.LatestReply == "" {
			continue
		}
		key := cursorKey{dmChannelID, m.Timestamp}
		c, err := loadCursor(ctx, db, key)
		if err != nil {
			return err
		}
		if tsLess(c.oldest, m.LatestReply) {
			p.mark(key)
		}
	}
	return nil
}

func (p *poller) run(ctx context.Context) {
	ctx = withSource(ctx, "poller")
	defer slackCalls.flush(time.Now)
	if err := p.loadCompactions(ctx); err != nil {
		p.log.Printf("load the sessions compacting when the daemon stopped: %v", err)
	}
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
	watches, takeDue, err := p.liveWatches(ctx, db, id)
	if err != nil {
		return time.Time{}, err
	}
	holder := dmHolderIn(watches, id.DMChannelID)
	mates, err := loadDriveMates(ctx, p.threads, holder.SubjectID, slices.Concat(dmSubjects(watches), heldSubjects(channelHolders(watches, id))))
	if err != nil {
		return time.Time{}, err
	}
	api := p.newAPI(id.Token)
	if now := p.now(); now.Sub(p.resweepAt) >= resweepEvery {
		p.resweepAt = now
		p.reconcile.Store(true)
	}
	if p.reconcile.Swap(false) {
		full := p.reconcileFull.Swap(false)
		if err := p.reconcileOperators(ctx, db, api, id, watches, full); err != nil {
			p.reconcile.Store(true)
			p.reconcileFull.Store(full)
			return time.Time{}, err
		}
	}
	seen, since := p.pendingMarks()
	bound, err := p.threads.bound(ctx, id.DMChannelID, dmThreadKeys(id.DMChannelID, seen))
	if err != nil {
		return time.Time{}, err
	}
	groups := groupWatches(watches, id, since, bound, mates)
	p.keepWatched(groups, seen)
	settled, done := drainable(ctx, drainGrace)
	defer done()
	p.takeAllPushed(settled, db, api, id, groups)
	next, err := p.pollGroups(ctx, settled, db, api, id, groups)
	if err != nil {
		return next, err
	}
	if err := ctx.Err(); err != nil {
		return next, err
	}
	due, err := p.ackHeld(settled, db, api, id)
	if err != nil {
		p.log.Printf("ack held messages: %v", err)
	}
	next = earliest(next, due)
	if due, err = p.sweepOwnerMessages(settled, db, api, id, watches); err != nil {
		p.log.Printf("sweep owner messages: %v", err)
	}
	next = earliest(next, due)
	next = earliest(next, takeDue)
	if due := p.retryTaps(ctx, id); !due.IsZero() {
		next = earliest(next, due)
	}
	if due := p.keepUser(ctx, id); !due.IsZero() {
		next = earliest(next, due)
	}
	due, err = p.feedTick(ctx, db, api, id, watches)
	if err != nil {
		return time.Time{}, err
	}
	if !due.IsZero() {
		next = earliest(next, due)
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
	if b.IsZero() {
		return a
	}
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

func (p *poller) liveWatches(ctx context.Context, db *sql.DB, id identity) ([]watch, time.Time, error) {
	rows, err := db.QueryContext(ctx, `SELECT w.id, w.subject_id, w.owner_agent_id, w.kind, w.channel_id, w.thread_ts, s.claude_pid
		FROM watches w JOIN subjects s ON s.id = w.subject_id WHERE w.status = 'active' ORDER BY w.id`)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("load watches: %w", err)
	}
	var live, dead []watch
	for rows.Next() {
		var w watch
		if err := rows.Scan(&w.ID, &w.SubjectID, &w.Owner, &w.Kind, &w.ChannelID, &w.ThreadTS, &w.PID); err != nil {
			_ = rows.Close()
			return nil, time.Time{}, fmt.Errorf("scan watch: %w", err)
		}
		if w.PID == 0 || !p.alive(w.PID) {
			dead = append(dead, w)
			continue
		}
		live = append(live, w)
	}
	if err := rows.Close(); err != nil {
		return nil, time.Time{}, fmt.Errorf("close watches: %w", err)
	}
	if err := rows.Err(); err != nil {
		return nil, time.Time{}, fmt.Errorf("iterate watches: %w", err)
	}
	for _, w := range dead {
		if _, err := db.ExecContext(ctx, `UPDATE watches SET status = 'orphaned' WHERE id = ?`, w.ID); err != nil {
			return nil, time.Time{}, fmt.Errorf("orphan watch %d: %w", w.ID, err)
		}
	}
	holder := dmHolderIn(live, id.DMChannelID)
	untaken, err := loadUntaken(ctx, db, dmSubjects(live))
	if err != nil {
		return nil, time.Time{}, err
	}
	now := p.now()
	var (
		deaf []string
		due  time.Time
	)
	for subjectID, since := range untaken {
		switch {
		case subjectID == holder.SubjectID:
		case untaken.deaf(subjectID, now):
			deaf = append(deaf, subjectID)
		default:
			due = earliest(due, since.Add(ackWait))
		}
	}
	live = slices.DeleteFunc(live, func(w watch) bool { return w.Kind == kindDM && slices.Contains(deaf, w.SubjectID) })
	return live, due, p.handBack(ctx, db, id, live, slices.Concat(p.takeLeft(), dmSubjects(dead), deaf))
}

type untaken map[string]time.Time

func loadUntaken(ctx context.Context, db *sql.DB, subjectIDs []string) (untaken, error) {
	rows, err := db.QueryContext(ctx, `SELECT subject_id, MIN(created_at) FROM directives
		WHERE subject_id IN (SELECT value FROM json_each(?)) AND agent_id = ? AND delivered_at IS NULL GROUP BY subject_id`,
		string(mustJSON(subjectIDs)), agent.TopLevel)
	if err != nil {
		return nil, fmt.Errorf("find the messages %d sessions have not taken: %w", len(subjectIDs), err)
	}
	defer func() { _ = rows.Close() }()
	u := untaken{}
	for rows.Next() {
		var (
			subjectID string
			created   int64
		)
		if err := rows.Scan(&subjectID, &created); err != nil {
			return nil, fmt.Errorf("scan an untaken message: %w", err)
		}
		u[subjectID] = time.Unix(created, 0)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate untaken messages: %w", err)
	}
	return u, nil
}

func (u untaken) deaf(subjectID string, now time.Time) bool {
	since, ok := u[subjectID]
	return ok && !now.Before(since.Add(ackWait))
}

func dmThreadKeys(dmChannelID string, pending map[cursorKey]uint64) []string {
	var threads []string
	for key := range pending {
		if key.channelID == dmChannelID && key.threadTS != "" {
			threads = append(threads, key.threadTS)
		}
	}
	return threads
}

func groupWatches(watches []watch, id identity, since map[cursorKey]string, bound map[string][]string, mates driveMates) []cursorGroup {
	var groups []cursorGroup
	dmChannelID, operators := id.DMChannelID, id.operatorChannelIDs()
	holder, held := dmHolderIn(watches, dmChannelID), channelHolders(watches, id)
	index := map[cursorKey]int{}
	group := func(key cursorKey) *cursorGroup {
		i, ok := index[key]
		if !ok {
			i = len(groups)
			index[key] = i
			groups = append(groups, cursorGroup{key: key, mates: mates.heldBy(held[key.channelID]), holder: holder, owner: held[key.channelID]})
		}
		return &groups[i]
	}
	channels := map[string][]watch{}
	sessions, subscriptions := dmSubscribers(watches, dmChannelID)
	for _, w := range watches {
		if w.Kind == kindDM {
			continue
		}
		g := group(cursorKey{w.ChannelID, w.ThreadTS})
		g.watches = append(g.watches, w)
		if w.Kind == kindChannel {
			channels[w.ChannelID] = append(channels[w.ChannelID], w)
		}
	}
	for _, thread := range slices.Sorted(maps.Keys(bound)) {
		for _, subjectID := range bound[thread] {
			w, live := subscriptions[subjectID]
			if !live {
				continue
			}
			w.ThreadTS = thread
			g := group(cursorKey{dmChannelID, thread})
			g.watches = append(g.watches, w)
		}
	}
	for i := range groups {
		inThread := slices.Clone(groups[i].watches)
		groups[i].watches = slices.DeleteFunc(groups[i].watches, func(w watch) bool { return mates.leaves(w, inThread) })
	}
	for _, key := range slices.SortedFunc(maps.Keys(since), compareKeys) {
		_, own := index[key]
		if !own && len(channels[key.channelID]) == 0 && !slices.Contains(operators, key.channelID) {
			continue
		}
		g := group(key)
		g.since = since[key]
		for _, w := range channels[key.channelID] {
			if !slices.ContainsFunc(g.watches, func(o watch) bool { return o.SubjectID == w.SubjectID && o.Owner == w.Owner }) && !mates.leaves(w, g.watches) {
				g.covering = append(g.covering, w)
			}
		}
	}
	for _, channelID := range operators {
		group(cursorKey{channelID, ""}).root = true
	}
	group(cursorKey{dmChannelID, ""}).sessions = sessions
	return groups
}

func dmHolderIn(watches []watch, dmChannelID string) watch {
	i := slices.IndexFunc(watches, func(w watch) bool { return w.Kind == kindChannel && w.ChannelID == dmChannelID })
	if i < 0 {
		return watch{}
	}
	return watches[i]
}

func dmSubjects(watches []watch) []string {
	var subjects []string
	for _, w := range watches {
		if w.Kind == kindDM {
			subjects = append(subjects, w.SubjectID)
		}
	}
	return subjects
}

func dmSubscribers(watches []watch, dmChannelID string) ([]watch, map[string]watch) {
	var sessions []watch
	subscriptions := map[string]watch{}
	for _, w := range watches {
		if _, dup := subscriptions[w.SubjectID]; w.Kind == kindDM && w.ChannelID == dmChannelID && !dup {
			subscriptions[w.SubjectID] = w
			sessions = append(sessions, w)
		}
	}
	return sessions, subscriptions
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
	operator := slices.Contains(id.operatorChannelIDs(), g.key.channelID)
	if g.key.threadTS != "" {
		observe := p.cacheObserver(ctx, g.key)
		msgs, err = fetchReplies(withoutRateWait(ctx), api, g.key.channelID, g.key.threadTS, c.oldest)
		observe(c.oldest, msgs, err)
		_, limited := errors.AsType[*slack.RateLimitedError](err)
		p.cache.budget.pollerWaits(g.key, operator && limited)
	} else {
		msgs, err = fetchHistory(ctx, api, g.key.channelID, c.oldest, pageLimit)
	}
	now := p.now()
	if err != nil {
		return p.pollFailed(ctx, db, g, c, now, err, operator)
	}
	latest := c.oldest
	for _, m := range msgs {
		if !tsLess(c.oldest, m.Timestamp) {
			continue
		}
		latest = m.Timestamp
		if p.tookPushed(g.key.channelID, m.Timestamp) {
			continue
		}
		if err := p.take(ctx, db, api, id, g, m); err != nil {
			return c, err
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

func (p *poller) take(ctx context.Context, db *sql.DB, api slackAPI, id identity, g cursorGroup, m slack.Message) error {
	if g.key.channelID == id.DMChannelID {
		m = dashboardWords(id, m)
	}
	if g.root && isShortcutMessage(id, m) {
		return p.shortcuts.fromDM(ctx, id, m)
	}
	if g.key.channelID == id.DMChannelID {
		tapped, err := p.dmMessage(ctx, api, id, g.key.threadTS, m)
		if err != nil || tapped {
			return err
		}
		if err := p.recordOwnerMessage(ctx, db, id, g, m); err != nil {
			p.log.Printf("route %s/%s with no delivery record: %v", g.key.channelID, m.Timestamp, err)
		}
	} else {
		if id.fromOperator(m) && routedSubtypes[m.SubType] {
			p.laneHeard(ctx, id, g.key.channelID, g.key.threadTS, m.Timestamp, m.Text)
		}
		if err := p.trackOwed(ctx, id, g.key.channelID, g.key.threadTS, m); err != nil {
			return err
		}
	}
	if g.idle() {
		return nil
	}
	return p.route(ctx, db, api, id, g, m)
}

func (p *poller) route(ctx context.Context, db *sql.DB, api slackAPI, id identity, g cursorGroup, m slack.Message) error {
	route, err := p.routable(ctx, db, g, m)
	if err != nil || !route {
		return err
	}
	to, why, err := p.addressee(ctx, db, api, id, g.key.channelID, m)
	if err != nil {
		return err
	}
	if to == addressedUnclear {
		p.log.Printf("%s/%s goes to the DM holder to judge: %s", g.key.channelID, m.Timestamp, why)
	}
	watching := slices.Concat(g.watches, g.covering)
	if g.key.channelID != id.DMChannelID && to != addressedToPeople {
		watching = g.toHolder(watching)
	} else if to != addressedToPeople {
		watching = g.toChannelHolder(watching)
	}
	rooted := len(g.sessions) > 0 && operatorRoot(id, m) && !g.held()
	reaching := watching
	if rooted && len(g.sessions) == 1 {
		reaching = append(slices.Clone(watching), g.sessions[0])
	}
	arrived := false
	if g.key.channelID == id.DMChannelID && id.fromOperator(m) {
		if arrived, err = p.newToAny(ctx, db, reaching, m); err != nil {
			return err
		}
	}
	if arrived {
		p.holdAck(ctx, db, g.key.channelID, reaching, m)
	}
	if err := p.routeAll(ctx, db, api, id, watching, m, g.mates, to); err != nil {
		return err
	}
	if rooted {
		if err := p.routeRoot(ctx, db, api, id, g.sessions, m, g.mates); err != nil {
			return err
		}
	}
	if arrived {
		p.pickUp(ctx, api, id, m)
	}
	return nil
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
	return len(g.watches) == 0 && g.since != ""
}

func (g cursorGroup) held() bool {
	return slices.ContainsFunc(g.watches, func(w watch) bool { return w.Kind == kindChannel })
}

func (g cursorGroup) toHolder(watching []watch) []watch {
	if g.holder.ID == 0 {
		return watching
	}
	kept := slices.DeleteFunc(slices.Clone(watching), func(w watch) bool {
		return w.Kind == kindChannel && w.SubjectID != g.holder.SubjectID && w.PID != g.holder.PID
	})
	if len(kept) > 0 || len(watching) == 0 {
		return kept
	}
	relay := g.holder
	relay.ChannelID, relay.Relayed = g.key.channelID, true
	return []watch{relay}
}

func (p *poller) routeAll(ctx context.Context, db *sql.DB, api slackAPI, id identity, watches []watch, m slack.Message, mates driveMates, to addressee) error {
	fed, err := p.feeds.fed(ctx, watches)
	if err != nil {
		return err
	}
	if err := p.routeFeed(ctx, db, api, id, fed, m); err != nil {
		return err
	}
	for _, w := range watches {
		if slices.ContainsFunc(fed, func(fw fedWatch) bool { return fw.ID == w.ID }) {
			continue
		}
		if to == addressedToPeople && (w.Kind == kindDM || (w.Kind == kindChannel && w.ChannelID == id.DMChannelID)) {
			continue
		}
		d, err := p.messageDelivery(ctx, api, id, w, m, mates, to)
		if err != nil {
			return err
		}
		if err := p.deliver(ctx, db, w, d); err != nil {
			return err
		}
	}
	return nil
}

func (p *poller) dmMessage(ctx context.Context, api slackAPI, id identity, polledThread string, m slack.Message) (bool, error) {
	if tap, ok := p.numberedTap(id, m); ok {
		p.eyes(ctx, api, id, m)
		return true, p.cardAction(ctx, id, tap)
	}
	return false, p.trackOwed(ctx, id, id.DMChannelID, polledThread, m)
}

func (p *poller) trackOwed(ctx context.Context, id identity, channelID, polledThread string, m slack.Message) error {
	if !id.fromOperator(m) || !routedSubtypes[m.SubType] || m.Timestamp == polledThread {
		return nil
	}
	thread := cmp.Or(m.ThreadTimestamp, m.Timestamp)
	return p.owed.owe(ctx, owedReply{ThreadTS: thread, TS: m.Timestamp, Text: messageText(m), Permalink: id.permalink(channelID, m.Timestamp, thread)})
}

func (p *poller) newToAny(ctx context.Context, db *sql.DB, watches []watch, m slack.Message) (bool, error) {
	for _, w := range watches {
		seen, err := delivered(ctx, db, w.SubjectID, messageDedup(w, m.Timestamp))
		if err != nil || !seen {
			return err == nil, err
		}
	}
	return false, nil
}

func (p *poller) pickUp(ctx context.Context, api slackAPI, id identity, m slack.Message) {
	p.eyes(ctx, api, id, m)
	thread := cmp.Or(m.ThreadTimestamp, m.Timestamp)
	if line := setWorkingLine(ctx, api, id, id.DMChannelID, thread, workingStatus, arrivalLoading); line != "" {
		p.log.Printf("pick up %s: working line under thread %s: %s", m.Timestamp, thread, line)
	}
}

func (p *poller) eyes(ctx context.Context, api slackAPI, id identity, m slack.Message) {
	switch err := api.AddReactionContext(ctx, pickupReaction, slack.ItemRef{Channel: id.DMChannelID, Timestamp: m.Timestamp}); {
	case err == nil:
		sec, micro := splitTS(m.Timestamp)
		p.log.Printf("pick up %s: eyes on %.1fs after it was sent", m.Timestamp, p.now().Sub(time.Unix(sec, micro*1000)).Seconds())
	case errorCode(err) != "already_reacted":
		p.log.Printf("pick up %s: eyes: %v", m.Timestamp, err)
	}
}

func operatorRoot(id identity, m slack.Message) bool {
	return id.fromOperator(m) && (m.ThreadTimestamp == "" || m.ThreadTimestamp == m.Timestamp)
}

func (p *poller) routable(ctx context.Context, db *sql.DB, g cursorGroup, m slack.Message) (bool, error) {
	if !routedSubtypes[m.SubType] || m.Timestamp == g.key.threadTS {
		return false, nil
	}
	own, err := ownPost(ctx, db, g.key.channelID, m.Timestamp)
	return !own, err
}

func (p *poller) pollFailed(ctx context.Context, db *sql.DB, g cursorGroup, c cursorRow, now time.Time, err error, operator bool) (cursorRow, error) {
	var rate *slack.RateLimitedError
	switch {
	case errors.As(err, &rate) && operator:
		c.backoffS = 0
		c.nextPollAt = now.Add(rate.RetryAfter).UnixMilli()
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
		if w.Kind == kindDM && !g.root {
			continue
		}
		if _, err := db.ExecContext(ctx, `UPDATE watches SET status = 'error', error = ? WHERE id = ? AND kind <> 'dm'`, text, w.ID); err != nil {
			return fmt.Errorf("fail watch %d: %w", w.ID, err)
		}
		if err := p.deliver(ctx, db, w, watchErrorDelivery(w, text)); err != nil {
			return err
		}
	}
	return nil
}

func holderIn(watches []watch, dmChannelID, channelID string) watch {
	dm := dmHolderIn(watches, dmChannelID)
	if channelID == dmChannelID {
		return dm
	}
	if held := channelHolderIn(watches, dm, channelID); held.ID != 0 {
		return held
	}
	return dmHolderIn(watches, channelID)
}

func channelHolderIn(watches []watch, dm watch, channelID string) watch {
	for _, w := range slices.Backward(watches) {
		if w.Kind == kindChannel && w.ChannelID == channelID && w.SubjectID != dm.SubjectID && (dm.ID == 0 || w.PID != dm.PID) {
			return w
		}
	}
	return watch{}
}

func channelHolders(watches []watch, id identity) map[string]watch {
	dm := dmHolderIn(watches, id.DMChannelID)
	held := map[string]watch{}
	for _, c := range id.OperatorChannels {
		if w := channelHolderIn(watches, dm, c.ID); w.ID != 0 {
			held[c.ID] = w
		}
	}
	return held
}

func heldSubjects(held map[string]watch) []string {
	subjects := make([]string, 0, len(held))
	for _, w := range held {
		subjects = append(subjects, w.SubjectID)
	}
	return subjects
}

func (g cursorGroup) toChannelHolder(watching []watch) []watch {
	if g.owner.ID == 0 {
		return watching
	}
	return slices.DeleteFunc(slices.Clone(watching), func(w watch) bool { return w.Kind == kindChannel && w.ID != g.owner.ID })
}

func (p *poller) pollGroups(ctx, settled context.Context, db *sql.DB, api slackAPI, id identity, groups []cursorGroup) (time.Time, error) {
	now := p.now()
	var next time.Time
	for _, g := range groups {
		if err := ctx.Err(); err != nil {
			return next, err
		}
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
		c, err = p.poll(settled, db, api, id.at(g.key.channelID), g, c)
		if err != nil {
			p.mark(g.key)
			return time.Time{}, err
		}
		if p.isPending(g.key) {
			next = earliest(next, p.dueAt(g.key, c))
		}
	}
	return next, nil
}

func (p *poller) resweepDue(ctx context.Context, db *sql.DB, key cursorKey, full bool) (bool, error) {
	now := p.now()
	if p.resweptAt == nil {
		p.resweptAt = map[cursorKey]time.Time{}
	}
	if full {
		p.resweptAt[key] = now
		return true, nil
	}
	if at, ok := p.resweptAt[key]; ok && now.Sub(at) < resweepEvery {
		return false, nil
	}
	c, err := loadCursor(ctx, db, key)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	latest := c.oldest
	if latest == "" {
		latest = key.threadTS
	}
	if latest != "" && now.Sub(tsTime(latest)) > resweepIdleAfter {
		return false, nil
	}
	p.resweptAt[key] = now
	return true, nil
}
