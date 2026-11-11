package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/slack-go/slack"

	"github.com/yasyf/cc-interact/agent"
	"github.com/yasyf/cc-interact/consume"
	"github.com/yasyf/cc-interact/event"
)

const (
	testPRURL    = "https://github.com/acme/app/pull/41"
	testBuildURL = "https://buildkite.com/acme/deploy/builds/7"
)

type fakeRefs struct {
	mu           sync.Mutex
	pr           string
	build        string
	status       int
	header       http.Header
	queries      []string
	buildMessage string
}

func (f *fakeRefs) set(pr, build string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pr, f.build = pr, build
}

func (f *fakeRefs) limit(status int, header http.Header) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status, f.header = status, header
}

func (f *fakeRefs) query(path string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, q := range f.queries {
		if strings.HasPrefix(q, "POST "+path+" ") {
			return q
		}
	}
	return ""
}

func (f *fakeRefs) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.queries)
}

func (f *fakeRefs) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	f.queries = append(f.queries, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization")+" "+string(body))
	if f.status != 0 {
		for k, v := range f.header {
			w.Header()[k] = v
		}
		w.WriteHeader(f.status)
		return
	}
	switch r.URL.Path {
	case "/graphql":
		_, _ = fmt.Fprintf(w, `{"data":{"p0":{"pullRequest":%s}}}`, f.pr)
	case "/v2/organizations/acme/pipelines/deploy/builds/7":
		_, _ = fmt.Fprintf(w, `{"state":%q,"message":%q}`, f.build, f.buildMessage)
	default:
		http.NotFound(w, r)
	}
}

func prJSON(state, checks string, queued bool) string {
	entry := "null"
	if queued {
		entry = `{"state":"QUEUED"}`
	}
	return fmt.Sprintf(`{"state":%q,"isDraft":false,"mergeQueueEntry":%s,"commits":{"nodes":[{"commit":{"statusCheckRollup":{"state":%q}}}]}}`, state, entry, checks)
}

func serveRefs(t *testing.T) (*fakeRefs, *httptest.Server) {
	t.Helper()
	f := &fakeRefs{pr: prJSON("OPEN", "PENDING", false), build: "running"}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv
}

func loggedInWithRefs(t *testing.T, l *liveDaemon) {
	t.Helper()
	l.slack.setUser(&slack.User{ID: testUser, Name: "ym", RealName: "Yasyf Mohamedali", Profile: slack.UserProfile{
		Email: "ym@example.com", Image512: "https://avatars/ym.png", DisplayName: "yasyf",
	}})
	l.do(opLogin, map[string]any{
		"token": "xoxb-test", "email": "ym@example.com", "socket_url": "ws://127.0.0.1:1/live", "repo_root": "/repo", "capt_hook_path": testCaptHook,
		"avatar_key_id": "r2-key", "avatar_secret": "r2-secret",
		"config": map[string]any{"github": map[string]string{"token": "ghp-test"}, "buildkite": map[string]string{"token": "bk-test"}},
	})
}

type testClock struct{ at atomic.Int64 }

func (c *testClock) now() time.Time          { return time.Unix(0, c.at.Load()) }
func (c *testClock) advance(d time.Duration) { c.at.Add(int64(d)) }

func standaloneCards(t *testing.T, l *liveDaemon, srv *httptest.Server, clock *testClock) *liveCards {
	t.Helper()
	return &liveCards{
		path:    t.TempDir() + "/live-cards.json",
		client:  refClient{http: srv.Client(), githubURL: srv.URL, buildkiteURL: srv.URL, now: clock.now},
		log:     log.New(io.Discard, "", 0),
		now:     clock.now,
		plane:   l.srv,
		newAPI:  func(string) slackAPI { return l.slack },
		wakeCh:  make(chan struct{}, 1),
		threads: map[string]*liveThread{},
		refs:    map[string]refState{},
		hosts:   map[string]*refHost{},
		drawing: map[string]*sync.Mutex{},
		undrawn: map[string]map[string]bool{},
	}
}

func seedLiveThread(l *liveCards, card statusCard, at time.Time) {
	t := &liveThread{Channel: "D" + testUser, Thread: "1800000000.000100"}
	t.apply(card, cardTag{Repo: "app", Branch: "dev"}, at, false)
	t.Reply.TS = t.Thread
	l.threads[threadKey(t.Channel, t.Thread)] = t
}

func editsOf(edits []fakeEdit, ts string) []string {
	var out []string
	for _, e := range edits {
		if e.ts == ts {
			out = append(out, e.blocks)
		}
	}
	return out
}

func TestLiveRefsRedrawTheCardWhenStateChangesThenStopPolling(t *testing.T) {
	l := startDaemon(t)
	loggedInWithRefs(t, l)
	refs, srv := serveRefs(t)
	clock := &testClock{}
	clock.at.Store(time.Date(2026, 10, 22, 17, 0, 0, 0, time.UTC).UnixNano())
	cards := standaloneCards(t, l, srv, clock)
	seedLiveThread(cards, statusCard{Status: "progress", Title: "Landing", Refs: []cardLink{{URL: testPRURL}, {URL: testBuildURL, Label: "deploy"}}}, clock.now())

	cards.tick(l.ctx)
	root := editsOf(l.slack.edited(), "1800000000.000100")
	if len(root) != 1 || !strings.Contains(root[0], "🔄 CI running") || !strings.Contains(root[0], "🔄 build running") {
		t.Fatalf("root edits after the first poll = %v, want PR and build state", root)
	}
	if q := refs.query("/graphql"); !strings.Contains(q, "Bearer ghp-test") || !strings.Contains(q, `repository(owner: \"acme\", name: \"app\")`) {
		t.Fatalf("GitHub query = %q, want one batched query with the configured token", q)
	}
	calls := refs.calls()
	if wait := cards.tick(l.ctx); refs.calls() != calls || len(l.slack.edited()) != 1 || wait < 50*time.Second {
		t.Fatalf("an early tick polled %d times, edited %d cards, and waits %s; want no poll and no edit before the interval", refs.calls()-calls, len(l.slack.edited()), wait)
	}

	refs.set(prJSON("OPEN", "SUCCESS", true), "passed")
	clock.advance(refPollEvery)
	cards.tick(l.ctx)
	if root = editsOf(l.slack.edited(), "1800000000.000100"); !strings.Contains(root[len(root)-1], "⏳ queued, CI passed") || !strings.Contains(root[len(root)-1], "✅ build passed") {
		t.Fatalf("root after the PR queued = %s", root[len(root)-1])
	}
	refs.set(prJSON("MERGED", "SUCCESS", false), "passed")
	clock.advance(refPollEvery)
	cards.tick(l.ctx)
	if root = editsOf(l.slack.edited(), "1800000000.000100"); !strings.Contains(root[len(root)-1], "🟣 landed") {
		t.Fatalf("root after the PR landed = %s", root[len(root)-1])
	}
	calls, edits := refs.calls(), len(l.slack.edited())
	clock.advance(refPollEvery)
	cards.tick(l.ctx)
	if refs.calls() != calls || len(l.slack.edited()) != edits {
		t.Fatalf("landed and passed refs polled again: %d calls, %d edits", refs.calls()-calls, len(l.slack.edited())-edits)
	}
}

func TestARestartRedrawsNoCardWhoseRefsStayed(t *testing.T) {
	l := startDaemon(t)
	loggedInWithRefs(t, l)
	_, srv := serveRefs(t)
	clock := &testClock{}
	clock.at.Store(time.Date(2026, 10, 22, 17, 0, 0, 0, time.UTC).UnixNano())
	cards := standaloneCards(t, l, srv, clock)
	seedLiveThread(cards, statusCard{Status: "progress", Title: "Landing", Refs: []cardLink{{URL: testPRURL}, {URL: testBuildURL, Label: "deploy"}}}, clock.now())
	cards.tick(l.ctx)
	if edits := editsOf(l.slack.edited(), "1800000000.000100"); len(edits) != 1 {
		t.Fatalf("edits after the first poll = %d, want the one drawing PR and build state", len(edits))
	}

	restarted := standaloneCards(t, l, srv, clock)
	restarted.path = cards.path
	if err := restarted.load(); err != nil {
		t.Fatal(err)
	}
	clock.advance(refPollEvery)
	restarted.tick(l.ctx)
	if edits := editsOf(l.slack.edited(), "1800000000.000100"); len(edits) != 1 {
		t.Fatalf("a restart redrew the card %d times with nothing moved, want none", len(edits)-1)
	}
}

func TestARateLimitedCardSweepStopsThenRedrawsWhatItSkippedOnceTheLimitPasses(t *testing.T) {
	l := startDaemon(t)
	loggedInWithRefs(t, l)
	_, srv := serveRefs(t)
	clock := &testClock{}
	clock.at.Store(time.Date(2026, 10, 22, 17, 0, 0, 0, time.UTC).UnixNano())
	cards := standaloneCards(t, l, srv, clock)
	card := statusCard{Status: "progress", Title: "Landing", Refs: []cardLink{{URL: testPRURL}}}
	seedLiveThread(cards, card, clock.now())
	second := &liveThread{Channel: "D" + testUser, Thread: "1800000000.000200"}
	second.apply(card, cardTag{Repo: "app", Branch: "dev"}, clock.now(), false)
	second.Reply.TS = second.Thread
	cards.threads[threadKey(second.Channel, second.Thread)] = second
	l.slack.fail("chat.update", &slack.RateLimitedError{RetryAfter: 20 * time.Second})

	if wait := cards.tick(l.ctx); wait != 20*time.Second || len(l.slack.edited()) != 0 {
		t.Fatalf("a sweep Slack limited wrote %d cards and waits %s, want no write and the 20s Slack asked for", len(l.slack.edited()), wait)
	}
	clock.advance(20 * time.Second)
	cards.tick(l.ctx)
	for _, ts := range []string{"1800000000.000100", "1800000000.000200"} {
		if edits := editsOf(l.slack.edited(), ts); len(edits) != 1 || !strings.Contains(edits[0], "🔄 CI running") {
			t.Fatalf("card %s after the limit passed = %v, want one redraw with its PR's state", ts, edits)
		}
	}
}

func TestLiveRefsBackOffUntilTheRateLimitResets(t *testing.T) {
	l := startDaemon(t)
	loggedInWithRefs(t, l)
	refs, srv := serveRefs(t)
	clock := &testClock{}
	clock.at.Store(time.Date(2026, 10, 22, 17, 0, 0, 0, time.UTC).UnixNano())
	cards := standaloneCards(t, l, srv, clock)
	seedLiveThread(cards, statusCard{Status: "progress", Title: "Landing", Refs: []cardLink{{URL: testPRURL}}}, clock.now())
	refs.limit(http.StatusForbidden, http.Header{"X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset": {fmt.Sprint(clock.now().Add(10 * time.Minute).Unix())}})

	if wait := cards.tick(l.ctx); wait < 9*time.Minute || len(l.slack.edited()) != 0 {
		t.Fatalf("a rate-limited poll waits %s and edited %d cards, want the reset honored and no edit", wait, len(l.slack.edited()))
	}
	calls := refs.calls()
	clock.advance(5 * time.Minute)
	cards.tick(l.ctx)
	if refs.calls() != calls {
		t.Fatalf("polled %d times inside the rate-limit window", refs.calls()-calls)
	}
	refs.limit(0, nil)
	clock.advance(6 * time.Minute)
	cards.tick(l.ctx)
	if root := editsOf(l.slack.edited(), "1800000000.000100"); len(root) != 1 || !strings.Contains(root[0], "🔄 CI running") {
		t.Fatalf("root after the window = %v, want one redraw", root)
	}
}

func TestDMStatusCardRefsGoLiveThroughTheDaemonPoller(t *testing.T) {
	_, srv := serveRefs(t)
	l := startDaemon(t, func(a *app) {
		a.cards.client = refClient{http: srv.Client(), githubURL: srv.URL, buildkiteURL: srv.URL, now: time.Now}
	})
	loggedInWithRefs(t, l)
	l.do(opDMStatus, map[string]any{"card": statusCard{Status: "progress", Title: "Landing", Refs: []cardLink{{URL: testPRURL}}}})
	root := l.slack.sent()[0].ts
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, b := range editsOf(l.slack.edited(), root) {
			if strings.Contains(b, "🔄 CI running") {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("root edits %v never showed the polled PR state", editsOf(l.slack.edited(), root))
}

func richCard(at time.Time) statusCard {
	eta := at.Add(30 * time.Minute)
	started := at.Add(-72 * time.Minute)
	expires := at.Add(time.Hour)
	lanes := make([]rollupLane, cardMaxRollup)
	prs := make([]rollupPR, cardMaxRollup)
	for i := range lanes {
		lanes[i] = rollupLane{Name: fmt.Sprintf("lane-%02d", i), Status: "running", Detail: strings.Repeat("d", cardMaxDetailLen)}
		prs[i] = rollupPR{URL: fmt.Sprintf("https://github.com/acme/app/pull/%d", 100+i), Status: "open"}
	}
	c := statusCard{
		Status: "progress", Title: "Landing the stack", Body: "Three of ten PRs are in.", Drive: "stack-landing", Phase: "landing",
		Links:   slices.Repeat([]cardLink{{Label: "Dashboard", URL: "https://example.com/d"}}, cardMaxLinks),
		Options: []string{"a", "b", "c", "d"}, Progress: &cardProgress{Step: 3, Total: 10, Label: "PRs landed"}, StartedAt: &started, ETA: &eta,
		Facts:  slices.Repeat([]cardFact{{Label: "Queue", Value: "4 waiting"}}, cardMaxFacts),
		Rollup: &cardRollup{Lanes: lanes, PRs: prs},
	}
	for i := range cardMaxRefs {
		c.Refs = append(c.Refs, cardLink{URL: fmt.Sprintf("https://github.com/acme/app/pull/%d", i+1)})
	}
	for i := range cardMaxAsks {
		c.Asks = append(c.Asks, cardAsk{ID: fmt.Sprintf("merge-%d", i), Label: fmt.Sprintf("Merge #%d", i), Description: "green and approved",
			Style: "primary", Confirm: "Merge it now?", ExpiresAt: &expires})
	}
	return c
}

func TestRichCardRendersEveryPartWithinSlackLimits(t *testing.T) {
	at := time.Date(2026, 10, 22, 17, 0, 0, 0, time.UTC)
	card := richCard(at)
	if err := card.check(at, false, ""); err != nil {
		t.Fatalf("a maxed-out card: %v", err)
	}
	thread := &liveThread{Channel: "D1", Thread: "1.1"}
	for i := range 3 * cardMaxAsks {
		ask := card
		ask.Asks = []cardAsk{{ID: fmt.Sprintf("extra-%d", i), Label: "Hold"}}
		thread.apply(ask, cardTag{Repo: "app", Branch: "dev"}, at.Add(time.Duration(i)*time.Minute), true)
	}
	thread.apply(card, cardTag{Repo: "app", Branch: "dev"}, at, true)
	thread.apply(card, cardTag{Repo: "app", Branch: "dev"}, at, false)
	refs := map[string]refState{"pr:acme/app#1": {State: "open", Checks: "passed"}}
	_, root := thread.rootMessage(at, canvas{refs: refs})
	_, reply := thread.replyMessage(thread.Reply, at, canvas{refs: refs})
	if len(root) > 50 || len(reply) > 50 {
		t.Fatalf("root has %d blocks, reply %d; Slack takes at most 50", len(root), len(reply))
	}
	got := blocksJSON(t, root)
	for _, part := range []string{"`▰▰▰▱▱▱▱▱▱▱` *3/10* PRs landed", "running 1h 12m", "ETA 10:30am", "*Queue*\\n4 waiting", "✅ CI passed",
		"*Lanes* (50)", "more\"", "more open asks"} {
		if !strings.Contains(got, part) {
			t.Fatalf("root card lacks %q: %s", part, got)
		}
	}
	for _, b := range root {
		if s, ok := b.(*slack.SectionBlock); ok && s.Text != nil && runes(s.Text.Text) > cardMaxSectionLen {
			t.Fatalf("section %q runs %d characters past Slack's %d", s.BlockID, runes(s.Text.Text), cardMaxSectionLen)
		}
	}
	if timeline := blocksJSON(t, timelineBlock(thread.Timeline, at)); strings.Count(timeline, "Landing the stack") != cardMaxTimeline {
		t.Fatalf("timeline = %s, want the last %d updates", timeline, cardMaxTimeline)
	}
}

func askButton(t *testing.T, blocks []slack.Block, id string) *slack.ButtonBlockElement {
	t.Helper()
	for _, b := range blocks {
		if s, ok := b.(*slack.SectionBlock); ok && s.BlockID == cardAskBlock+id && s.Accessory != nil && s.Accessory.ButtonElement != nil {
			return s.Accessory.ButtonElement
		}
	}
	return nil
}

func TestAskButtonsCarryTheirIDConfirmAndExpiry(t *testing.T) {
	at := time.Date(2026, 10, 22, 17, 0, 0, 0, time.UTC)
	expires := at.Add(time.Hour)
	card := statusCard{Status: "needs_you", Title: "Merge #41?", Asks: []cardAsk{{ID: "merge-41", Label: "Merge #41", Style: "danger",
		Confirm: "Merge #41 at abc123?", ExpiresAt: &expires}}}
	b := askButton(t, card.replyBlocks(cardView{at: at, now: at}), "merge-41")
	if b == nil || b.ActionID != cardAskAction+"merge-41" || b.Style != slack.StyleDanger || b.Confirm == nil || b.Confirm.Text.Text != "Merge #41 at abc123?" {
		t.Fatalf("ask button = %+v, want the stable id, danger style, and a confirm dialog", b)
	}
	var k askTap
	if err := json.Unmarshal([]byte(b.Value), &k); err != nil || !reflect.DeepEqual(k, askTap{ID: "merge-41", Label: "Merge #41", Expires: expires.Unix()}) {
		t.Fatalf("button value %q decodes to %+v, %v", b.Value, k, err)
	}
	answered := blocksJSON(t, card.replyBlocks(cardView{at: at, now: at, answers: map[string]askAnswer{"merge-41": {At: at}}}))
	if !strings.Contains(answered, "✓ *Merge #41* · answered 10:00am") || strings.Contains(answered, cardAskAction) {
		t.Fatalf("an answered ask renders %s", answered)
	}
	expired := blocksJSON(t, card.replyBlocks(cardView{at: at, now: expires}))
	if !strings.Contains(expired, "⌛ *Merge #41* · expired 11:00am") || strings.Contains(expired, cardAskAction) {
		t.Fatalf("an expired ask renders %s", expired)
	}
}

func TestRootKeepsStickyStateAndAnsweredAsksStayAnswered(t *testing.T) {
	at := time.Date(2026, 10, 22, 17, 0, 0, 0, time.UTC)
	tag := cardTag{Repo: "app", Branch: "dev"}
	thread := &liveThread{Channel: "D1", Thread: "1.1"}
	thread.apply(statusCard{Status: "progress", Title: "Started", Progress: &cardProgress{Step: 1, Total: 4},
		Rollup: &cardRollup{Lanes: []rollupLane{{Name: "w1"}}}, Asks: []cardAsk{{ID: "go", Label: "Go"}}}, tag, at, true)
	thread.apply(statusCard{Status: "info", Title: "Halfway"}, tag, at.Add(time.Minute), true)
	root := thread.Root.Card
	if root.Title != "Halfway" || root.Progress == nil || root.Rollup == nil || len(thread.Timeline) != 2 {
		t.Fatalf("root after a bare card = %+v, timeline %v; want the latest title over the earlier progress and rollup", root, thread.Timeline)
	}
	thread.apply(statusCard{Rollup: &cardRollup{Lanes: []rollupLane{{Name: "w2"}}}}, tag, at.Add(2*time.Minute), true)
	if thread.Root.Card.Title != "Halfway" || thread.Root.Card.Rollup.Lanes[0].Name != "w2" || len(thread.Timeline) != 2 || thread.Reply != nil {
		t.Fatalf("a root-only rollup gave root %+v, reply %+v, timeline %v", thread.Root.Card, thread.Reply, thread.Timeline)
	}
	thread.Answers = map[string]askAnswer{"go": {At: at}}
	thread.apply(statusCard{Status: "needs_you", Title: "Again", Asks: []cardAsk{{ID: "go", Label: "Go"}}}, tag, at.Add(3*time.Minute), false)
	if open := thread.openAsks(at.Add(3 * time.Minute)); len(open) != 0 {
		t.Fatalf("re-posting an answered ask reopened it: %+v", open)
	}
	if thread.Root.Card.Title != "Halfway" || thread.Reply.Card.Title != "Again" {
		t.Fatalf("a plain card gave root %+v, reply %+v; want the root untouched", thread.Root.Card, thread.Reply.Card)
	}
}

func (l *liveDaemon) channelPayloads() []string {
	l.t.Helper()
	if _, err := l.srv.Append(l.ctx, &event.Event{
		SubjectID: l.subID, Origin: event.OriginSystem, Type: sentinelType, Payload: json.RawMessage(`{"type":"` + sentinelType + `"}`),
	}); err != nil {
		l.t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(l.ctx, 10*time.Second)
	defer cancel()
	var payloads []string
	err := consume.ConsumeEvents(ctx, consume.StreamSource{
		Port: l.port, SubjectID: l.subID, Consumer: channelConsumer, ExcludeOrigin: event.OriginAgent, Paths: appPaths(),
	}, func(_ int64, data string) (bool, error) {
		payloads = append(payloads, data)
		return strings.Contains(data, sentinelType), nil
	})
	if err != nil {
		l.t.Fatal(err)
	}
	return payloads
}

func TestCardAskTapSteersWithItsIDAndRedrawsTheThread(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	expires := time.Now().Add(time.Hour)
	ask := cardAsk{ID: "merge-41", Label: "Merge #41", Confirm: "Merge #41 now?", ExpiresAt: &expires}
	l.do(opDMStatus, map[string]any{"card": statusCard{Status: "needs_you", Title: "Merge #41?", Asks: []cardAsk{ask}}})
	sent := l.slack.sent()
	asked := sent[0].ts
	if len(sent) != 1 || !strings.Contains(sent[0].blocks, cardAskAction+"merge-41") {
		t.Fatalf("posts %+v: want one card carrying the open ask", sent)
	}
	id, err := loadIdentity(l.ctx, l.srv.DB())
	if err != nil {
		t.Fatal(err)
	}
	push := cardActionPush{User: testUser, Channel: id.DMChannelID, MessageTS: asked, ThreadTS: asked, ActionID: cardAskAction + "merge-41",
		Value: ask.value(), ActionTS: fmt.Sprintf("%d.000100", time.Now().Unix()), Seq: "001800000100000-abcd0124"}
	l.tap(id, push)
	if payloads := strings.Join(l.channelPayloads(), "\n"); !strings.Contains(payloads, `"id":"merge-41"`) || !strings.Contains(payloads, eventAction) {
		t.Fatalf("channel payloads %s, want a slack.action carrying the ask id", payloads)
	}
	if got := l.inject(agent.TopLevel); !strings.Contains(got, `answered your ask "merge-41" with "Merge #41"`) {
		t.Fatalf("directive = %q, want the ask's id and label", got)
	}
	if edits := editsOf(l.slack.edited(), asked); len(edits) != 1 || !strings.Contains(edits[0], "✓ *Merge #41* · answered") {
		t.Fatalf("card edits %v: want the ask answered on its card", edits)
	}

	late := cardAsk{ID: "hold-41", Label: "Hold #41", ExpiresAt: &expires}
	l.do(opDMStatus, map[string]any{"card": statusCard{Status: "needs_you", Title: "Hold?", Asks: []cardAsk{late}}})
	push.MessageTS, push.ActionID, push.Value = l.slack.sent()[1].ts, cardAskAction+"hold-41", late.value()
	push.ThreadTS = push.MessageTS
	push.ActionTS, push.Seq = fmt.Sprintf("%d.000100", expires.Add(time.Minute).Unix()), "001800000100000-abcd0125"
	l.tap(id, push)
	if got := l.inject(agent.TopLevel); got != "" {
		t.Fatalf("a tap after the ask expired steered %q", got)
	}
}

func TestWaitingCardTapCarriesTheQuestionAndAnswersOnce(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	card := statusCard{Status: "needs_you", Title: "Merge #41 now?", Options: []string{"Merge", "Hold"}, Recommended: "Merge", Waits: "step-3:merge"}
	l.do(opDMStatus, map[string]any{"card": card})
	reply := l.slack.sent()[0]
	if !strings.Contains(reply.blocks, cardWaitAction+"0:step-3:merge") || !strings.Contains(reply.blocks, cardExplainAction+"step-3:merge") ||
		strings.Contains(reply.blocks, cardAckAction) {
		t.Fatalf("reply %s, want the answers and Explain more as buttons and no Ack", reply.blocks)
	}
	id, err := loadIdentity(l.ctx, l.srv.DB())
	if err != nil {
		t.Fatal(err)
	}
	push := cardActionPush{User: testUser, Channel: id.DMChannelID, MessageTS: reply.ts, ThreadTS: reply.ts,
		ActionID: cardExplainAction + "step-3:merge", Value: string(mustJSON(askTap{ID: card.Waits, Label: cardExplainLabel, Question: card.Title})),
		ActionTS: fmt.Sprintf("%d.000050", time.Now().Unix()), Seq: "001800000100000-abcd0125"}
	l.tap(id, push)
	if got := l.inject(agent.TopLevel); !strings.Contains(got, `explain the waiting ask "step-3:merge" more`) || !strings.Contains(got, "stays stopped") {
		t.Fatalf("directive = %q, want an explain request that keeps the lane stopped", got)
	}
	edits := l.slack.edited()
	if len(edits) == 0 || edits[0].ts != reply.ts || !strings.Contains(edits[0].blocks, "You asked for more") || !strings.Contains(edits[0].blocks, cardWaitAction) {
		t.Fatalf("edits %+v, want the tapped reply redrawn first, still offering its answers", edits)
	}
	if _, done := l.app.cards.answer(id.DMChannelID, reply.ts, card.Waits); done {
		t.Fatal("Explain more answered the waiting ask")
	}

	if !strings.Contains(reply.blocks, `\"o\":[\"Merge\",\"Hold\"]`) {
		t.Fatalf("reply %s, want each answer button to carry the card's options", reply.blocks)
	}
	push.ActionID, push.Value = cardWaitAction+"0:step-3:merge", string(mustJSON(askTap{ID: card.Waits, Label: "Merge", Question: card.Title, Options: card.Options}))
	push.ActionTS, push.Seq = fmt.Sprintf("%d.000100", time.Now().Unix()), "001800000100000-abcd0126"
	l.tap(id, push)
	var tap map[string]any
	for _, raw := range l.channelPayloads() {
		var got map[string]any
		if err := json.Unmarshal([]byte(raw), &got); err != nil {
			t.Fatal(err)
		}
		if got["type"] == eventAction && got["label"] == "Merge" {
			tap = got
		}
	}
	raw, err := os.ReadFile(filepath.Join("..", "..", "plugin", "capt-hook", "hooks", "fixtures", "slack_action.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture map[string]any
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	if keys, want := slices.Sorted(maps.Keys(tap)), slices.Sorted(maps.Keys(fixture)); !slices.Equal(keys, want) {
		t.Fatalf("tap payload %v has keys %v, want %v, the keys of the tap the permission hook's tests read", tap, keys, want)
	}
	if tap["id"] != "step-3:merge" || tap["waits"] != true {
		t.Fatalf("tap payload = %v, want the card id and waits", tap)
	}
	question, _ := tap["question"].(string)
	options, _ := tap["options"].([]any)
	if question != "Merge #41 now?" || !slices.Equal(options, []any{"Merge", "Hold"}) || !slices.Contains(options, tap["label"]) {
		t.Fatalf("tap payload = %v, want the question, every option as a string, and the label among them: capt-hook's card_answer reads a tap as his answer only then", tap)
	}
	if got := l.inject(agent.TopLevel); !strings.Contains(got, `answered the waiting ask "step-3:merge" with "Merge"`) || !strings.Contains(got, "hand it to that lane") ||
		!strings.Contains(got, `answered "Merge #41 now?" with "Merge" at `) {
		t.Fatalf("directive = %q, want the waiting ask's id, answer, the hand-off, and the capture", got)
	}
	if edits := editsOf(l.slack.edited(), reply.ts); len(edits) != 2 || !strings.Contains(edits[1], "✓ *Merge* · answered") ||
		strings.Contains(edits[1], cardWaitAction) || strings.Contains(edits[1], "You asked for more") {
		t.Fatalf("reply edits %v, want the answer in place of the hint and buttons", edits)
	}

	push.ActionID, push.Value = cardWaitAction+"1:step-3:merge", string(mustJSON(askTap{ID: card.Waits, Label: "Hold", Question: card.Title}))
	push.ActionTS, push.Seq = fmt.Sprintf("%d.000200", time.Now().Unix()), "001800000100000-abcd0127"
	l.tap(id, push)
	if got := l.inject(agent.TopLevel); got != "" {
		t.Fatalf("a second tap on an answered waiting ask steered %q", got)
	}
}

func TestAQuestionCardTapAndItsUndoReachTheCardCommandAsHisAnswer(t *testing.T) {
	cmd := fakeCardCommand(t, nil)
	l, _ := prLinksDaemon(t, map[string]any{"card_command": cmd.argv})
	card := statusCard{Status: "needs_you", Title: "Which store keeps the tally?", Waits: "demo:j3-661440", Options: []string{"Ring", "Table"}, Recommended: "Ring"}
	l.do(opDMStatus, map[string]any{"card": card})
	reply := l.slack.sent()[0]
	id, err := loadIdentity(l.ctx, l.srv.DB())
	if err != nil {
		t.Fatal(err)
	}
	picked, undone := time.Now(), time.Now().Add(time.Minute)
	l.tap(id, cardActionPush{User: testUser, Channel: id.DMChannelID, MessageTS: reply.ts, ThreadTS: reply.ts,
		ActionID: cardWaitAction + "0:" + card.Waits, Value: string(mustJSON(askTap{ID: card.Waits, Label: "Ring", Question: card.Title, Options: card.Options})),
		ActionTS: fmt.Sprintf("%d.000100", picked.Unix()), Seq: "001800000100000-abcd0400"})
	l.tap(id, cardActionPush{User: testUser, Channel: id.DMChannelID, MessageTS: reply.ts, ThreadTS: reply.ts,
		ActionID: cardUndoAction + card.Waits, ActionTS: fmt.Sprintf("%d.000900", undone.Unix()), Seq: "001800000100000-abcd0401"})
	answer := func(at string, undone bool) string {
		return string(mustJSON(map[string]any{"answer": map[string]any{
			"waits": card.Waits, "label": "Ring", "other": false, "undone": undone, "url": id.permalink(id.DMChannelID, reply.ts, reply.ts),
			"action_ts": at, "source": "pressed the button",
		}}))
	}
	want := []string{answer(fmt.Sprintf("%d.000100", picked.Unix()), false), answer(fmt.Sprintf("%d.000900", undone.Unix()), true)}
	if got := answersOf(cmd.requests(t)); !slices.Equal(got, want) {
		t.Fatalf("card command asked %v, want his pick and then its undo, each with the card's waits id and link", got)
	}
}

func TestHisWordsUnderAWaitingDriveCardReachTheSessionWithEyesAndLeaveItOpen(t *testing.T) {
	cmd := fakeCardCommand(t, nil)
	l, _ := prLinksDaemon(t, map[string]any{"card_command": cmd.argv})
	id, err := loadIdentity(l.ctx, l.srv.DB())
	if err != nil {
		t.Fatal(err)
	}
	card := statusCard{Status: "needs_you", Title: "Which store keeps the tally?", Drive: "demo", Waits: "demo:j3-661440", Options: []string{"Ring", "Table"}, Recommended: "Ring"}
	var posted map[string]string
	if err := json.Unmarshal(l.do(opDMStatus, map[string]any{"card": card}).Body, &posted); err != nil {
		t.Fatal(err)
	}
	thread := posted["ts"]
	steered := ""
	say := func(n int, text string) string {
		t.Helper()
		ts := fmt.Sprintf("%d.%06d", slackTime(thread).Unix()+1, n)
		l.slack.add(id.DMChannelID, slack.Message{Msg: slack.Msg{Timestamp: ts, ThreadTimestamp: thread, User: testUser, Text: text}})
		l.app.poller.nudged(messageNudge{Channel: id.DMChannelID, TS: ts, ThreadTS: thread})
		steered = ""
		eventually(t, "his words to reach the session", func() bool {
			steered += l.inject(agent.TopLevel)
			return steered != ""
		})
		return ts
	}
	workingLines := func() int {
		l.slack.mu.Lock()
		defer l.slack.mu.Unlock()
		return len(slices.DeleteFunc(slices.Clone(l.slack.statuses), func(s slack.AssistantThreadsSetStatusParameters) bool {
			return s.ThreadTS != thread || s.Status != workingStatus
		}))
	}

	for n, words := range []string{"And what would the light run version look like?", "table"} {
		ts := say(n+1, words)
		if strings.Contains(steered, "answered the waiting ask") || !strings.Contains(steered, words) {
			t.Fatalf("directive = %q, want %q delivered as his message", steered, words)
		}
		if got := l.slack.reactedTo(ts); !slices.Equal(got, []string{pickupReaction}) {
			t.Fatalf("reactions on %q = %v, want eyes", words, got)
		}
		if got := workingLines(); got != n+1 {
			t.Fatalf("working lines under the card's thread = %d after %q, want one per reply", got, words)
		}
		if _, done := l.app.cards.answer(id.DMChannelID, thread, card.Waits); done {
			t.Fatalf("%q answered the card, want it still waiting", words)
		}
	}
	if edits := editsOf(l.slack.edited(), thread); slices.ContainsFunc(edits, func(e string) bool { return strings.Contains(e, "answered") || strings.Contains(e, cardUndoAction) }) {
		t.Fatalf("card edits %v, want the card still offering its options", edits)
	}
	if got := answersOf(cmd.requests(t)); len(got) != 0 {
		t.Fatalf("card command asked %v, want no answer recorded from his words", got)
	}

	picked := say(3, "2")
	if !strings.Contains(steered, `answered the waiting ask "demo:j3-661440" with "Table"`) {
		t.Fatalf("directive = %q, want a bare number taken as that option", steered)
	}
	if got := l.slack.reactedTo(picked); !slices.Equal(got, []string{pickupReaction}) {
		t.Fatalf("reactions on his typed number = %v, want eyes", got)
	}
	want := []string{string(mustJSON(map[string]any{"answer": map[string]any{
		"waits": card.Waits, "label": "Table", "other": false, "undone": false, "url": id.permalink(id.DMChannelID, thread, thread),
		"action_ts": picked, "source": "typed its number in the thread",
	}}))}
	eventually(t, "the card command to record the typed number", func() bool { return len(answersOf(cmd.requests(t))) == len(want) })
	if got := answersOf(cmd.requests(t)); !slices.Equal(got, want) {
		t.Fatalf("card command asked %v, want the typed number recorded once", got)
	}
}

func TestCardReopenUndoesAnAnswerTakenFromHisReplyInTheCardsThread(t *testing.T) {
	cmd := fakeCardCommand(t, nil)
	l, _ := prLinksDaemon(t, map[string]any{"card_command": cmd.argv})
	id, err := loadIdentity(l.ctx, l.srv.DB())
	if err != nil {
		t.Fatal(err)
	}
	card := statusCard{Status: "needs_you", Title: "Which store keeps the tally?", Drive: "demo", Waits: "demo:j3-661442", Options: []string{"Ring", "Table"}, Recommended: "Ring"}
	var posted map[string]string
	if err := json.Unmarshal(l.do(opDMStatus, map[string]any{"card": card}).Body, &posted); err != nil {
		t.Fatal(err)
	}
	thread := posted["ts"]
	cardURL := id.permalink(id.DMChannelID, thread, thread)
	reply := func(n int, words string) string {
		ts := fmt.Sprintf("%d.%06d", slackTime(thread).Unix()+1, n)
		l.slack.add(id.DMChannelID, slack.Message{Msg: slack.Msg{Timestamp: ts, ThreadTimestamp: thread, User: testUser, Text: words}})
		return id.permalink(id.DMChannelID, ts, thread)
	}
	question := "And what would the light run version look like?"
	asked, other := reply(1, question), reply(2, "Ring, but only on staging")
	if res := l.do(opCardAnswer, map[string]any{"url": cardURL, "label": question, "other": true, "via": asked}); !res.OK {
		t.Fatalf("card-answer: %s", res.Error)
	}

	if res := l.do(opCardReopen, map[string]any{"url": cardURL, "via": other}); res.OK || !strings.Contains(res.Error, "is not answered with the words of") {
		t.Fatalf("card-reopen with a reply the card does not show = %+v, want a refusal", res)
	}
	if _, done := l.app.cards.answer(id.DMChannelID, thread, card.Waits); !done {
		t.Fatal("a refused card-reopen reopened the card")
	}

	if res := l.do(opCardReopen, map[string]any{"url": cardURL, "via": asked}); !res.OK {
		t.Fatalf("card-reopen: %s", res.Error)
	}
	if _, done := l.app.cards.answer(id.DMChannelID, thread, card.Waits); done {
		t.Fatal("card-reopen left the card answered")
	}
	if edits := editsOf(l.slack.edited(), thread); len(edits) == 0 || !strings.Contains(edits[len(edits)-1], cardWaitAction) || strings.Contains(edits[len(edits)-1], question) {
		t.Fatalf("card edits %v, want the card offering its options again", edits)
	}
	want := []string{string(mustJSON(map[string]any{"answer": map[string]any{
		"waits": card.Waits, "label": question, "other": true, "undone": true, "url": cardURL,
		"action_ts": fmt.Sprintf("%d.%06d", slackTime(thread).Unix()+1, 1), "source": "wrote in the card's thread",
	}}))}
	eventually(t, "the card command to record the reopened answer", func() bool { return len(answersOf(cmd.requests(t))) == len(want) })
	if got := answersOf(cmd.requests(t)); !slices.Equal(got, want) {
		t.Fatalf("card command asked %v, want his reply recorded as undone once", got)
	}
}

func TestUndoReopensAStrayPickForTenMinutes(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	card := statusCard{Status: "needs_you", Title: "What should the runner keep for each statement?", Waits: "durable-agents:j242-657927-runner",
		Options: []string{"One tally table, a typed row per statement and step", "Statement rows in the key-value table, step totals summed from them",
			"Keep nothing extra and count calls each time"}, Recommended: "One tally table, a typed row per statement and step"}
	l.do(opDMStatus, map[string]any{"card": card})
	reply := l.slack.sent()[0]
	id, err := loadIdentity(l.ctx, l.srv.DB())
	if err != nil {
		t.Fatal(err)
	}
	explained := time.Now()
	picked := explained.Add(54 * time.Second)
	pick := func(n int, at time.Time, seq string) {
		l.tap(id, cardActionPush{User: testUser, Channel: id.DMChannelID, MessageTS: reply.ts, ThreadTS: reply.ts,
			ActionID: fmt.Sprintf("%s%d:%s", cardWaitAction, n, card.Waits),
			Value:    string(mustJSON(askTap{ID: card.Waits, Label: card.Options[n], Question: card.Title, Options: card.Options})),
			ActionTS: fmt.Sprintf("%d.%06d", at.Unix(), n), Seq: seq})
	}
	undo := func(at time.Time, seq string) {
		l.tap(id, cardActionPush{User: testUser, Channel: id.DMChannelID, MessageTS: reply.ts, ThreadTS: reply.ts,
			ActionID: cardUndoAction + card.Waits, ActionTS: fmt.Sprintf("%d.000900", at.Unix()), Seq: seq})
	}
	lastEdit := func() string {
		edits := editsOf(l.slack.edited(), reply.ts)
		if len(edits) == 0 {
			t.Fatal("the card was never redrawn")
		}
		return edits[len(edits)-1]
	}

	undoneEvents := func() []map[string]any {
		var undone []map[string]any
		for _, raw := range l.channelPayloads() {
			var got map[string]any
			if err := json.Unmarshal([]byte(raw), &got); err != nil {
				t.Fatal(err)
			}
			if got["type"] == eventAction && got["undo"] == true {
				undone = append(undone, got)
			}
		}
		return undone
	}

	l.tap(id, cardActionPush{User: testUser, Channel: id.DMChannelID, MessageTS: reply.ts, ThreadTS: reply.ts,
		ActionID: cardExplainAction + card.Waits, Value: string(mustJSON(askTap{ID: card.Waits, Label: cardExplainLabel, Question: card.Title})),
		ActionTS: fmt.Sprintf("%d.000050", explained.Unix()), Seq: "001800000100000-abcd0200"})
	if got := l.inject(agent.TopLevel); !strings.Contains(got, "stays stopped") {
		t.Fatalf("directive = %q, want Explain more to keep the lane stopped", got)
	}
	if drawn := lastEdit(); strings.Contains(drawn, cardUndoAction) {
		t.Fatalf("card %s, want no Undo before any answer", drawn)
	}

	pick(2, picked, "001800000100000-abcd0201")
	if got := l.inject(agent.TopLevel); !strings.Contains(got, `with "Keep nothing extra and count calls each time"`) {
		t.Fatalf("directive = %q, want the stray pick handed over", got)
	}
	if drawn := lastEdit(); !strings.Contains(drawn, "✓ *Keep nothing extra and count calls each time* · answered") || !strings.Contains(drawn, cardUndoAction+card.Waits) {
		t.Fatalf("card %s, want the pick marked with an Undo button beside it", drawn)
	}

	undo(picked.Add(time.Minute), "001800000100000-abcd0202")
	events := undoneEvents()
	if len(events) != 1 {
		t.Fatalf("undo events %v, want exactly one", events)
	}
	undone := events[0]
	if undone["id"] != card.Waits || undone["label"] != card.Options[2] || undone["question"] != card.Title {
		t.Fatalf("undo payload = %v, want the waiting ask, the undone answer and the question", undone)
	}
	raw, err := os.ReadFile(filepath.Join("..", "..", "plugin", "capt-hook", "hooks", "fixtures", "slack_action.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture map[string]any
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	delete(fixture, "capture")
	delete(fixture, "options")
	fixture["undo"] = true
	if keys, want := slices.Sorted(maps.Keys(undone)), slices.Sorted(maps.Keys(fixture)); !slices.Equal(keys, want) {
		t.Fatalf("undo payload %v has keys %v, want %v: a tap's keys with no options or capture, so capt-hook's card_answer never reads it as an answer", undone, keys, want)
	}
	long := maps.Clone(undone)
	long["question"] = strings.Repeat("q", monitorEvent)
	line, err := fitted(string(mustJSON(long)))
	if err != nil {
		t.Fatal(err)
	}
	var shown map[string]any
	if err := json.Unmarshal([]byte(line), &shown); err != nil {
		t.Fatal(err)
	}
	if shown["undo"] != true || shown["id"] != card.Waits {
		t.Fatalf("Monitor line %s, want undo and the waiting ask kept", line)
	}
	if got := l.inject(agent.TopLevel); !strings.Contains(got, `undid their answer "Keep nothing extra and count calls each time"`) || !strings.Contains(got, "treat it as never given") {
		t.Fatalf("directive = %q, want the undo handed over as an answer never given", got)
	}
	if drawn := lastEdit(); !strings.Contains(drawn, fmt.Sprintf("%s0:%s", cardWaitAction, card.Waits)) || strings.Contains(drawn, "✓ *") || strings.Contains(drawn, cardUndoAction) {
		t.Fatalf("card %s, want its Pick buttons back with no answer and no Undo", drawn)
	}
	if _, done := l.app.cards.answer(id.DMChannelID, reply.ts, card.Waits); done {
		t.Fatal("the undone answer is still on record")
	}

	repicked := picked.Add(2 * time.Minute)
	pick(0, repicked, "001800000100000-abcd0203")
	if got := l.inject(agent.TopLevel); !strings.Contains(got, `with "One tally table, a typed row per statement and step"`) {
		t.Fatalf("directive = %q, want the new pick taken", got)
	}

	undo(repicked.Add(11*time.Minute), "001800000100000-abcd0204")
	if got := l.inject(agent.TopLevel); got != "" {
		t.Fatalf("an Undo past its ten minutes steered %q", got)
	}
	if ans, _ := l.app.cards.answer(id.DMChannelID, reply.ts, card.Waits); ans.Label != card.Options[0] {
		t.Fatalf("answer = %+v, want the new pick to stand", ans)
	}
	if drawn := lastEdit(); !strings.Contains(drawn, "had passed") || strings.Contains(drawn, cardUndoAction) {
		t.Fatalf("card %s, want the late Undo refused on the card and its button gone", drawn)
	}
	if events := undoneEvents(); len(events) != 0 {
		t.Fatalf("undo events %v after the first, want none", events)
	}
}

func pickFixture(t *testing.T) (*fixture, statusCard, string) {
	t.Helper()
	f := newFixture(t)
	thread := postDMThread(t, f)
	if err := f.app.openSessionThread(f.ctx, f.db(), subjectFor(t, f), f.id.DMChannelID, thread); err != nil {
		t.Fatal(err)
	}
	card := statusCard{Status: "needs_you", Title: "How should a subagent chat get one runner?",
		Options: []string{"One door for every chat", "A lock per chat", "Leave it as it is"}, Waits: "one-runner"}
	f.app.cards.threads[threadKey(f.id.DMChannelID, thread)] = &liveThread{Channel: f.id.DMChannelID, Thread: thread,
		Waiting: map[string]*liveMessage{card.Waits: {TS: thread, Card: card}}}
	return f, card, thread
}

func pick(f *fixture, card statusCard, thread string, n int) error {
	return f.poller.cardAction(f.ctx, f.id, cardActionPush{User: testUser, Channel: f.id.DMChannelID, MessageTS: thread, ThreadTS: thread,
		ActionID: fmt.Sprintf("%s%d:%s", cardWaitAction, n, card.Waits),
		Value:    string(mustJSON(askTap{ID: card.Waits, Label: card.Options[n], Question: card.Title, Options: card.Options})),
		ActionTS: fmt.Sprintf("%d.%06d", f.now.Unix(), n)})
}

func TestAPickShowsOnItsCardBeforeTheLaneTakesIt(t *testing.T) {
	f, card, thread := pickFixture(t)
	f.plane.directErr = errors.New("the lane is mid-turn")
	if err := pick(f, card, thread, 0); err == nil {
		t.Fatal("the failed hand-off to the lane reported nothing")
	}
	edits := editsOf(f.slack.edited(), thread)
	if len(edits) != 1 || !strings.Contains(edits[0], "✓ *One door for every chat* · answered") || strings.Contains(edits[0], cardWaitAction) {
		t.Fatalf("card edits %v, want the pick marked and its buttons gone whatever the lane is doing", edits)
	}
}

func TestARepeatPickLeavesACorrectPostAloneAndHandsOverNothing(t *testing.T) {
	f, card, thread := pickFixture(t)
	if err := pick(f, card, thread, 0); err != nil {
		t.Fatal(err)
	}
	handed := len(f.plane.directives)
	if err := pick(f, card, thread, 1); err != nil {
		t.Fatal(err)
	}
	if len(f.plane.directives) != handed {
		t.Fatalf("the repeat pick reached the lane: %+v", f.plane.directives[handed:])
	}
	if edits := editsOf(f.slack.edited(), thread); len(edits) != 1 {
		t.Fatalf("card edits %v, want the post that already shows his answer left alone", edits)
	}
}

func TestARepeatPickRestoresAPostThatStillShowsItsButtonsAndThenLeavesItAlone(t *testing.T) {
	f, card, thread := pickFixture(t)
	f.slack.fail("chat.update", errors.New("slack is down"))
	f.slack.fail(methodReplies, errors.New("slack is down"))
	if err := pick(f, card, thread, 0); err != nil {
		t.Fatal(err)
	}
	if edits := editsOf(f.slack.edited(), thread); len(edits) != 0 {
		t.Fatalf("card edits %v, want none while Slack refuses them", edits)
	}
	handed := len(f.plane.directives)
	if err := pick(f, card, thread, 1); err != nil {
		t.Fatal(err)
	}
	edits := editsOf(f.slack.edited(), thread)
	if len(edits) != 1 || !strings.Contains(edits[0], "✓ *One door for every chat* · answered") || strings.Contains(edits[0], cardWaitAction) {
		t.Fatalf("card edits %v, want the repeat pick to restore the first answer", edits)
	}
	if err := pick(f, card, thread, 2); err != nil {
		t.Fatal(err)
	}
	if again := editsOf(f.slack.edited(), thread); len(again) != 1 {
		t.Fatalf("card edits %v, want a restored post left alone", again)
	}
	if len(f.plane.directives) != handed {
		t.Fatalf("a repeat pick reached the lane: %+v", f.plane.directives[handed:])
	}
}

func TestABareNumberReplyAnswersTheWaitingCardAsItsButtonWould(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	options := []string{"Cap the run search at 200 sessions", "Run the whole search tonight"}
	card := statusCard{Status: "needs_you", Title: "How should the run search pay for itself?", Options: options, Recommended: options[0], Waits: "cost-pick"}
	l.do(opDMStatus, map[string]any{"card": card})
	reply := l.slack.sent()[0]
	id, err := loadIdentity(l.ctx, l.srv.DB())
	if err != nil {
		t.Fatal(err)
	}
	var steered string
	say := func(n int, text string) {
		t.Helper()
		ts := fmt.Sprintf("%d.%06d", slackTime(reply.ts).Unix()+1, n)
		l.slack.add(id.DMChannelID, slack.Message{Msg: slack.Msg{Timestamp: ts, ThreadTimestamp: reply.ts, User: testUser, Text: text}})
		l.app.poller.nudged(messageNudge{Channel: id.DMChannelID, TS: ts, ThreadTS: reply.ts})
		steered = ""
		eventually(t, "the reply to reach the session", func() bool {
			steered += l.inject(agent.TopLevel)
			return steered != ""
		})
	}

	say(1, "+1")
	if _, done := l.app.cards.answer(id.DMChannelID, reply.ts, card.Waits); strings.Contains(steered, "answered the waiting ask") || done {
		t.Fatalf("+1 answered the card: %q", steered)
	}
	say(2, " 2 ")
	if !strings.Contains(steered, `answered the waiting ask "cost-pick" with "Run the whole search tonight"`) {
		t.Fatalf("directive = %q, want the second listed option as the answer", steered)
	}
	if edits := editsOf(l.slack.edited(), reply.ts); len(edits) == 0 || !strings.Contains(edits[len(edits)-1], "✓ *Run the whole search tonight* · answered") ||
		strings.Contains(edits[len(edits)-1], cardWaitAction) {
		t.Fatalf("card edits %v, want the card marked answered", edits)
	}
	say(3, "1")
	if strings.Contains(steered, "answered the waiting ask") {
		t.Fatalf("a number after the answer answered again: %q", steered)
	}
}

func TestRedrawsKeepATappedOptionAndAWhoamiHidesRefTokens(t *testing.T) {
	at := time.Date(2026, 10, 22, 17, 0, 0, 0, time.UTC)
	thread := &liveThread{Channel: "D1", Thread: "1.1"}
	thread.apply(statusCard{Status: "needs_you", Title: "Ship?", Options: []string{"ship it"}}, cardTag{}, at, false)
	thread.Reply.Tapped = "✓ *ship it* · 10:00am"
	if _, reply := thread.replyMessage(thread.Reply, at, canvas{}); strings.Contains(blocksJSON(t, reply), cardOptionAction) || !strings.Contains(blocksJSON(t, reply), "✓ *ship it*") {
		t.Fatalf("a redrawn reply reoffers its tapped option: %s", blocksJSON(t, reply))
	}
	id := identity{Config: config{GitHub: &tokenSource{Token: "ghp-secret"}, Buildkite: &tokenSource{Command: []string{"/bin/bk-token"}}}}
	if got := string(mustJSON(newWhoami(id, ""))); strings.Contains(got, "ghp-secret") || !strings.Contains(got, "/bin/bk-token") {
		t.Fatalf("whoami = %s, want the token hidden and the command shown", got)
	}
	if id.Config.GitHub.Token != "ghp-secret" {
		t.Fatal("whoami redaction changed the stored identity")
	}
}

func TestRootOnlyPostsOneLiveRootAndRedrawsIt(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	rollup := func(lane string) map[string]any {
		return map[string]any{"root_only": true, "card": statusCard{Rollup: &cardRollup{Lanes: []rollupLane{{Name: lane, Status: "shipping"}}}}}
	}
	l.do(opDMStatus, rollup("w4"))
	sent := l.slack.sent()
	if len(sent) != 1 || sent[0].threadTS != "" || !strings.Contains(sent[0].blocks, "*w4* · shipping") {
		t.Fatalf("first root-only posts = %+v, want the rollup at the DM root", sent)
	}
	l.do(opDMStatus, cardBody("Started"))
	if len(l.slack.edited()) != 0 {
		t.Fatalf("a plain card edited %+v, want the root left alone", l.slack.edited())
	}
	l.do(opDMStatus, rollup("w5"))
	root, edits := sent[0].ts, l.slack.edited()
	if len(l.slack.sent()) != 2 || len(edits) != 1 || edits[0].ts != root || !strings.Contains(edits[0].blocks, "*w5* · shipping") {
		t.Fatalf("posts %+v, edits %+v: want the first root redrawn and no new message", l.slack.sent(), edits)
	}
}
