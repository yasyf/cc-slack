package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/slack-go/slack"

	"github.com/yasyf/cc-interact/agent"
)

const (
	firstReply  = "1800000000.000200"
	secondReply = "1800000000.000300"
)

func rootMessage(ts, text string) slack.Message {
	return slack.Message{Msg: slack.Msg{Timestamp: ts, ThreadTimestamp: ts, User: testUser, Text: text}}
}

func texts(msgs []slack.Message) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, m.Text)
	}
	return out
}

func (f *fixture) read(threadTS string) threadRead {
	f.t.Helper()
	r, err := f.cache.replies(f.ctx, f.slack, testChannel, threadTS)
	if err != nil {
		f.t.Fatal(err)
	}
	return r
}

func (f *fixture) cached(threadTS string) bool {
	f.t.Helper()
	_, found, err := f.cache.load(f.ctx, testChannel, threadTS)
	if err != nil {
		f.t.Fatal(err)
	}
	return found
}

func seedThread(f *fixture) {
	f.slack.add(testChannel, rootMessage(testThread, "root"))
	f.slack.add(testChannel, reply(firstReply, "first"))
}

func TestThreadReadServesAFreshCacheWithoutCallingSlack(t *testing.T) {
	f := newFixture(t)
	seedThread(f)
	f.read(testThread)
	f.now = f.now.Add(cacheFresh - time.Second)
	if got := texts(f.read(testThread).msgs); !slices.Equal(got, []string{"root", "first"}) {
		t.Fatalf("cached thread = %q", got)
	}
	if n := f.slack.callCount(methodReplies); n != 1 {
		t.Fatalf("replies calls = %d, want 1: a fresh cache answers without Slack", n)
	}
}

func TestThreadReadFetchesOnlyTheDeltaAndRefreshesInFull(t *testing.T) {
	f := newFixture(t)
	seedThread(f)
	f.read(testThread)
	f.slack.add(testChannel, reply(secondReply, "second"))
	f.now = f.now.Add(cacheFresh)
	if got := texts(f.read(testThread).msgs); !slices.Equal(got, []string{"root", "first", "second"}) {
		t.Fatalf("merged thread = %q", got)
	}
	if got := f.slack.repliesOldest(); !slices.Equal(got, []string{"", firstReply}) {
		t.Fatalf("replies oldest = %q, want a full read then a delta from the newest cached ts", got)
	}

	f.slack.setText(testChannel, firstReply, "first, edited")
	f.slack.remove(testChannel, secondReply)
	f.now = f.now.Add(cacheFullRefresh)
	if got := texts(f.read(testThread).msgs); !slices.Equal(got, []string{"root", "first, edited"}) {
		t.Fatalf("refreshed thread = %q, want the edit picked up and the deleted reply gone", got)
	}
	if got := f.slack.repliesOldest(); got[len(got)-1] != "" {
		t.Fatalf("replies oldest = %q, want a full read after %v", got, cacheFullRefresh)
	}
}

func TestThreadReadServesTheCacheWhenSlackRateLimits(t *testing.T) {
	f := newFixture(t)
	seedThread(f)
	f.read(testThread)
	f.slack.add(testChannel, reply(secondReply, "second"))
	f.now = f.now.Add(cacheFresh)
	f.slack.fail(methodReplies, &slack.RateLimitedError{RetryAfter: 30 * time.Second})

	stale := f.read(testThread)
	if !stale.stale || stale.retryAfter != 30*time.Second || !slices.Equal(texts(stale.msgs), []string{"root", "first"}) {
		t.Fatalf("rate-limited read = %+v, want the cached thread marked stale with Retry-After", stale)
	}
	if fresh := f.read(testThread); fresh.stale || !slices.Equal(texts(fresh.msgs), []string{"root", "first", "second"}) {
		t.Fatalf("read after the limit = %+v, want a fresh merged thread", fresh)
	}

	f.slack.fail(methodReplies, &slack.RateLimitedError{RetryAfter: 30 * time.Second})
	_, err := f.cache.replies(f.ctx, f.slack, testChannel, "1800000000.000900")
	if !errors.As(err, new(*slack.RateLimitedError)) {
		t.Fatalf("uncached read under a rate limit: %v, want the rate-limit error", err)
	}
}

func TestCurrentReadWaitsOutARateLimitInsideItsPatience(t *testing.T) {
	f := newFixture(t)
	seedThread(f)
	var slept []time.Duration
	f.cache.sleep = func(_ context.Context, d time.Duration) error {
		slept = append(slept, d)
		f.now = f.now.Add(d)
		return nil
	}

	f.slack.fail(methodReplies, &slack.RateLimitedError{RetryAfter: 10 * time.Second})
	f.slack.fail(methodReplies, &slack.RateLimitedError{RetryAfter: 10 * time.Second})
	read, err := f.cache.current(f.ctx, f.slack, testChannel, testThread)
	if err != nil || !slices.Equal(texts(read.msgs), []string{"root", "first"}) || !slices.Equal(slept, []time.Duration{10 * time.Second, 10 * time.Second}) {
		t.Fatalf("current = %+v, %v after sleeping %v, want the thread after two 10s waits", read, err, slept)
	}

	slept = nil
	f.slack.add(testChannel, reply(secondReply, "second"))
	f.now = f.now.Add(currentFallback / 2)
	f.slack.fail(methodReplies, &slack.RateLimitedError{RetryAfter: 10 * time.Second})
	read, err = f.cache.current(f.ctx, f.slack, testChannel, testThread)
	if err != nil || read.stale || !slices.Equal(texts(read.msgs), []string{"root", "first"}) || len(slept) != 0 {
		t.Fatalf("current = %+v, %v after sleeping %v, want the copy synced %v ago without waiting", read, err, slept, currentFallback/2)
	}

	f.now = f.now.Add(currentFallback)
	f.slack.fail(methodReplies, &slack.RateLimitedError{RetryAfter: currentPatience + time.Second})
	_, err = f.cache.current(f.ctx, f.slack, testChannel, testThread)
	if !errors.As(err, new(*slack.RateLimitedError)) || len(slept) != 0 {
		t.Fatalf("current = %v after sleeping %v, want the rate limit when its wait outlasts the patience", err, slept)
	}
}

func TestPollerKeepsAWatchedThreadsCacheCurrent(t *testing.T) {
	f := newFixture(t)
	seedThread(f)
	f.watch(agent.TopLevel, firstReply)
	f.tick()
	if f.cached(testThread) {
		t.Fatal("a poll cached a thread no read had cached: its delta alone is not the whole thread")
	}

	f.read(testThread)
	f.slack.add(testChannel, reply(secondReply, "second"))
	f.now = f.now.Add(time.Minute)
	f.nudgeThread()
	f.tick()
	calls := f.slack.callCount(methodReplies)
	if got := texts(f.read(testThread).msgs); !slices.Equal(got, []string{"root", "first", "second"}) {
		t.Fatalf("watched thread = %q, want the polled reply merged", got)
	}
	if n := f.slack.callCount(methodReplies); n != calls {
		t.Fatalf("replies calls = %d after the poll, want %d: the watch already synced the thread", n, calls)
	}

	f.now = f.now.Add(cacheFullRefresh)
	f.nudgeThread()
	f.tick()
	f.read(testThread)
	if got := f.slack.repliesOldest(); got[len(got)-1] != "" {
		t.Fatalf("replies oldest = %q, want polls to leave the full refresh due", got)
	}

	if err := f.cache.invalidate(f.ctx, testChannel, testThread); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(time.Minute)
	f.nudgeThread()
	f.tick()
	f.read(testThread)
	if got := f.slack.repliesOldest(); got[len(got)-1] != "" {
		t.Fatalf("replies oldest = %q, want a poll to leave an invalidated thread for a full read", got)
	}
}

func TestObserveDropsAPollSupersededByARead(t *testing.T) {
	f := newFixture(t)
	seedThread(f)
	f.read(testThread)
	gen, err := f.cache.generation(f.ctx, testChannel, testThread)
	if err != nil {
		t.Fatal(err)
	}
	polled, err := fetchReplies(f.ctx, f.slack, testChannel, testThread, "")
	if err != nil {
		t.Fatal(err)
	}
	f.slack.setText(testChannel, firstReply, "first, edited")
	if err := f.cache.invalidate(f.ctx, testChannel, testThread); err != nil {
		t.Fatal(err)
	}
	f.read(testThread)
	if err := f.cache.observe(f.ctx, testChannel, testThread, firstReply, gen, polled); err != nil {
		t.Fatal(err)
	}
	if got := texts(f.read(testThread).msgs); !slices.Equal(got, []string{"root", "first, edited"}) {
		t.Fatalf("thread = %q, want the older poll dropped", got)
	}
}

func TestCacheEvictsTheLeastRecentlyReadThread(t *testing.T) {
	f := newFixture(t)
	threads := []string{"1800000000.000100", "1800000000.000200", "1800000000.000300"}
	for i, ts := range threads {
		f.slack.add(testChannel, rootMessage(ts, fmt.Sprintf("root %d", i)))
	}
	readAt := func(ts string) {
		f.now = f.now.Add(time.Second)
		f.read(ts)
	}
	readAt(threads[0])
	var size int64
	if err := f.cache.st.DB().QueryRowContext(f.ctx, `SELECT bytes FROM threads WHERE thread_ts = ?`, threads[0]).Scan(&size); err != nil {
		t.Fatal(err)
	}
	f.cache.maxBytes = 2*size + size/2
	readAt(threads[1])
	readAt(threads[0])
	readAt(threads[2])
	for ts, want := range map[string]bool{threads[0]: true, threads[1]: false, threads[2]: true} {
		if got := f.cached(ts); got != want {
			t.Errorf("thread %s cached = %v, want %v", ts, got, want)
		}
	}
}

func TestNamesSurviveARestartAndOutliveARateLimit(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "read-cache.db")
	fs := newFakeSlack()
	fs.users["UOTHER"] = &slack.User{ID: "UOTHER", Profile: slack.UserProfile{DisplayName: "teammate"}}
	m := reply(firstReply, "hi")
	for range 2 {
		c, err := openReadCache(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		if got := c.name(ctx, fs, m); got != "teammate" {
			t.Fatalf("name = %q", got)
		}
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if n := fs.callCount("users.info"); n != 1 {
		t.Fatalf("users.info calls = %d, want 1 across a restart", n)
	}

	c, err := openReadCache(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	c.now = func() time.Time { return time.Now().Add(nameTTL) }
	fs.fail("users.info", &slack.RateLimitedError{RetryAfter: time.Minute})
	if got := c.name(ctx, fs, m); got != "teammate" {
		t.Fatalf("expired name under a rate limit = %q, want the cached name", got)
	}
}

func TestThreadOpMarksStaleReadsAndSeesTheBotsOwnReply(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	l.slack.add(testChannel, rootMessage(testThread, "root"))
	permalink := "https://acme.slack.com/archives/C1/p" + strings.ReplaceAll(testThread, ".", "")
	readThread := func() threadResult {
		t.Helper()
		var res threadResult
		if err := json.Unmarshal(l.do(opThread, map[string]string{"url": permalink}).Body, &res); err != nil {
			t.Fatal(err)
		}
		return res
	}
	readThread()
	l.post(opReply, map[string]any{"url": permalink, "text": "on it", "no_watch": true})
	if res := readThread(); len(res.Messages) != 2 || res.Messages[1].Text != "on it" {
		t.Fatalf("thread after the bot's reply = %+v, want the reply without waiting out the cache", res.Messages)
	}

	if err := l.app.cache.invalidate(l.ctx, testChannel, testThread); err != nil {
		t.Fatal(err)
	}
	l.slack.fail(methodReplies, &slack.RateLimitedError{RetryAfter: 30 * time.Second})
	if res := readThread(); !res.Stale || res.RetryAfter != 30 || len(res.Messages) != 2 {
		t.Fatalf("rate-limited thread = %+v, want the cached copy with stale and retry_after", res)
	}

	l.slack.fail(methodReplies, &slack.RateLimitedError{RetryAfter: 30 * time.Second})
	raw, _ := json.Marshal(map[string]string{"channel_id": testChannel, "thread_ts": "1800000000.000900"})
	if _, err := invoke(l.ctx, l.deps, testSession, "/any", opThread, raw); err == nil ||
		!strings.Contains(err.Error(), "ratelimited") || !strings.Contains(err.Error(), mcpThread) {
		t.Fatalf("uncached rate-limited read: %v, want the error to name %s", err, mcpThread)
	}
}
