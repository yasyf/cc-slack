package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/slack-go/slack"
)

const (
	liveCardIdle      = 48 * time.Hour
	liveMaxThreads    = 1000
	liveMaxTimeline   = 20
	liveMaxAsks       = 20
	refPollEvery      = time.Minute
	refMaxBackoff     = 15 * time.Minute
	refHostGitHub     = "github"
	refHostBuildkite  = "buildkite"
	liveUpdateTimeout = 30 * time.Second
)

var goneCardCodes = map[string]bool{"message_not_found": true, "cant_update_message": true, "channel_not_found": true}

func liveCardsPath() string { return filepath.Join(appPaths().StateDir(), "live-cards.json") }

type liveMessage struct {
	TS     string     `json:"ts"`
	Card   statusCard `json:"card"`
	Tag    cardTag    `json:"tag"`
	At     time.Time  `json:"at"`
	Tapped string     `json:"tapped,omitempty"`
	Hint   string     `json:"hint,omitempty"`

	Final    bool `json:"final,omitempty"`
	Unfolded bool `json:"unfolded,omitempty"`
}

type liveThread struct {
	Channel  string                  `json:"channel"`
	Thread   string                  `json:"thread"`
	Root     *liveMessage            `json:"root,omitempty"`
	Reply    *liveMessage            `json:"reply,omitempty"`
	Waiting  map[string]*liveMessage `json:"waiting,omitempty"`
	Groups   map[string]*liveMessage `json:"groups,omitempty"`
	Keyed    map[string]*liveMessage `json:"keyed,omitempty"`
	PRCards  []*liveMessage          `json:"pr_cards,omitempty"`
	Copies   []liveCardCopy          `json:"copies,omitempty"`
	Timeline []timelineEntry         `json:"timeline,omitempty"`
	Asks     []cardAsk               `json:"asks,omitempty"`
	Answers  map[string]askAnswer    `json:"answers,omitempty"`
	Updated  time.Time               `json:"updated"`
	Drawn    time.Time               `json:"drawn"`
	Stale    bool                    `json:"stale,omitempty"`
}

type liveCardCopy struct {
	Card string `json:"card"`
	TS   string `json:"ts"`
}

func threadKey(channel, thread string) string { return channel + "/" + thread }

func (t *liveThread) apply(card statusCard, tag cardTag, at time.Time, rootOnly bool) {
	for _, k := range card.Asks {
		t.Asks = slices.DeleteFunc(t.Asks, func(o cardAsk) bool { return o.ID == k.ID })
		t.Asks = append(t.Asks, k)
	}
	t.Asks = t.Asks[max(0, len(t.Asks)-liveMaxAsks):]
	t.Updated = at
	if !rootOnly {
		t.Reply = &liveMessage{Card: card, Tag: tag, At: at}
		return
	}
	root := statusCard{}
	if t.Root != nil {
		root = t.Root.Card
	}
	t.Root = &liveMessage{TS: t.Thread, Card: root.merged(card), Tag: tag, At: at}
	if card.Title != "" {
		t.Timeline = append(t.Timeline, timelineEntry{At: at, Emoji: card.emoji(), Title: card.Title})
		t.Timeline = t.Timeline[max(0, len(t.Timeline)-liveMaxTimeline):]
	}
}

func (t *liveThread) remember(waits string, m *liveMessage) {
	if t.Waiting == nil {
		t.Waiting = map[string]*liveMessage{}
	}
	t.Waiting[waits] = m
	for len(t.Waiting) > liveMaxAsks {
		oldest := ""
		for k, w := range t.Waiting {
			if oldest == "" || w.At.Before(t.Waiting[oldest].At) {
				oldest = k
			}
		}
		delete(t.Waiting, oldest)
	}
}

func (t *liveThread) rememberGroup(group string, m *liveMessage) {
	if t.Groups == nil {
		t.Groups = map[string]*liveMessage{}
	}
	t.Groups[group] = m
}

func (t *liveThread) kept(card statusCard) *liveMessage {
	switch {
	case card.Group != "":
		return t.Groups[card.Group]
	case card.Key != "":
		return t.Keyed[card.Key]
	}
	return t.Waiting[card.Waits]
}

func (t *liveThread) keep(card statusCard, m *liveMessage) {
	switch {
	case card.Group != "":
		t.rememberGroup(card.Group, m)
	case card.Key != "":
		if t.Keyed == nil {
			t.Keyed = map[string]*liveMessage{}
		}
		t.Keyed[card.Key] = m
	case card.Waits != "":
		t.remember(card.Waits, m)
	}
}

func (t *liveThread) messages() []*liveMessage {
	return slices.Concat([]*liveMessage{t.Reply}, slices.Collect(maps.Values(t.Waiting)), slices.Collect(maps.Values(t.Groups)), slices.Collect(maps.Values(t.Keyed)), t.PRCards)
}

func (t *liveThread) openAsks(now time.Time) []cardAsk {
	return openAsks(t.Asks, t.Answers, now)
}

func openAsks(asks []cardAsk, answers map[string]askAnswer, now time.Time) []cardAsk {
	var open []cardAsk
	for _, k := range asks {
		if _, done := answers[k.ID]; !done && !k.expired(now) {
			open = append(open, k)
		}
	}
	return open
}

func (t *liveThread) view(m *liveMessage, now time.Time, refs map[string]refState) cardView {
	v := cardView{tag: m.Tag, at: m.At, now: now, refs: refs, answers: t.Answers, tapped: m.Tapped, hint: m.Hint, final: m.Final, unfolded: m.Unfolded}
	if m == t.Root {
		v.asks, v.timeline = t.openAsks(now), t.Timeline
	}
	return v
}

func (t *liveThread) rootMessage(now time.Time, refs map[string]refState) (string, []slack.Block) {
	c := t.Root.Card
	text := c.text()
	if c.Title == "" {
		text = cmp.Or(c.Drive, "Status")
	}
	return text, c.rootBlocks(t.view(t.Root, now, refs))
}

func (t *liveThread) replyMessage(m *liveMessage, now time.Time, refs map[string]refState) (string, []slack.Block) {
	v := t.view(m, now, refs)
	return m.Card.replyText(v), m.Card.replyBlocks(v)
}

func (m *liveMessage) cites(keys map[string]bool) bool {
	return m != nil && slices.ContainsFunc(m.Card.refIDs(), func(id refID) bool { return keys[id.key()] })
}

func (t *liveThread) cites(keys map[string]bool) bool {
	return t.Root.cites(keys) || t.Reply.cites(keys)
}

func (t *liveThread) carded(keys map[string]bool) bool {
	return t.cites(keys) || slices.ContainsFunc(t.PRCards, func(m *liveMessage) bool { return m.cites(keys) })
}

func (t *liveThread) open(m *liveMessage) bool {
	_, answered := t.Answers[m.Card.Waits]
	return !answered && m.Tapped == ""
}

func (t *liveThread) expiredSinceDrawn(now time.Time) bool {
	for _, k := range t.Asks {
		if k.ExpiresAt != nil && k.ExpiresAt.After(t.Drawn) && !now.Before(*k.ExpiresAt) {
			return true
		}
	}
	return false
}

func (t *liveThread) nextExpiry(now time.Time) time.Time {
	var next time.Time
	for _, k := range t.openAsks(now) {
		if k.ExpiresAt != nil {
			next = earliest(next, *k.ExpiresAt)
		}
	}
	return next
}

type refHost struct {
	source  tokenSource
	token   *refToken
	due     time.Time
	backoff time.Duration
}

type liveCards struct {
	path   string
	client refClient
	log    *log.Logger
	now    func() time.Time
	plane  plane
	newAPI func(token string) slackAPI
	recard func(ctx context.Context, id identity, card cardRef, pr refID, refused string)
	wakeCh chan struct{}

	mu      sync.Mutex
	threads map[string]*liveThread
	refs    map[string]refState
	hosts   map[string]*refHost
	drawing map[string]*sync.Mutex
}

func (l *liveCards) rememberCopy(channel, thread, card, ts string, at time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	t := l.draft(channel, thread)
	t.Copies = append(t.Copies, liveCardCopy{Card: card, TS: ts})
	t.Updated = at
	l.commit(t)
}

func (l *liveCards) copiesOf(channel, card string) []cardRef {
	l.mu.Lock()
	defer l.mu.Unlock()
	var copies []cardRef
	for _, t := range l.threads {
		if t.Channel != channel {
			continue
		}
		for _, c := range t.Copies {
			if c.Card == card {
				copies = append(copies, cardRef{channel, t.Thread, c.TS})
			}
		}
	}
	return copies
}

func (l *liveCards) openPRCard(channel string, pr refID) (ts, thread string, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	keys := map[string]bool{pr.key(): true}
	var card *liveMessage
	for _, t := range l.threads {
		if t.Channel != channel {
			continue
		}
		for _, m := range t.PRCards {
			if m.Tapped == "" && m.cites(keys) && (card == nil || m.At.After(card.At)) {
				card, thread = m, t.Thread
			}
		}
	}
	if card == nil {
		return "", "", false
	}
	return card.TS, thread, true
}

func (l *liveCards) load() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	data, err := os.ReadFile(l.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read live cards: %w", err)
	}
	var threads []*liveThread
	if err := json.Unmarshal(data, &threads); err != nil {
		return fmt.Errorf("decode live cards %s: %w", l.path, err)
	}
	for _, t := range threads {
		l.threads[threadKey(t.Channel, t.Thread)] = t
	}
	l.prune()
	return nil
}

func (l *liveCards) prune() {
	cutoff := l.now().Add(-liveCardIdle)
	maps.DeleteFunc(l.threads, func(_ string, t *liveThread) bool { return t.Updated.Before(cutoff) })
	for len(l.threads) > liveMaxThreads {
		oldest := ""
		for k, t := range l.threads {
			if oldest == "" || t.Updated.Before(l.threads[oldest].Updated) {
				oldest = k
			}
		}
		delete(l.threads, oldest)
	}
}

func (l *liveCards) save() {
	threads := slices.SortedFunc(maps.Values(l.threads), func(a, b *liveThread) int { return a.Updated.Compare(b.Updated) })
	tmp := l.path + ".tmp"
	if err := os.WriteFile(tmp, mustJSON(threads), 0o600); err != nil {
		l.log.Printf("write live cards: %v", err)
		return
	}
	if err := os.Rename(tmp, l.path); err != nil {
		l.log.Printf("save live cards: %v", err)
	}
}

func (l *liveCards) draft(channel, thread string) *liveThread {
	if t, ok := l.threads[threadKey(channel, thread)]; ok && thread != "" {
		c := *t
		c.Timeline, c.Asks, c.Answers, c.Waiting, c.Groups, c.Keyed = slices.Clone(t.Timeline), slices.Clone(t.Asks), maps.Clone(t.Answers), maps.Clone(t.Waiting), maps.Clone(t.Groups), maps.Clone(t.Keyed)
		c.PRCards, c.Copies = slices.Clone(t.PRCards), slices.Clone(t.Copies)
		return &c
	}
	return &liveThread{Channel: channel, Thread: thread}
}

func (l *liveCards) holding(channel string, threads []string, card statusCard) *liveThread {
	if card.Waits == "" && card.Group == "" && card.Key == "" {
		return nil
	}
	return l.sessionThread(channel, threads, func(t *liveThread) bool {
		_, answered := t.Answers[card.Waits]
		return t.kept(card) != nil || (card.Waits != "" && answered)
	})
}

func (l *liveCards) rooted(channel string, threads []string) *liveThread {
	return l.sessionThread(channel, threads, func(t *liveThread) bool { return t.Root != nil })
}

func (l *liveCards) sessionThread(channel string, threads []string, matches func(*liveThread) bool) *liveThread {
	for _, thread := range threads {
		if t, ok := l.threads[threadKey(channel, thread)]; ok && matches(t) {
			return t
		}
	}
	return nil
}

func (l *liveCards) rootPRCard(channel string, matches func(*liveThread, *liveMessage) bool) bool {
	for _, t := range l.threads {
		if t.Channel == channel && slices.ContainsFunc(t.PRCards, func(m *liveMessage) bool { return m.TS == t.Thread && matches(t, m) }) {
			return true
		}
	}
	return false
}

func (l *liveCards) pickOffers(channel string, offer cardOffer) bool {
	for _, t := range l.threads {
		if t.Channel == channel && slices.ContainsFunc(slices.Collect(maps.Values(t.Waiting)), func(m *liveMessage) bool { return offer.same(m.Card.offer()) }) {
			return true
		}
	}
	return false
}

type offeredCard struct {
	ref      cardRef
	waits    string
	question string
	options  []string
	pr       refID
}

func (l *liveCards) otherOffers(tapped cardRef) (prs, picks []offeredCard) {
	l.mu.Lock()
	defer l.mu.Unlock()
	t, ok := l.threads[threadKey(tapped.channel, tapped.thread)]
	if !ok {
		return nil, nil
	}
	at := slices.IndexFunc(t.messages(), func(m *liveMessage) bool { return m != nil && m.TS == tapped.ts })
	if at < 0 {
		return nil, nil
	}
	asked := t.messages()[at].Card
	offer := asked.offer()
	for _, o := range l.threads {
		if o.Channel != tapped.channel {
			continue
		}
		for _, m := range o.PRCards {
			ids := m.Card.refIDs()
			if m.TS != tapped.ts && o.open(m) && len(ids) > 0 && (m.Card.Waits == asked.Waits || offer.same(m.Card.offer())) {
				prs = append(prs, offeredCard{cardRef{o.Channel, o.Thread, m.TS}, m.Card.Waits, m.Card.Title, m.Card.answers(), ids[0]})
			}
		}
		for _, m := range o.Waiting {
			if m.TS != tapped.ts && o.open(m) && offer.same(m.Card.offer()) {
				picks = append(picks, offeredCard{ref: cardRef{o.Channel, o.Thread, m.TS}, waits: m.Card.Waits, question: m.Card.Title, options: m.Card.answers()})
			}
		}
	}
	return prs, picks
}

func (l *liveCards) forget(t *liveThread, ts string) {
	if t.Reply != nil && t.Reply.TS == ts {
		t.Reply = nil
	}
	at := func(_ string, m *liveMessage) bool { return m.TS == ts }
	maps.DeleteFunc(t.Waiting, at)
	maps.DeleteFunc(t.Groups, at)
	maps.DeleteFunc(t.Keyed, at)
	if t.Root == nil && t.Reply == nil && len(t.Waiting)+len(t.Groups)+len(t.Keyed)+len(t.PRCards) == 0 {
		delete(l.threads, threadKey(t.Channel, t.Thread))
	}
}

func (l *liveCards) commit(t *liveThread) {
	t.Drawn = l.now()
	l.threads[threadKey(t.Channel, t.Thread)] = t
	l.prune()
	l.save()
	l.wake()
}

func (l *liveCards) wake() {
	select {
	case l.wakeCh <- struct{}{}:
	default:
	}
}

func (l *liveCards) claim(channel, thread, askID string, ans askAnswer) (bool, askAnswer) {
	l.mu.Lock()
	defer l.mu.Unlock()
	t, ok := l.threads[threadKey(channel, thread)]
	if !ok {
		return true, ans
	}
	if prior, done := t.Answers[askID]; done {
		return false, prior
	}
	if t.Answers == nil {
		t.Answers = map[string]askAnswer{}
	}
	t.Answers[askID] = ans
	l.save()
	return true, ans
}

func (l *liveCards) release(channel, thread, askID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if t, ok := l.threads[threadKey(channel, thread)]; ok {
		delete(t.Answers, askID)
		l.save()
	}
}

func (l *liveCards) showPictures(channel, thread, ts string, shown statusCard) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	t, ok := l.threads[threadKey(channel, thread)]
	if !ok {
		return false
	}
	found := false
	for _, m := range append(t.messages(), t.Root) {
		if m != nil && m.TS == ts {
			m.Card.Images, m.Card.Artifacts = shown.Images, shown.Artifacts
			found = true
		}
	}
	return found
}

func (l *liveCards) shownImages(channel string, card statusCard) []slack.FileSummary {
	if card.ImagesHash == "" || card.Waits == "" {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, t := range l.threads {
		if m := t.Waiting[card.Waits]; t.Channel == channel && m != nil && m.Card.ImagesHash == card.ImagesHash {
			return m.Card.Images
		}
	}
	return nil
}

func (l *liveCards) answerDrawn(channel, thread, askID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	t, ok := l.threads[threadKey(channel, thread)]
	if !ok {
		return
	}
	if ans, done := t.Answers[askID]; done {
		ans.Drawn = true
		t.Answers[askID] = ans
		l.save()
	}
}

func (l *liveCards) waiting(channel, waits string, at func(thread, ts string) bool) (string, liveMessage, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, t := range l.threads {
		if m := t.Waiting[waits]; t.Channel == channel && m != nil && at(t.Thread, m.TS) {
			return t.Thread, *m, true
		}
	}
	return "", liveMessage{}, false
}

func (l *liveCards) asks(channel, thread, waits, question string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	t, ok := l.threads[threadKey(channel, thread)]
	if !ok || t.Waiting[waits] == nil {
		return true
	}
	return t.Waiting[waits].Card.Title == question
}

func (l *liveCards) answer(channel, thread, askID string) (askAnswer, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	t, ok := l.threads[threadKey(channel, thread)]
	if !ok {
		return askAnswer{}, false
	}
	ans, done := t.Answers[askID]
	return ans, done
}

func (l *liveCards) numberedPick(channel, thread, text string) (string, cardPick, bool) {
	n, err := strconv.Atoi(text)
	if err != nil || strconv.Itoa(n) != text {
		return "", cardPick{}, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	t, ok := l.threads[threadKey(channel, thread)]
	if !ok {
		return "", cardPick{}, false
	}
	var open *liveMessage
	for _, m := range t.messages() {
		if m == nil || m.TS == "" || m.Tapped != "" || m.Card.Group != "" || len(m.Card.Options) == 0 {
			continue
		}
		if _, done := t.Answers[m.Card.Waits]; m.Card.Waits != "" && done {
			continue
		}
		if open == nil || m.At.After(open.At) {
			open = m
		}
	}
	if open == nil || n < 1 || n > len(open.Card.Options) {
		return "", cardPick{}, false
	}
	return open.TS, open.Card.picks()[n-1], true
}

func (l *liveCards) supersedes(channel, thread, ts string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	t, ok := l.threads[threadKey(channel, thread)]
	if !ok {
		return nil
	}
	for _, m := range t.messages() {
		if m != nil && m.TS == ts {
			return m.Card.Supersedes
		}
	}
	return nil
}

func (l *liveCards) markReply(channel, thread, ts string, mark func(*liveMessage)) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	t, ok := l.threads[threadKey(channel, thread)]
	if !ok {
		return false
	}
	marked := false
	for _, m := range t.messages() {
		if m != nil && m.TS == ts {
			mark(m)
			marked = true
		}
	}
	return marked
}

func (l *liveCards) fold(channel, thread, ts string, now time.Time) [][]slack.Block {
	l.mu.Lock()
	defer l.mu.Unlock()
	t, ok := l.threads[threadKey(channel, thread)]
	if !ok {
		return nil
	}
	for _, m := range t.messages() {
		if m != nil && m.TS == ts && !m.Unfolded {
			_, pages := m.Card.layout(t.view(m, now, l.refs))
			return pages
		}
	}
	return nil
}

func (l *liveCards) redraw(ctx context.Context, api slackAPI, channel, thread, first string, now time.Time) (map[string]bool, error) {
	key := threadKey(channel, thread)
	defer l.drawLock(key)()
	l.mu.Lock()
	t, ok := l.threads[key]
	if !ok {
		l.mu.Unlock()
		return nil, nil
	}
	writes := l.plan(t, now, first)
	l.mu.Unlock()
	return l.flush(ctx, api, t, writes, now, true)
}

func (l *liveCards) drawLock(key string) func() {
	l.mu.Lock()
	mu, ok := l.drawing[key]
	if !ok {
		mu = &sync.Mutex{}
		l.drawing[key] = mu
	}
	l.mu.Unlock()
	mu.Lock()
	return mu.Unlock
}

type cardWrite struct {
	m        *liveMessage
	ts       string
	text     string
	blocks   []slack.Block
	entities []slack.MsgOption
}

func (l *liveCards) write(t *liveThread, m *liveMessage, now time.Time, root bool) cardWrite {
	text, blocks := t.replyMessage(m, now, l.refs)
	if root {
		text, blocks = t.rootMessage(now, l.refs)
	}
	w := cardWrite{m: m, ts: m.TS, text: text, blocks: blocks}
	if m.Tapped == "" {
		w.entities = m.Card.entities()
	}
	return w
}

func (l *liveCards) plan(t *liveThread, now time.Time, first string) []cardWrite {
	var writes []cardWrite
	for _, kept := range []map[string]*liveMessage{t.Waiting, t.Groups, t.Keyed} {
		for _, m := range kept {
			if m != nil && m.TS == first && (t.Reply == nil || t.Reply.TS != first) {
				writes = append(writes, l.write(t, m, now, false))
			}
		}
	}
	if i := slices.IndexFunc(t.PRCards, func(m *liveMessage) bool { return m.TS == first }); i >= 0 {
		writes = append(writes, l.write(t, t.PRCards[i], now, false))
	}
	order := []*liveMessage{t.Root, t.Reply}
	if t.Reply != nil && t.Reply.TS == first {
		order = []*liveMessage{t.Reply, t.Root}
	}
	for _, m := range order {
		if m != nil {
			writes = append(writes, l.write(t, m, now, m == t.Root))
		}
	}
	return writes
}

func (l *liveCards) flush(ctx context.Context, api slackAPI, t *liveThread, writes []cardWrite, now time.Time, whole bool) (map[string]bool, error) {
	redrawn := map[string]bool{}
	var gone []*liveMessage
	var errs []error
	for _, w := range writes {
		err := l.update(ctx, api, t.Channel, w)
		switch {
		case err != nil && goneCardCodes[errorCode(err)]:
			l.log.Printf("live card %s/%s: %s; no longer tracking it", t.Channel, w.ts, errorCode(err))
			gone = append(gone, w.m)
		case err != nil:
			errs = append(errs, fmt.Errorf("redraw card %s/%s: %w", t.Channel, w.ts, err))
		default:
			redrawn[w.ts] = true
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, m := range gone {
		t.drop(m)
	}
	if whole {
		t.Drawn, t.Stale = now, len(errs) > 0
	}
	l.save()
	return redrawn, errors.Join(errs...)
}

func (l *liveCards) update(ctx context.Context, api slackAPI, channel string, w cardWrite) error {
	ctx, cancel := context.WithTimeout(ctx, liveUpdateTimeout)
	defer cancel()
	return updateMessage(ctx, api, channel, w.ts, w.text, w.blocks, w.entities...)
}

func (t *liveThread) drop(m *liveMessage) {
	if t.Root == m {
		t.Root = nil
	}
	if t.Reply == m {
		t.Reply = nil
	}
	for _, kept := range []map[string]*liveMessage{t.Waiting, t.Groups, t.Keyed} {
		maps.DeleteFunc(kept, func(_ string, k *liveMessage) bool { return k == m })
	}
	t.PRCards = slices.DeleteFunc(t.PRCards, func(k *liveMessage) bool { return k == m })
}

func (l *liveCards) pending(now time.Time) (map[string][]refID, map[string]bool, time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	tracked := len(l.threads)
	l.prune()
	if len(l.threads) != tracked {
		l.save()
	}
	byHost := map[string][]refID{}
	seen := map[string]bool{}
	var unknown map[string]bool
	var expiry time.Time
	for _, t := range l.threads {
		expiry = earliest(expiry, t.nextExpiry(now))
		for _, m := range slices.Concat([]*liveMessage{t.Root, t.Reply}, t.PRCards) {
			if m == nil {
				continue
			}
			for _, id := range m.Card.refIDs() {
				k := id.key()
				state, known := l.refs[k]
				if seen[k] || state.terminal() {
					continue
				}
				seen[k] = true
				host := refHostGitHub
				if id.Kind == refBuild {
					host = refHostBuildkite
				}
				byHost[host] = append(byHost[host], id)
				if !known {
					if unknown == nil {
						unknown = map[string]bool{}
					}
					unknown[host] = true
				}
			}
		}
	}
	maps.DeleteFunc(l.refs, func(k string, _ refState) bool { return !seen[k] && !l.cited(k) })
	return byHost, unknown, expiry
}

func (l *liveCards) cited(key string) bool {
	for _, t := range l.threads {
		if t.carded(map[string]bool{key: true}) {
			return true
		}
	}
	return false
}

func (l *liveCards) host(name string, cfg config) *refHost {
	l.mu.Lock()
	defer l.mu.Unlock()
	src := cfg.GitHub
	if name == refHostBuildkite {
		src = cfg.Buildkite
	}
	if src == nil {
		delete(l.hosts, name)
		return nil
	}
	h, ok := l.hosts[name]
	if !ok || h.source.Token != src.Token || !slices.Equal(h.source.Command, src.Command) {
		h = &refHost{source: *src}
		h.token = &refToken{source: &h.source}
		l.hosts[name] = h
	}
	return h
}

func (l *liveCards) fetch(ctx context.Context, name string, h *refHost, refs []refID) (map[string]refState, error) {
	if name == refHostGitHub {
		return l.client.pullRequests(ctx, h.token, refs)
	}
	out := map[string]refState{}
	var errs []error
	for _, r := range refs {
		s, err := l.client.build(ctx, h.token, r)
		if _, limited := errors.AsType[errRateLimited](err); limited {
			return out, err
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", r.key(), err))
			continue
		}
		out[r.key()] = s
	}
	if len(out) > 0 {
		for _, err := range errs {
			l.log.Printf("poll buildkite ref %v", err)
		}
		return out, nil
	}
	return out, errors.Join(errs...)
}

func (l *liveCards) run(ctx context.Context) {
	for {
		wait := l.tick(ctx)
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-l.wakeCh:
			timer.Stop()
		case <-timer.C:
		}
	}
}

func (l *liveCards) tick(ctx context.Context) time.Duration {
	id, err := loadIdentity(ctx, l.plane.DB())
	if err != nil {
		return refMaxBackoff
	}
	now := l.now()
	byHost, unknown, next := l.pending(now)
	next = earliest(next, now.Add(refMaxBackoff))
	changed, moved := map[string]bool{}, map[string]bool{}
	for _, name := range slices.Sorted(maps.Keys(byHost)) {
		h := l.host(name, id.Config)
		if h == nil {
			continue
		}
		if now.Before(h.due) && (h.backoff > 0 || !unknown[name]) {
			next = earliest(next, h.due)
			continue
		}
		states, err := l.fetch(ctx, name, h, byHost[name])
		l.mu.Lock()
		for k, s := range states {
			if l.refs[k] != s {
				moved[k] = s.movedFrom(l.refs[k])
				l.refs[k] = s
				changed[k] = true
			}
		}
		l.mu.Unlock()
		h.due = now.Add(refPollEvery)
		if err != nil {
			h.backoff = min(max(2*h.backoff, refPollEvery), refMaxBackoff)
			h.due = now.Add(h.backoff)
			if limited, ok := errors.AsType[errRateLimited](err); ok && limited.until.After(h.due) {
				h.due = limited.until
			}
			l.log.Printf("poll %s refs: %v; next try %s", name, err, h.due.Format(time.RFC3339))
		} else {
			h.backoff = 0
		}
		next = earliest(next, h.due)
	}
	l.redrawChanged(ctx, l.newAPI(id.Token), changed, now)
	l.recardMoved(ctx, id, moved)
	return max(next.Sub(now), time.Second)
}

func (l *liveCards) redrawChanged(ctx context.Context, api slackAPI, changed map[string]bool, now time.Time) {
	l.mu.Lock()
	keys := slices.Sorted(maps.Keys(l.threads))
	l.mu.Unlock()
	for _, k := range keys {
		l.redrawMoved(ctx, api, k, changed, now)
	}
}

func (l *liveCards) redrawMoved(ctx context.Context, api slackAPI, key string, changed map[string]bool, now time.Time) {
	defer l.drawLock(key)()
	l.mu.Lock()
	t, ok := l.threads[key]
	if !ok {
		l.mu.Unlock()
		return
	}
	whole := t.Stale || t.cites(changed) || t.expiredSinceDrawn(now)
	var writes, prCards []cardWrite
	if whole {
		writes = l.plan(t, now, "")
	}
	for _, m := range t.PRCards {
		if m.cites(changed) {
			prCards = append(prCards, l.write(t, m, now, false))
		}
	}
	l.mu.Unlock()
	if whole {
		if _, err := l.flush(ctx, api, t, writes, now, true); err != nil {
			l.log.Printf("live card %s: %v", key, err)
		}
	}
	if len(prCards) > 0 {
		if _, err := l.flush(ctx, api, t, prCards, now, false); err != nil {
			l.log.Printf("live card %v", err)
		}
	}
}

func (l *liveCards) recardMoved(ctx context.Context, id identity, moved map[string]bool) {
	if len(id.Config.PRLinks.CardCommand) == 0 {
		return
	}
	for _, posted := range l.postedPRCards() {
		if moved[posted.pr.key()] {
			l.recard(ctx, id, posted.card, posted.pr, "")
		}
	}
}

type postedPRCard struct {
	card cardRef
	pr   refID
}

// postedPRCards lists every PR card, answered ones too, since each follows its
// PR's row for as long as it stays in the thread.
func (l *liveCards) postedPRCards() []postedPRCard {
	l.mu.Lock()
	defer l.mu.Unlock()
	var posted []postedPRCard
	for _, t := range l.threads {
		for _, m := range t.PRCards {
			if ids := m.Card.refIDs(); len(ids) > 0 {
				posted = append(posted, postedPRCard{cardRef{t.Channel, t.Thread, m.TS}, ids[0]})
			}
		}
	}
	return posted
}

// replacePRCard redraws a posted PR card in place from what its command says now.
func (l *liveCards) replacePRCard(ctx context.Context, api slackAPI, card cardRef, drawn statusCard, now time.Time) error {
	key := threadKey(card.channel, card.thread)
	defer l.drawLock(key)()
	l.mu.Lock()
	t, ok := l.threads[key]
	if !ok {
		l.mu.Unlock()
		return fmt.Errorf("no live thread %s/%s", card.channel, card.thread)
	}
	i := slices.IndexFunc(t.PRCards, func(m *liveMessage) bool { return m.TS == card.ts })
	if i < 0 {
		l.mu.Unlock()
		return fmt.Errorf("no PR card %s in thread %s/%s", card.ts, card.channel, card.thread)
	}
	m := t.PRCards[i]
	if drawn.Waits == "" {
		drawn.Waits, drawn.Options, drawn.Recommended = m.Card.Waits, m.Card.Options, m.Card.Recommended
		if t.open(m) {
			m.Tapped = cardNoLongerWaits
		}
	}
	m.Card = drawn
	w := l.write(t, m, now, false)
	l.mu.Unlock()
	_, err := l.flush(ctx, api, t, []cardWrite{w}, now, false)
	return err
}

func (l *liveCards) threadCard(channel, thread string) (id, title string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	t, ok := l.threads[threadKey(channel, thread)]
	if !ok {
		return "", ""
	}
	for _, m := range append(t.messages(), t.Root) {
		if m != nil && m.TS == thread {
			return cmp.Or(m.Card.Waits, m.Card.Group, m.Card.Key), m.Card.Title
		}
	}
	return "", ""
}

func (l *liveCards) prCard(channel, thread, waits string) (refID, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	t, ok := l.threads[threadKey(channel, thread)]
	if !ok {
		return refID{}, false
	}
	for _, m := range t.PRCards {
		if ids := m.Card.refIDs(); m.Card.Waits == waits && len(ids) > 0 {
			return ids[0], true
		}
	}
	return refID{}, false
}

func (l *liveCards) prRow(channel, thread, row string) (refID, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	t, ok := l.threads[threadKey(channel, thread)]
	if !ok {
		return refID{}, false
	}
	for _, m := range t.messages() {
		if m == nil {
			continue
		}
		for _, r := range m.Card.Rows {
			if r.ID == row && r.PRCard {
				return parseRef(r.PR.URL)
			}
		}
	}
	return refID{}, false
}
