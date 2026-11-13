package main

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/slack-go/slack"

	"github.com/yasyf/cc-interact/store"
)

const (
	cacheFresh       = 15 * time.Second
	cacheFullRefresh = 10 * time.Minute
	cacheMaxBytes    = 64 << 20
	nameTTL          = 24 * time.Hour
)

var cacheSchema = store.Schema{DDL: `
CREATE TABLE threads (
  channel_id TEXT NOT NULL,
  thread_ts  TEXT NOT NULL,
  messages   BLOB NOT NULL,
  newest_ts  TEXT NOT NULL,
  synced_at  INTEGER NOT NULL,
  full_at    INTEGER NOT NULL,
  used_at    INTEGER NOT NULL,
  bytes      INTEGER NOT NULL,
  gen        INTEGER NOT NULL,
  followed   INTEGER NOT NULL,
  PRIMARY KEY (channel_id, thread_ts)
);
CREATE INDEX idx_threads_used ON threads(used_at);
CREATE TABLE users (
  user_id    TEXT PRIMARY KEY,
  name       TEXT NOT NULL,
  fetched_at INTEGER NOT NULL
);
`}

func cachePath() string { return filepath.Join(appPaths().StateDir(), "read-cache.db") }

type readCache struct {
	mu           sync.Mutex
	st           *store.Store
	maxBytes     int64
	now          func() time.Time
	live         func() bool
	inflight     map[cursorKey]*inflightRead
	log          *log.Logger
	limitedUntil time.Time
}

type threadEntry struct {
	msgs     []slack.Message
	syncedAt time.Time
	fullAt   time.Time
	gen      int64
	followed bool
}

type inflightRead struct {
	reads  int
	nudged bool
}

type threadRead struct {
	msgs       []slack.Message
	stale      bool
	syncedAt   time.Time
	retryAfter time.Duration
}

func openReadCache(ctx context.Context, path string) (*readCache, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("read cache dir: %w", err)
	}
	st, err := store.Open(ctx, path, cacheSchema, store.WithUnsupportedSchema(store.ArchiveUnsupportedSchema))
	if err != nil {
		return nil, fmt.Errorf("open read cache %s: %w", path, err)
	}
	return &readCache{
		st: st, maxBytes: cacheMaxBytes, now: time.Now, inflight: map[cursorKey]*inflightRead{},
		log: log.New(os.Stderr, "["+binaryName+" cache] ", log.LstdFlags),
	}, nil
}

func (c *readCache) Close() error { return c.st.Close() }

func (c *readCache) replies(ctx context.Context, api slackAPI, channelID, threadTS string) (threadRead, error) {
	return c.read(ctx, api, channelID, threadTS, "")
}

func (c *readCache) repliesHolding(ctx context.Context, api slackAPI, channelID, threadTS, ts string) (threadRead, error) {
	return c.read(ctx, api, channelID, threadTS, ts)
}

func (c *readCache) read(ctx context.Context, api slackAPI, channelID, threadTS, want string) (threadRead, error) {
	now := c.now()
	c.mu.Lock()
	entry, found, err := c.load(ctx, channelID, threadTS)
	c.mu.Unlock()
	if err != nil {
		return threadRead{}, err
	}
	held := found && (want == "" || slices.ContainsFunc(entry.msgs, func(m slack.Message) bool { return m.Timestamp == want }))
	if held && entry.fresh(now, c.live()) {
		return threadRead{msgs: entry.msgs}, c.touch(ctx, channelID, threadTS, now)
	}
	if until := c.limited(); held && now.Before(until) {
		return threadRead{msgs: entry.msgs, stale: true, syncedAt: entry.syncedAt, retryAfter: until.Sub(now)}, c.touch(ctx, channelID, threadTS, now)
	}
	since := ""
	if found && entry.whole(now) {
		since = entry.newest()
	}
	fetchCtx := ctx
	if held {
		fetchCtx = withoutRateWait(ctx)
	}
	fetched, nudged, err := c.fetch(fetchCtx, api, cursorKey{channelID, threadTS}, since)
	var rate *slack.RateLimitedError
	if errors.As(err, &rate) {
		c.limit(c.now().Add(rate.RetryAfter))
	}
	if found && errors.As(err, &rate) {
		return threadRead{msgs: entry.msgs, stale: true, syncedAt: entry.syncedAt, retryAfter: rate.RetryAfter}, c.touch(ctx, channelID, threadTS, now)
	}
	if err != nil {
		return threadRead{}, err
	}
	_, asUser := api.(userReads)
	next := threadEntry{msgs: fetched, syncedAt: now, fullAt: now, gen: entry.gen, followed: !asUser}
	if nudged {
		next.syncedAt = time.UnixMilli(0)
	}
	if since != "" {
		next.msgs, next.fullAt = mergeMessages(entry.msgs, fetched), entry.fullAt
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return threadRead{msgs: next.msgs}, c.commit(ctx, channelID, threadTS, next)
}

func (c *readCache) limited() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.limitedUntil
}

func (c *readCache) limit(until time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if until.After(c.limitedUntil) {
		c.limitedUntil = until
	}
}

func (c *readCache) cached(ctx context.Context, channelID, threadTS string) ([]slack.Message, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, _, err := c.load(ctx, channelID, threadTS)
	return entry.msgs, err
}

func (c *readCache) fetch(ctx context.Context, api slackAPI, key cursorKey, since string) ([]slack.Message, bool, error) {
	c.mu.Lock()
	read := c.inflight[key]
	if read == nil {
		read = &inflightRead{}
		c.inflight[key] = read
	}
	read.reads++
	c.mu.Unlock()
	fetched, err := fetchReplies(ctx, api, key.channelID, key.threadTS, since)
	c.mu.Lock()
	defer c.mu.Unlock()
	read.reads--
	if read.reads == 0 {
		delete(c.inflight, key)
	}
	return fetched, read.nudged, err
}

func (c *readCache) nudged(ctx context.Context, n messageNudge) {
	key := cursorKey{n.Channel, cmp.Or(n.ThreadTS, n.TS)}
	c.mu.Lock()
	defer c.mu.Unlock()
	if read := c.inflight[key]; read != nil {
		read.nudged = true
	}
	if _, err := c.st.DB().ExecContext(ctx, `UPDATE threads SET synced_at = 0, gen = gen + 1 WHERE channel_id = ? AND thread_ts = ?`,
		key.channelID, key.threadTS); err != nil {
		c.log.Printf("nudge %s/%s: its cached thread stays as it was: %v", key.channelID, key.threadTS, err)
	}
}

func (c *readCache) missedNudges(ctx context.Context) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := c.st.DB().ExecContext(ctx, `UPDATE threads SET synced_at = 0, gen = gen + 1`); err != nil {
		c.log.Printf("socket replays no nudges, but the cached threads stay as they were: %v", err)
	}
}

func (c *readCache) generation(ctx context.Context, channelID, threadTS string) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.loadGeneration(ctx, channelID, threadTS)
}

func (c *readCache) after(ctx context.Context, channelID, threadTS, since string) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, _, err := c.load(ctx, channelID, threadTS)
	return len(slices.DeleteFunc(entry.msgs, func(m slack.Message) bool { return !tsLess(since, m.Timestamp) })), err
}

func (c *readCache) observe(ctx context.Context, channelID, threadTS, since string, gen int64, fetched []slack.Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if len(fetched) == 0 {
		var entry threadEntry
		var fullAt int64
		var newest string
		err := c.st.DB().QueryRowContext(ctx, `SELECT gen, full_at, newest_ts FROM threads WHERE channel_id = ? AND thread_ts = ?`,
			channelID, threadTS).Scan(&entry.gen, &fullAt, &newest)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("load cached thread %s/%s: %w", channelID, threadTS, err)
		}
		entry.fullAt = time.UnixMilli(fullAt)
		if entry.gen != gen || !entry.whole(now) || tsLess(newest, since) {
			return nil
		}
		if _, err := c.st.DB().ExecContext(ctx, `UPDATE threads SET synced_at = ?, used_at = ?, gen = gen + 1 WHERE channel_id = ? AND thread_ts = ?`,
			now.UnixMilli(), c.now().UnixMilli(), channelID, threadTS); err != nil {
			return fmt.Errorf("save cached thread %s/%s: %w", channelID, threadTS, err)
		}
		return nil
	}
	entry, found, err := c.load(ctx, channelID, threadTS)
	if err != nil || !found || entry.gen != gen || !entry.whole(now) || tsLess(entry.newest(), since) {
		return err
	}
	entry.msgs = mergeMessages(entry.msgs, fetched)
	entry.syncedAt = now
	return c.commit(ctx, channelID, threadTS, entry)
}

func (c *readCache) invalidate(ctx context.Context, channelID, threadTS string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := c.st.DB().ExecContext(ctx, `UPDATE threads SET synced_at = 0, full_at = 0, gen = gen + 1 WHERE channel_id = ? AND thread_ts = ?`,
		channelID, threadTS); err != nil {
		return fmt.Errorf("invalidate cached thread %s/%s: %w", channelID, threadTS, err)
	}
	return nil
}

func (c *readCache) edited(ctx context.Context, channelID, ts string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := c.st.DB().ExecContext(ctx, `UPDATE threads SET synced_at = 0, full_at = 0, gen = gen + 1
		WHERE channel_id = ? AND thread_ts <= ? AND newest_ts >= ? AND instr(messages, ?) > 0`,
		channelID, ts, ts, `"ts":"`+ts+`"`); err != nil {
		c.log.Printf("message %s/%s changed, but the cached threads holding it stay: %v", channelID, ts, err)
	}
}

type expiringAPI struct {
	slackAPI
	cache *readCache
}

func (c *readCache) expiring(newAPI func(token string) slackAPI) func(token string) slackAPI {
	return func(token string) slackAPI { return expiringAPI{newAPI(token), c} }
}

func (e expiringAPI) UpdateMessageContext(ctx context.Context, channelID, timestamp string, options ...slack.MsgOption) (string, string, string, error) {
	channel, ts, text, err := e.slackAPI.UpdateMessageContext(ctx, channelID, timestamp, options...)
	if err == nil {
		e.cache.edited(ctx, channelID, timestamp)
	}
	return channel, ts, text, err
}

func (e expiringAPI) DeleteMessageContext(ctx context.Context, channelID, timestamp string) (string, string, error) {
	channel, ts, err := e.slackAPI.DeleteMessageContext(ctx, channelID, timestamp)
	if err == nil {
		e.cache.edited(ctx, channelID, timestamp)
	}
	return channel, ts, err
}

func (c *readCache) commit(ctx context.Context, channelID, threadTS string, e threadEntry) error {
	current, err := c.loadGeneration(ctx, channelID, threadTS)
	if err != nil || current != e.gen {
		return err
	}
	e.gen++
	return c.save(ctx, channelID, threadTS, e)
}

func (c *readCache) loadGeneration(ctx context.Context, channelID, threadTS string) (int64, error) {
	var gen int64
	err := c.st.DB().QueryRowContext(ctx, `SELECT gen FROM threads WHERE channel_id = ? AND thread_ts = ?`,
		channelID, threadTS).Scan(&gen)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("load cached thread %s/%s: %w", channelID, threadTS, err)
	}
	return gen, nil
}

func (c *readCache) load(ctx context.Context, channelID, threadTS string) (threadEntry, bool, error) {
	var (
		body             []byte
		syncedAt, fullAt int64
		gen              int64
		followed         bool
	)
	err := c.st.DB().QueryRowContext(ctx, `SELECT messages, synced_at, full_at, gen, followed FROM threads WHERE channel_id = ? AND thread_ts = ?`,
		channelID, threadTS).Scan(&body, &syncedAt, &fullAt, &gen, &followed)
	if errors.Is(err, sql.ErrNoRows) {
		return threadEntry{}, false, nil
	}
	if err != nil {
		return threadEntry{}, false, fmt.Errorf("load cached thread %s/%s: %w", channelID, threadTS, err)
	}
	e := threadEntry{syncedAt: time.UnixMilli(syncedAt), fullAt: time.UnixMilli(fullAt), gen: gen, followed: followed}
	if err := json.Unmarshal(body, &e.msgs); err != nil {
		return threadEntry{}, false, fmt.Errorf("decode cached thread %s/%s: %w", channelID, threadTS, err)
	}
	return e, true, nil
}

func (c *readCache) save(ctx context.Context, channelID, threadTS string, e threadEntry) error {
	body, err := json.Marshal(e.msgs)
	if err != nil {
		return fmt.Errorf("encode cached thread %s/%s: %w", channelID, threadTS, err)
	}
	db := c.st.DB()
	if _, err := db.ExecContext(ctx, `INSERT INTO threads(channel_id, thread_ts, messages, newest_ts, synced_at, full_at, used_at, bytes, gen, followed)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(channel_id, thread_ts) DO UPDATE SET messages = excluded.messages, newest_ts = excluded.newest_ts,
		synced_at = excluded.synced_at, full_at = excluded.full_at, used_at = excluded.used_at, bytes = excluded.bytes, gen = excluded.gen,
		followed = excluded.followed`,
		channelID, threadTS, body, e.newest(), e.syncedAt.UnixMilli(), e.fullAt.UnixMilli(), c.now().UnixMilli(), len(body), e.gen, e.followed); err != nil {
		return fmt.Errorf("save cached thread %s/%s: %w", channelID, threadTS, err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM threads WHERE rowid IN (
		SELECT rowid FROM (SELECT rowid, SUM(bytes) OVER (ORDER BY used_at DESC, rowid DESC) AS kept FROM threads) WHERE kept > ?)`,
		c.maxBytes); err != nil {
		return fmt.Errorf("evict cached threads: %w", err)
	}
	return nil
}

func (c *readCache) touch(ctx context.Context, channelID, threadTS string, now time.Time) error {
	if _, err := c.st.DB().ExecContext(ctx, `UPDATE threads SET used_at = ? WHERE channel_id = ? AND thread_ts = ?`,
		now.UnixMilli(), channelID, threadTS); err != nil {
		return fmt.Errorf("touch cached thread %s/%s: %w", channelID, threadTS, err)
	}
	return nil
}

func (c *readCache) name(ctx context.Context, api slackAPI, m slack.Message) string {
	if m.Username != "" {
		return m.Username
	}
	if m.User == "" {
		return m.BotID
	}
	db := c.st.DB()
	var (
		cached    string
		fetchedAt int64
	)
	err := db.QueryRowContext(ctx, `SELECT name, fetched_at FROM users WHERE user_id = ?`, m.User).Scan(&cached, &fetchedAt)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		c.log.Printf("load cached name %s: %v", m.User, err)
	}
	if err == nil && c.now().Sub(time.UnixMilli(fetchedAt)) < nameTTL {
		return cached
	}
	u, apiErr := api.GetUserInfoContext(ctx, m.User)
	if apiErr != nil {
		if err == nil {
			return cached
		}
		return m.User
	}
	name := u.Profile.DisplayName
	if name == "" {
		name = u.RealName
	}
	if name == "" {
		name = u.Name
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO users(user_id, name, fetched_at) VALUES (?, ?, ?)
		ON CONFLICT(user_id) DO UPDATE SET name = excluded.name, fetched_at = excluded.fetched_at`,
		m.User, name, c.now().UnixMilli()); err != nil {
		c.log.Printf("save cached name %s: %v", m.User, err)
	}
	return name
}

func (e threadEntry) newest() string { return newestTS(e.msgs) }

func (e threadEntry) whole(now time.Time) bool { return now.Sub(e.fullAt) < cacheFullRefresh }

func (e threadEntry) fresh(now time.Time, live bool) bool {
	if !e.whole(now) {
		return false
	}
	if e.followed && live {
		return e.syncedAt.UnixMilli() != 0
	}
	return now.Sub(e.syncedAt) < cacheFresh
}

func newestTS(msgs []slack.Message) string {
	if len(msgs) == 0 {
		return ""
	}
	return msgs[len(msgs)-1].Timestamp
}

func mergeMessages(base, fetched []slack.Message) []slack.Message {
	byTS := make(map[string]slack.Message, len(base)+len(fetched))
	for _, m := range base {
		byTS[m.Timestamp] = m
	}
	for _, m := range fetched {
		byTS[m.Timestamp] = m
	}
	return sortByTS(slices.Collect(maps.Values(byTS)))
}
