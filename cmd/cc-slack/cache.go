package main

import (
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
	currentFallback  = 30 * time.Second
	currentPatience  = 60 * time.Second
	currentReserve   = 30 * time.Second
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
	mu       sync.Mutex
	st       *store.Store
	maxBytes int64
	now      func() time.Time
	sleep    func(context.Context, time.Duration) error
	log      *log.Logger
}

type threadEntry struct {
	msgs     []slack.Message
	syncedAt time.Time
	fullAt   time.Time
	gen      int64
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
		st: st, maxBytes: cacheMaxBytes, now: time.Now, sleep: sleepFor,
		log: log.New(os.Stderr, "["+binaryName+" cache] ", log.LstdFlags),
	}, nil
}

func (c *readCache) Close() error { return c.st.Close() }

func (c *readCache) replies(ctx context.Context, api slackAPI, channelID, threadTS string) (threadRead, error) {
	return c.read(ctx, api, channelID, threadTS, cacheFresh)
}

func (c *readCache) current(ctx context.Context, api slackAPI, channelID, threadTS string) (threadRead, error) {
	patience := currentPatience
	if deadline, ok := ctx.Deadline(); ok {
		patience = min(patience, time.Until(deadline)-currentReserve)
	}
	giveUp := c.now().Add(patience)
	for {
		read, err := c.read(ctx, api, channelID, threadTS, 0)
		if err == nil && !read.stale {
			return read, nil
		}
		if err == nil && c.now().Sub(read.syncedAt) < currentFallback {
			return threadRead{msgs: read.msgs}, nil
		}
		wait := read.retryAfter
		if err != nil {
			var rate *slack.RateLimitedError
			if !errors.As(err, &rate) {
				return threadRead{}, err
			}
			wait = rate.RetryAfter
		}
		if c.now().Add(wait).After(giveUp) {
			return threadRead{}, &slack.RateLimitedError{RetryAfter: wait}
		}
		c.log.Printf("thread %s/%s is rate limited; reading it again in %s", channelID, threadTS, wait)
		if err := c.sleep(ctx, wait); err != nil {
			return threadRead{}, err
		}
	}
}

func sleepFor(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (c *readCache) read(ctx context.Context, api slackAPI, channelID, threadTS string, freshFor time.Duration) (threadRead, error) {
	now := c.now()
	c.mu.Lock()
	entry, found, err := c.load(ctx, channelID, threadTS)
	c.mu.Unlock()
	if err != nil {
		return threadRead{}, err
	}
	if found && entry.fresh(now, freshFor) {
		return threadRead{msgs: entry.msgs}, c.touch(ctx, channelID, threadTS, now)
	}
	since := ""
	if found && entry.whole(now) {
		since = entry.newest()
	}
	fetched, err := fetchReplies(ctx, api, channelID, threadTS, since)
	var rate *slack.RateLimitedError
	if found && errors.As(err, &rate) {
		return threadRead{msgs: entry.msgs, stale: true, syncedAt: entry.syncedAt, retryAfter: rate.RetryAfter}, c.touch(ctx, channelID, threadTS, now)
	}
	if err != nil {
		return threadRead{}, err
	}
	next := threadEntry{msgs: fetched, syncedAt: now, fullAt: now, gen: entry.gen}
	if since != "" {
		next.msgs, next.fullAt = mergeMessages(entry.msgs, fetched), entry.fullAt
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return threadRead{msgs: next.msgs}, c.commit(ctx, channelID, threadTS, next)
}

func (c *readCache) generation(ctx context.Context, channelID, threadTS string) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, _, err := c.load(ctx, channelID, threadTS)
	return entry.gen, err
}

func (c *readCache) observe(ctx context.Context, channelID, threadTS, since string, gen int64, fetched []slack.Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
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

func (c *readCache) commit(ctx context.Context, channelID, threadTS string, e threadEntry) error {
	current, _, err := c.load(ctx, channelID, threadTS)
	if err != nil || current.gen != e.gen {
		return err
	}
	e.gen++
	return c.save(ctx, channelID, threadTS, e)
}

func (c *readCache) load(ctx context.Context, channelID, threadTS string) (threadEntry, bool, error) {
	var (
		body             []byte
		syncedAt, fullAt int64
		gen              int64
	)
	err := c.st.DB().QueryRowContext(ctx, `SELECT messages, synced_at, full_at, gen FROM threads WHERE channel_id = ? AND thread_ts = ?`,
		channelID, threadTS).Scan(&body, &syncedAt, &fullAt, &gen)
	if errors.Is(err, sql.ErrNoRows) {
		return threadEntry{}, false, nil
	}
	if err != nil {
		return threadEntry{}, false, fmt.Errorf("load cached thread %s/%s: %w", channelID, threadTS, err)
	}
	e := threadEntry{syncedAt: time.UnixMilli(syncedAt), fullAt: time.UnixMilli(fullAt), gen: gen}
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
	if _, err := db.ExecContext(ctx, `INSERT INTO threads(channel_id, thread_ts, messages, newest_ts, synced_at, full_at, used_at, bytes, gen)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(channel_id, thread_ts) DO UPDATE SET messages = excluded.messages, newest_ts = excluded.newest_ts,
		synced_at = excluded.synced_at, full_at = excluded.full_at, used_at = excluded.used_at, bytes = excluded.bytes, gen = excluded.gen`,
		channelID, threadTS, body, e.newest(), e.syncedAt.UnixMilli(), e.fullAt.UnixMilli(), c.now().UnixMilli(), len(body), e.gen); err != nil {
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

func (e threadEntry) fresh(now time.Time, within time.Duration) bool {
	return e.whole(now) && now.Sub(e.syncedAt) < within
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
