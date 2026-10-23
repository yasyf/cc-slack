package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
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
	mu      sync.Mutex
	pr      string
	build   string
	status  int
	header  http.Header
	queries []string
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
		_, _ = fmt.Fprintf(w, `{"state":%q}`, f.build)
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
	}
}

func seedLiveThread(l *liveCards, card statusCard, at time.Time) {
	t := &liveThread{Channel: "D" + testUser, Thread: "1800000000.000100"}
	t.apply(card, cardTag{Repo: "app", Branch: "dev"}, at, false)
	t.Reply.TS = "1800000000.000200"
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
	if wait := cards.tick(l.ctx); refs.calls() != calls || len(l.slack.edited()) != 2 || wait < 50*time.Second {
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
	if err := card.check(at, false); err != nil {
		t.Fatalf("a maxed-out card: %v", err)
	}
	thread := &liveThread{Channel: "D1", Thread: "1.1"}
	for i := range 3 * cardMaxAsks {
		ask := card
		ask.Asks = []cardAsk{{ID: fmt.Sprintf("extra-%d", i), Label: "Hold"}}
		thread.apply(ask, cardTag{Repo: "app", Branch: "dev"}, at.Add(time.Duration(i)*time.Minute), false)
	}
	thread.apply(card, cardTag{Repo: "app", Branch: "dev"}, at, false)
	refs := map[string]refState{"pr:acme/app#1": {State: "open", Checks: "passed"}}
	_, root := thread.rootMessage(at, refs)
	_, reply := thread.replyMessage(at, refs)
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
	if err := json.Unmarshal([]byte(b.Value), &k); err != nil || k != (askTap{ID: "merge-41", Label: "Merge #41", Expires: expires.Unix()}) {
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
		Rollup: &cardRollup{Lanes: []rollupLane{{Name: "w1"}}}, Asks: []cardAsk{{ID: "go", Label: "Go"}}}, tag, at, false)
	thread.apply(statusCard{Status: "info", Title: "Halfway"}, tag, at.Add(time.Minute), false)
	root := thread.Root.Card
	if root.Title != "Halfway" || root.Progress == nil || root.Rollup == nil || len(thread.Timeline) != 2 {
		t.Fatalf("root after a bare card = %+v, timeline %v; want the latest title over the earlier progress and rollup", root, thread.Timeline)
	}
	thread.apply(statusCard{Rollup: &cardRollup{Lanes: []rollupLane{{Name: "w2"}}}}, tag, at.Add(2*time.Minute), true)
	if thread.Root.Card.Title != "Halfway" || thread.Root.Card.Rollup.Lanes[0].Name != "w2" || len(thread.Timeline) != 2 || thread.Reply.Card.Title != "Halfway" {
		t.Fatalf("a root-only rollup gave root %+v, reply %+v, timeline %v", thread.Root.Card, thread.Reply.Card, thread.Timeline)
	}
	thread.Answers = map[string]askAnswer{"go": {At: at}}
	thread.apply(statusCard{Status: "needs_you", Title: "Again", Asks: []cardAsk{{ID: "go", Label: "Go"}}}, tag, at.Add(3*time.Minute), false)
	if open := thread.openAsks(at.Add(3 * time.Minute)); len(open) != 0 {
		t.Fatalf("re-posting an answered ask reopened it: %+v", open)
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
	root, reply := sent[0].ts, sent[1].ts
	if !strings.Contains(sent[0].blocks, cardAskAction+"merge-41") || !strings.Contains(sent[1].blocks, cardAskAction+"merge-41") {
		t.Fatalf("root %s and reply %s: want the open ask on both", sent[0].blocks, sent[1].blocks)
	}
	id, err := loadIdentity(l.ctx, l.srv.DB())
	if err != nil {
		t.Fatal(err)
	}
	push := cardActionPush{User: testUser, Channel: id.DMChannelID, MessageTS: root, ThreadTS: root, ActionID: cardAskAction + "merge-41",
		Value: ask.value(), ActionTS: fmt.Sprintf("%d.000100", time.Now().Unix()), Seq: "001800000100000-abcd0124"}
	l.tap(id, push)
	if got := l.inject(agent.TopLevel); !strings.Contains(got, `answered your ask "merge-41" with "Merge #41"`) {
		t.Fatalf("directive = %q, want the ask's id and label", got)
	}
	if payloads := strings.Join(l.channelPayloads(), "\n"); !strings.Contains(payloads, `"id":"merge-41"`) || !strings.Contains(payloads, eventAction) {
		t.Fatalf("channel payloads %s, want a slack.action carrying the ask id", payloads)
	}
	rootEdits, replyEdits := editsOf(l.slack.edited(), root), editsOf(l.slack.edited(), reply)
	if len(rootEdits) != 1 || strings.Contains(rootEdits[0], cardAskAction) || len(replyEdits) != 1 || !strings.Contains(replyEdits[0], "✓ *Merge #41* · answered") {
		t.Fatalf("root edits %v, reply edits %v: want the ask gone from the root and answered on the reply", rootEdits, replyEdits)
	}

	late := cardAsk{ID: "hold-41", Label: "Hold #41", ExpiresAt: &expires}
	l.do(opDMStatus, map[string]any{"card": statusCard{Status: "needs_you", Title: "Hold?", Asks: []cardAsk{late}}})
	push.MessageTS, push.ActionID, push.Value = l.slack.sent()[2].ts, cardAskAction+"hold-41", late.value()
	push.ActionTS, push.Seq = fmt.Sprintf("%d.000100", expires.Add(time.Minute).Unix()), "001800000100000-abcd0125"
	l.tap(id, push)
	if got := l.inject(agent.TopLevel); got != "" {
		t.Fatalf("a tap after the ask expired steered %q", got)
	}
}

func TestWaitingCardTapCarriesTheQuestionAndAnswersOnce(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	card := statusCard{Status: "needs_you", Title: "Merge #41 now?", Options: []string{"Merge", "Hold"}, Waits: "step-3:merge"}
	l.do(opDMStatus, map[string]any{"card": card})
	root, reply := l.slack.sent()[0], l.slack.sent()[1]
	if !strings.Contains(reply.blocks, cardWaitAction+"0:step-3:merge") || !strings.Contains(reply.blocks, cardExplainAction+"step-3:merge") ||
		strings.Contains(reply.blocks, cardAckAction) {
		t.Fatalf("reply %s, want the answers and Explain more as buttons and no Ack", reply.blocks)
	}
	id, err := loadIdentity(l.ctx, l.srv.DB())
	if err != nil {
		t.Fatal(err)
	}
	push := cardActionPush{User: testUser, Channel: id.DMChannelID, MessageTS: reply.ts, ThreadTS: root.ts,
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
	if l.app.cards.answered(id.DMChannelID, root.ts, card.Waits) {
		t.Fatal("Explain more answered the waiting ask")
	}

	push.ActionID, push.Value = cardWaitAction+"0:step-3:merge", string(mustJSON(askTap{ID: card.Waits, Label: "Merge", Question: card.Title}))
	push.ActionTS, push.Seq = fmt.Sprintf("%d.000100", time.Now().Unix()), "001800000100000-abcd0126"
	l.tap(id, push)
	if got := l.inject(agent.TopLevel); !strings.Contains(got, `answered the waiting ask "step-3:merge" with "Merge"`) || !strings.Contains(got, "hand it to that lane") {
		t.Fatalf("directive = %q, want the waiting ask's id, answer, and the hand-off", got)
	}
	payloads := strings.Join(l.channelPayloads(), "\n")
	for _, part := range []string{`"id":"step-3:merge"`, `"waits":true`, `"question":"Merge #41 now?"`, `answered \"Merge #41 now?\" with \"Merge\" at `} {
		if !strings.Contains(payloads, part) {
			t.Fatalf("channel payloads %s lack %s", payloads, part)
		}
	}
	if edits := editsOf(l.slack.edited(), reply.ts); len(edits) != 2 || !strings.Contains(edits[1], "✓ *Merge* · answered") ||
		strings.Contains(edits[1], cardWaitAction) || strings.Contains(edits[1], "still stopped") {
		t.Fatalf("reply edits %v, want the answer in place of the hint and buttons", edits)
	}

	push.ActionID, push.Value = cardWaitAction+"1:step-3:merge", string(mustJSON(askTap{ID: card.Waits, Label: "Hold", Question: card.Title}))
	push.ActionTS, push.Seq = fmt.Sprintf("%d.000200", time.Now().Unix()), "001800000100000-abcd0127"
	l.tap(id, push)
	if got := l.inject(agent.TopLevel); got != "" {
		t.Fatalf("a second tap on an answered waiting ask steered %q", got)
	}
}

func TestRedrawsKeepATappedOptionAndAWhoamiHidesRefTokens(t *testing.T) {
	at := time.Date(2026, 10, 22, 17, 0, 0, 0, time.UTC)
	thread := &liveThread{Channel: "D1", Thread: "1.1"}
	thread.apply(statusCard{Status: "needs_you", Title: "Ship?", Options: []string{"ship it"}}, cardTag{}, at, false)
	thread.Reply.Tapped = "✓ *ship it* · 10:00am"
	if _, reply := thread.replyMessage(at, nil); strings.Contains(blocksJSON(t, reply), cardOptionAction) || !strings.Contains(blocksJSON(t, reply), "✓ *ship it*") {
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

func TestRootOnlyRedrawsTheRootWithoutAReply(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	rollup := map[string]any{"root_only": true, "card": statusCard{Rollup: &cardRollup{Lanes: []rollupLane{{Name: "w4", Status: "shipping"}}}}}
	raw := mustJSON(rollup)
	if _, err := invoke(l.ctx, l.deps, testSession, "/any", opDMStatus, raw); err == nil || !strings.Contains(err.Error(), "post a card first") {
		t.Fatalf("root_only before any card: %v", err)
	}
	l.do(opDMStatus, cardBody("Started"))
	l.do(opDMStatus, rollup)
	sent, edits := l.slack.sent(), l.slack.edited()
	if len(sent) != 2 || len(edits) != 1 || edits[0].ts != sent[0].ts || !strings.Contains(edits[0].blocks, "*w4* · shipping") || !strings.Contains(edits[0].blocks, "*Started*") {
		t.Fatalf("posts %d, edits %+v: want the root redrawn with the rollup over the last title and no reply", len(sent), edits)
	}
}
