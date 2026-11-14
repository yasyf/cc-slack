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

func (f *fixture) read(threadTS string) []slack.Message {
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
	if got := texts(f.read(testThread)); !slices.Equal(got, []string{"root", "first"}) {
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
	if got := texts(f.read(testThread)); !slices.Equal(got, []string{"root", "first", "second"}) {
		t.Fatalf("merged thread = %q", got)
	}
	if got := f.slack.repliesOldest(); !slices.Equal(got, []string{"", firstReply}) {
		t.Fatalf("replies oldest = %q, want a full read then a delta from the newest cached ts", got)
	}

	f.slack.setText(firstReply, "first, edited")
	f.slack.remove(testChannel, secondReply)
	f.now = f.now.Add(cacheFullRefresh)
	if got := texts(f.read(testThread)); !slices.Equal(got, []string{"root", "first, edited"}) {
		t.Fatalf("refreshed thread = %q, want the edit picked up and the deleted reply gone", got)
	}
	if got := f.slack.repliesOldest(); got[len(got)-1] != "" {
		t.Fatalf("replies oldest = %q, want a full read after %v", got, cacheFullRefresh)
	}
}

func TestARateLimitedThreadReadFailsUntilSlackAnswers(t *testing.T) {
	f := newFixture(t)
	seedThread(f)
	f.read(testThread)
	f.slack.add(testChannel, reply(secondReply, "second"))
	f.now = f.now.Add(cacheFresh)
	f.slack.fail(methodReplies, &slack.RateLimitedError{RetryAfter: 30 * time.Second})

	msgs, err := f.cache.replies(f.ctx, f.slack, testChannel, testThread)
	if rate, limited := errors.AsType[*slack.RateLimitedError](err); !limited || rate.RetryAfter != 30*time.Second || msgs != nil {
		t.Fatalf("rate-limited read = %q, %v, want no messages and Slack's 30s Retry-After", texts(msgs), err)
	}
	calls := f.slack.callCount(methodReplies)
	f.now = f.now.Add(10 * time.Second)
	msgs, err = f.cache.replies(f.ctx, f.slack, testChannel, testThread)
	if rate, limited := errors.AsType[*slack.RateLimitedError](err); !limited || rate.RetryAfter != 20*time.Second || msgs != nil {
		t.Fatalf("read inside the limit = %q, %v, want no messages and the 20s Slack still limits", texts(msgs), err)
	}
	if n := f.slack.callCount(methodReplies); n != calls {
		t.Fatalf("replies calls = %d, want %d: a cached thread asks Slack nothing until its Retry-After passes", n, calls)
	}
	f.now = f.now.Add(20 * time.Second)
	if got := texts(f.read(testThread)); !slices.Equal(got, []string{"root", "first", "second"}) {
		t.Fatalf("read after the limit = %q, want a fresh merged thread", got)
	}

	f.slack.fail(methodReplies, &slack.RateLimitedError{RetryAfter: 30 * time.Second})
	_, err = f.cache.replies(f.ctx, f.slack, testChannel, "1800000000.000900")
	if !errors.As(err, new(*slack.RateLimitedError)) {
		t.Fatalf("uncached read under a rate limit: %v, want the rate-limit error", err)
	}
}

func TestThreadReadNamingAMessageTheCacheLacksAsksSlack(t *testing.T) {
	f := newFixture(t)
	seedThread(f)
	f.read(testThread)
	f.slack.add(testChannel, reply(secondReply, "second"))
	f.now = f.now.Add(time.Second)
	read, err := f.cache.repliesHolding(f.ctx, f.slack, testChannel, testThread, secondReply)
	if err != nil {
		t.Fatal(err)
	}
	if got := texts(read); !slices.Equal(got, []string{"root", "first", "second"}) {
		t.Fatalf("thread read for its newest reply = %q, want it fetched though the cached copy is fresh", got)
	}
	f.slack.fail(methodReplies, &slack.RateLimitedError{RetryAfter: 30 * time.Second})
	f.now = f.now.Add(cacheFresh)
	if _, err := f.cache.replies(f.ctx, f.slack, testChannel, testThread); !errors.As(err, new(*slack.RateLimitedError)) {
		t.Fatalf("rate-limited read: %v, want the rate-limit error", err)
	}
	calls := f.slack.callCount(methodReplies)
	if _, err := f.cache.repliesHolding(f.ctx, f.slack, testChannel, testThread, "1800000000.000900"); err != nil {
		t.Fatal(err)
	}
	if n := f.slack.callCount(methodReplies); n != calls+1 {
		t.Fatalf("replies calls = %d, want %d: a read naming a message the cache lacks asks Slack inside a rate limit", n, calls+1)
	}
}

func TestAFollowedThreadStaysFreshUntilANudgeNamesIt(t *testing.T) {
	f := newFixture(t)
	f.cache.live = func() bool { return true }
	seedThread(f)
	f.read(testThread)
	f.now = f.now.Add(cacheFullRefresh - time.Second)
	f.read(testThread)
	if n := f.slack.callCount(methodReplies); n != 1 {
		t.Fatalf("replies calls = %d, want 1: no nudge named the thread", n)
	}

	f.slack.add(testChannel, reply(secondReply, "second"))
	f.cache.nudged(f.ctx, messageNudge{Channel: testChannel, TS: secondReply, ThreadTS: testThread})
	if got := texts(f.read(testThread)); !slices.Equal(got, []string{"root", "first", "second"}) {
		t.Fatalf("nudged thread = %q, want the reply the nudge named", got)
	}
	f.read(testThread)
	if got := f.slack.repliesOldest(); !slices.Equal(got, []string{"", firstReply}) {
		t.Fatalf("replies oldest = %q, want one whole read, then one delta for the nudge", got)
	}
}

func TestAThreadReadAsTheUserKeepsTheFifteenSecondWindow(t *testing.T) {
	f := newFixture(t)
	f.cache.live = func() bool { return true }
	seedThread(f)
	for range 2 {
		if _, err := f.cache.replies(f.ctx, userReads{f.slack}, testChannel, testThread); err != nil {
			t.Fatal(err)
		}
		f.now = f.now.Add(cacheFresh)
	}
	if n := f.slack.callCount(methodReplies); n != 2 {
		t.Fatalf("replies calls = %d, want 2: no nudge follows a channel the bot is not in", n)
	}
}

type nudgedMidRead struct {
	slackAPI
	nudge func()
}

func (n nudgedMidRead) GetConversationRepliesContext(ctx context.Context, params *slack.GetConversationRepliesParameters) ([]slack.Message, bool, string, error) {
	msgs, more, next, err := n.slackAPI.GetConversationRepliesContext(ctx, params)
	n.nudge()
	return msgs, more, next, err
}

func TestANudgeDuringAThreadsFirstReadLeavesItForADelta(t *testing.T) {
	f := newFixture(t)
	f.cache.live = func() bool { return true }
	seedThread(f)
	api := nudgedMidRead{f.slack, func() {
		f.slack.add(testChannel, reply(secondReply, "second"))
		f.cache.nudged(f.ctx, messageNudge{Channel: testChannel, TS: secondReply, ThreadTS: testThread})
	}}
	if _, err := f.cache.replies(f.ctx, api, testChannel, testThread); err != nil {
		t.Fatal(err)
	}
	if got := texts(f.read(testThread)); !slices.Equal(got, []string{"root", "first", "second"}) {
		t.Fatalf("thread = %q, want the reply nudged while the first read ran", got)
	}
}

func TestAHelloWithNoReplayRereadsEveryFollowedThread(t *testing.T) {
	f := newFixture(t)
	f.cache.live = func() bool { return true }
	seedThread(f)
	f.read(testThread)
	f.slack.add(testChannel, reply(secondReply, "second"))
	if _, err := f.app.hello(); err != nil {
		t.Fatal(err)
	}
	if got := texts(f.read(testThread)); !slices.Equal(got, []string{"root", "first", "second"}) {
		t.Fatalf("thread = %q, want a reread once the socket could not replay what it missed", got)
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
	if got := texts(f.read(testThread)); !slices.Equal(got, []string{"root", "first", "second"}) {
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
	f.slack.setText(firstReply, "first, edited")
	if err := f.cache.invalidate(f.ctx, testChannel, testThread); err != nil {
		t.Fatal(err)
	}
	f.read(testThread)
	if err := f.cache.observe(f.ctx, testChannel, testThread, firstReply, gen, polled); err != nil {
		t.Fatal(err)
	}
	if got := texts(f.read(testThread)); !slices.Equal(got, []string{"root", "first, edited"}) {
		t.Fatalf("thread = %q, want the older poll dropped", got)
	}
}

func TestEmptyPollUpdatesOnlyCacheFreshness(t *testing.T) {
	for _, followed := range []bool{false, true} {
		t.Run(fmt.Sprintf("followed=%t", followed), func(t *testing.T) {
			ctx := context.Background()
			c := openTestCache(t)
			now := time.Unix(1_800_000_100, 0)
			c.now = func() time.Time { return now }
			entry := threadEntry{
				msgs:     []slack.Message{rootMessage(testThread, "root"), reply(firstReply, "first")},
				syncedAt: time.UnixMilli(0), fullAt: now, gen: 7, followed: followed,
			}
			if err := c.save(ctx, testChannel, testThread, entry); err != nil {
				t.Fatal(err)
			}
			type row struct {
				body, newest                        string
				syncedAt, fullAt, usedAt, size, gen int64
				followed                            bool
			}
			readRow := func() row {
				t.Helper()
				var got row
				err := c.st.DB().QueryRowContext(ctx, `SELECT messages, newest_ts, synced_at, full_at, used_at, bytes, gen, followed
					FROM threads WHERE channel_id = ? AND thread_ts = ?`, testChannel, testThread).
					Scan(&got.body, &got.newest, &got.syncedAt, &got.fullAt, &got.usedAt, &got.size, &got.gen, &got.followed)
				if err != nil {
					t.Fatal(err)
				}
				return got
			}
			want := readRow()
			for _, stmt := range []string{
				`CREATE TRIGGER no_body_rewrite BEFORE UPDATE OF messages, newest_ts, full_at, bytes, followed ON threads
					BEGIN SELECT RAISE(ABORT, 'empty poll rewrote cached content'); END`,
				`CREATE TRIGGER no_eviction BEFORE DELETE ON threads
					BEGIN SELECT RAISE(ABORT, 'empty poll evicted a thread'); END`,
			} {
				if _, err := c.st.DB().ExecContext(ctx, stmt); err != nil {
					t.Fatal(err)
				}
			}
			c.maxBytes = 0
			for range 2 {
				now = now.Add(time.Second)
				if err := c.observe(ctx, testChannel, testThread, firstReply, want.gen, nil); err != nil {
					t.Fatal(err)
				}
				want.syncedAt, want.usedAt, want.gen = now.UnixMilli(), now.UnixMilli(), want.gen+1
				if got := readRow(); got != want {
					t.Fatalf("cached row = %+v, want %+v", got, want)
				}
			}
		})
	}
}

func TestEmptyPollIgnoresUnusableCacheEntries(t *testing.T) {
	for _, name := range []string{"missing", "stale generation", "expired", "invalidated", "newer cursor"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			c := openTestCache(t)
			now := time.Unix(1_800_000_100, 0)
			c.now = func() time.Time { return now }
			entry := threadEntry{
				msgs:     []slack.Message{rootMessage(testThread, "root"), reply(firstReply, "first")},
				syncedAt: now, fullAt: now, gen: 7, followed: true,
			}
			gen, since := entry.gen, firstReply
			switch name {
			case "stale generation":
				gen--
			case "expired":
				entry.fullAt = now.Add(-cacheFullRefresh)
			case "invalidated":
				entry.fullAt = time.UnixMilli(0)
			case "newer cursor":
				since = secondReply
			}
			if name != "missing" {
				if err := c.save(ctx, testChannel, testThread, entry); err != nil {
					t.Fatal(err)
				}
			}
			for _, stmt := range []string{
				`CREATE TRIGGER no_update BEFORE UPDATE ON threads BEGIN SELECT RAISE(ABORT, 'ignored poll updated a thread'); END`,
				`CREATE TRIGGER no_insert BEFORE INSERT ON threads BEGIN SELECT RAISE(ABORT, 'ignored poll inserted a thread'); END`,
				`CREATE TRIGGER no_delete BEFORE DELETE ON threads BEGIN SELECT RAISE(ABORT, 'ignored poll deleted a thread'); END`,
			} {
				if _, err := c.st.DB().ExecContext(ctx, stmt); err != nil {
					t.Fatal(err)
				}
			}
			if err := c.observe(ctx, testChannel, testThread, since, gen, []slack.Message{}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCacheGenerationChecksDoNotScaleWithMessageBodies(t *testing.T) {
	ctx := context.Background()
	c := openTestCache(t)
	now := time.Unix(1_800_000_100, 0)
	c.now = func() time.Time { return now }
	gen, err := c.generation(ctx, testChannel, testThread)
	if err != nil || gen != 0 {
		t.Fatalf("missing generation = %d, %v, want 0, nil", gen, err)
	}
	entry := threadEntry{msgs: []slack.Message{rootMessage(testThread, "root")}, syncedAt: now, fullAt: now}
	if err := c.commit(ctx, testChannel, testThread, entry); err != nil {
		t.Fatal(err)
	}
	checks := []struct {
		name string
		run  func()
	}{
		{"generation", func() {
			gen, err := c.generation(ctx, testChannel, testThread)
			if err != nil || gen != 1 {
				t.Fatalf("generation = %d, %v, want 1, nil", gen, err)
			}
		}},
		{"stale commit", func() {
			if err := c.commit(ctx, testChannel, testThread, entry); err != nil {
				t.Fatal(err)
			}
		}},
	}
	small := make([]float64, len(checks))
	for i, check := range checks {
		small[i] = testing.AllocsPerRun(5, check.run)
	}
	large := entry
	large.msgs = slices.Clone(entry.msgs)
	large.gen = 1
	for i := range 128 {
		large.msgs = append(large.msgs, reply(fmt.Sprintf("1800000000.%06d", 200+i), strings.Repeat("x", 1024)))
	}
	if err := c.save(ctx, testChannel, testThread, large); err != nil {
		t.Fatal(err)
	}
	for i, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			if got := testing.AllocsPerRun(5, check.run); got > small[i] {
				t.Fatalf("allocations with 129 messages = %g, want no more than %g with 1 message", got, small[i])
			}
		})
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

func TestThreadOpSeesTheBotsOwnReply(t *testing.T) {
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
}

func TestThreadOpFailsUnderARateLimitRatherThanAnswerAnOlderCopy(t *testing.T) {
	const (
		thread = "1794519611.114589"
		ninth  = "1794526467.338209"
		his    = "1794531817.560739"
	)
	l := startDaemon(t)
	loggedIn(t, l)
	inThread := func(ts, text string) slack.Message {
		m := reply(ts, text)
		m.ThreadTimestamp = thread
		return m
	}
	l.slack.add(testChannel, rootMessage(thread, "root"))
	for i := range 7 {
		l.slack.add(testChannel, inThread(fmt.Sprintf("179452%04d.000100", i), fmt.Sprintf("reply %d", i+1)))
	}
	l.slack.add(testChannel, inThread(ninth, "the ninth message"))
	permalink := "https://acme.slack.com/archives/C1/p" + strings.ReplaceAll(thread, ".", "")
	var cached threadResult
	if err := json.Unmarshal(l.do(opThread, map[string]string{"url": permalink}).Body, &cached); err != nil {
		t.Fatal(err)
	}
	if len(cached.Messages) != 9 {
		t.Fatalf("first read = %d messages, want the 9 the cache then held", len(cached.Messages))
	}
	l.slack.add(testChannel, inThread(his, "his newest message"))
	l.post(opReply, map[string]any{"url": permalink, "text": "on it", "no_watch": true})
	l.slack.fail(methodReplies, &slack.RateLimitedError{RetryAfter: 30 * time.Second})

	raw, _ := json.Marshal(map[string]string{"url": permalink})
	answer, err := invoke(l.ctx, l.deps, testSession, "/any", opThread, raw)
	if err == nil || !strings.Contains(err.Error(), "ratelimited") || !strings.Contains(err.Error(), "30s more") || strings.Contains(err.Error(), "mcp__slack__") {
		t.Fatalf("rate-limited read of thread %s = %s, %v, want it refused with the 30s Slack limits it and no Slack MCP tool, never the 9 cached messages without %s", thread, answer.Body, err, his)
	}
}

func TestEachRateLimitedSlackCallOfAThreadReadNamesTheThreadAndTheWait(t *testing.T) {
	named := "cc-slack can't read thread " + testThread + " now (ratelimited): Slack limits the read for "
	cases := []struct {
		name   string
		method string
		err    error
		raw    bool
		want   string
	}{
		{"the replies fetch", methodReplies, &slack.RateLimitedError{RetryAfter: 30 * time.Second}, false, named + "30s more, so read the thread again after that"},
		{"the raw refetch", methodRawMessage, &slack.RateLimitedError{RetryAfter: 10 * time.Second}, true, named + "10s more, so read the thread again after that"},
		{"an image block's files.info", "files.info", &slack.RateLimitedError{RetryAfter: time.Minute}, false, named + "60s more, so read the thread again after that"},
		{"a ratelimited answer with no Retry-After", methodReplies, slack.SlackErrorResponse{Err: "ratelimited"}, false, "(ratelimited)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := startDaemon(t)
			loggedIn(t, l)
			chart := reply(firstReply, "the chart")
			chart.Blocks = slack.Blocks{BlockSet: []slack.Block{&slack.ImageBlock{Type: slack.MBTImage, AltText: "chart.png", SlackFile: &slack.SlackFileObject{ID: "F2"}}}}
			l.slack.add(testChannel, rootMessage(testThread, "root"))
			l.slack.add(testChannel, chart)
			l.slack.fail(tc.method, tc.err)
			l.refused(opThread, map[string]any{"channel_id": testChannel, "thread_ts": testThread, "ts": firstReply, "raw": tc.raw}, tc.want)
		})
	}
}

func TestRawReadsPrintOnlyTheMessageTheyName(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	l.slack.add(testChannel, rootMessage(testThread, "root"))
	l.slack.add(testChannel, reply(firstReply, "first"))
	l.refused(opThread, map[string]any{"channel_id": testChannel, "thread_ts": testThread, "raw": true}, rawNeedsTS)
	l.refused(opHistory, map[string]any{"channel_id": testChannel, "raw": true}, rawNeedsTS)
	var res threadResult
	if err := json.Unmarshal(l.do(opThread, map[string]any{"channel_id": testChannel, "thread_ts": testThread, "ts": firstReply, "raw": true}).Body, &res); err != nil {
		t.Fatal(err)
	}
	for _, m := range res.Messages {
		if (m.Raw != nil) != (m.TS == firstReply) {
			t.Errorf("message %s raw = %s, want raw only on %s", m.TS, m.Raw, firstReply)
		}
	}
}

func TestThreadOpSinceTSReadsOnlyWhatIsNewer(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	l.slack.add(testChannel, rootMessage(testThread, "root"))
	l.slack.add(testChannel, reply(firstReply, "first"))
	l.slack.add(testChannel, reply(secondReply, "second"))
	readSince := func(since string, limit int) threadResult {
		t.Helper()
		var res threadResult
		if err := json.Unmarshal(l.do(opThread, map[string]any{"channel_id": testChannel, "thread_ts": testThread, "since_ts": since, "limit": limit}).Body, &res); err != nil {
			t.Fatal(err)
		}
		return res
	}

	if res := readSince(firstReply, 0); len(res.Messages) != 1 || res.Messages[0].TS != secondReply || res.Truncated {
		t.Fatalf("read since the first reply = %+v, want only the second reply, whole", res)
	}
	if res := readSince(secondReply, 0); res.Messages == nil || len(res.Messages) != 0 || res.Truncated {
		t.Fatalf("read since the newest reply = %+v, want an empty, whole read", res)
	}
	if res := readSince(testThread, 1); len(res.Messages) != 1 || res.Messages[0].TS != firstReply || !res.Truncated {
		t.Fatalf("read since the root with limit 1 = %+v, want the first reply, marked truncated", res)
	}

	raw, _ := json.Marshal(map[string]string{"channel_id": testChannel, "thread_ts": testThread, "since_ts": "yesterday"})
	if _, err := invoke(l.ctx, l.deps, testSession, "/any", opThread, raw); err == nil || !strings.Contains(err.Error(), `since_ts "yesterday"`) {
		t.Fatalf("read since a bad ts: %v, want it refused", err)
	}
}

func TestThreadOpSinceTSWithALimitPagesForwardFromTheOldest(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	const thread, since, his = "1794352034.047459", "1794354480.000000", "1794354488.276359"
	msg := func(ts, user string) slack.Message {
		return slack.Message{Msg: slack.Msg{Timestamp: ts, ThreadTimestamp: thread, User: user, Text: ts}}
	}
	l.slack.add(testChannel, msg(thread, testUser))
	l.slack.add(testChannel, msg("1794352100.000001", "UBOT"))
	newer := []string{his, "1794354500.000001", "1794354600.000001", "1794407891.335679", "1794407892.657149", "1794477108.255259"}
	for i, ts := range newer {
		l.slack.add(testChannel, msg(ts, []string{testUser, "UBOT"}[i%2]))
	}

	var read []string
	for from, pages := since, 0; ; pages++ {
		var res threadResult
		if err := json.Unmarshal(l.do(opThread, map[string]any{"channel_id": testChannel, "thread_ts": thread, "since_ts": from, "limit": 3}).Body, &res); err != nil {
			t.Fatal(err)
		}
		if pages == 0 && (len(res.Messages) == 0 || res.Messages[0].TS != his || !res.Messages[0].FromUser || !res.Truncated) {
			t.Fatalf("read since %s with limit 3 = %+v, want his %s first, marked truncated", since, res, his)
		}
		for _, m := range res.Messages {
			read = append(read, m.TS)
		}
		if !res.Truncated || pages > len(newer) {
			break
		}
		from = read[len(read)-1]
	}
	if !slices.Equal(read, newer) {
		t.Fatalf("paging from %s by the last ts read = %v, want every newer message once, in order: %v", since, read, newer)
	}
}

func TestThreadOpSinceTSTakesWholeSeconds(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	l.slack.add(testChannel, rootMessage(testThread, "root"))
	l.slack.add(testChannel, reply(firstReply, "first"))
	l.slack.add(testChannel, reply(secondReply, "second"))
	whole, _, _ := strings.Cut(testThread, ".")
	var res threadResult
	if err := json.Unmarshal(l.do(opThread, map[string]any{"channel_id": testChannel, "thread_ts": testThread, "since_ts": whole}).Body, &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Messages) != 3 || res.Messages[2].TS != secondReply {
		t.Fatalf("read since %s = %+v, want the root and both replies, all newer than that second", whole, res)
	}
}

func TestThreadOpSaysWhatItReadAndRefusesAFieldItDoesNotKnow(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	l.refused(opThread, map[string]any{"channel": testChannel, "thread_ts": testThread}, `thread takes no field "channel"`)
	l.slack.fail("conversations.open", slack.SlackErrorResponse{Err: "user_not_found"})
	l.refused(opThread, map[string]any{"channel_id": "U0NOBODY001", "thread_ts": testThread}, "U0NOBODY001 is not a conversation the bot can read (user_not_found)")
	l.slack.fail(methodReplies, slack.SlackErrorResponse{Err: "thread_not_found"})
	l.refused(opThread, map[string]any{"thread_ts": "1790000000.000001"}, "thread 1790000000.000001 is not in your DM with the app")
}
