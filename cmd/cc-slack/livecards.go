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
	"sync"
	"time"

	"github.com/slack-go/slack"
)

const (
	liveCardIdle      = 48 * time.Hour
	liveMaxThreads    = 100
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
}

type liveThread struct {
	Channel  string               `json:"channel"`
	Thread   string               `json:"thread"`
	Root     *liveMessage         `json:"root,omitempty"`
	Reply    *liveMessage         `json:"reply,omitempty"`
	Timeline []timelineEntry      `json:"timeline,omitempty"`
	Asks     []cardAsk            `json:"asks,omitempty"`
	Answers  map[string]askAnswer `json:"answers,omitempty"`
	Updated  time.Time            `json:"updated"`
	Drawn    time.Time            `json:"drawn"`
	Stale    bool                 `json:"stale,omitempty"`
}

func threadKey(channel, thread string) string { return channel + "/" + thread }

func (t *liveThread) apply(card statusCard, tag cardTag, at time.Time, rootOnly bool) {
	root := statusCard{}
	if t.Root != nil {
		root = t.Root.Card
	}
	t.Root = &liveMessage{TS: t.Thread, Card: root.merged(card), Tag: tag, At: at}
	if card.Title != "" {
		t.Timeline = append(t.Timeline, timelineEntry{At: at, Emoji: card.emoji(), Title: card.Title})
		t.Timeline = t.Timeline[max(0, len(t.Timeline)-liveMaxTimeline):]
	}
	for _, k := range card.Asks {
		t.Asks = slices.DeleteFunc(t.Asks, func(o cardAsk) bool { return o.ID == k.ID })
		t.Asks = append(t.Asks, k)
	}
	t.Asks = t.Asks[max(0, len(t.Asks)-liveMaxAsks):]
	if !rootOnly {
		t.Reply = &liveMessage{Card: card, Tag: tag, At: at}
	}
	t.Updated = at
}

func (t *liveThread) openAsks(now time.Time) []cardAsk {
	var open []cardAsk
	for _, k := range t.Asks {
		if _, done := t.Answers[k.ID]; !done && !k.expired(now) {
			open = append(open, k)
		}
	}
	return open
}

func (t *liveThread) view(m *liveMessage, now time.Time, refs map[string]refState) cardView {
	v := cardView{tag: m.Tag, at: m.At, now: now, refs: refs, answers: t.Answers, tapped: m.Tapped}
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

func (t *liveThread) replyMessage(now time.Time, refs map[string]refState) (string, []slack.Block) {
	return t.Reply.Card.text(), t.Reply.Card.replyBlocks(t.view(t.Reply, now, refs))
}

func (t *liveThread) cites(keys map[string]bool) bool {
	for _, m := range []*liveMessage{t.Root, t.Reply} {
		if m == nil {
			continue
		}
		for _, id := range m.Card.refIDs() {
			if keys[id.key()] {
				return true
			}
		}
	}
	return false
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
	wakeCh chan struct{}

	mu      sync.Mutex
	threads map[string]*liveThread
	refs    map[string]refState
	hosts   map[string]*refHost
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
		c.Timeline, c.Asks, c.Answers = slices.Clone(t.Timeline), slices.Clone(t.Asks), maps.Clone(t.Answers)
		return &c
	}
	return &liveThread{Channel: channel, Thread: thread}
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

func (l *liveCards) answer(channel, thread, askID string, at time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	t, ok := l.threads[threadKey(channel, thread)]
	if !ok {
		return
	}
	if t.Answers == nil {
		t.Answers = map[string]askAnswer{}
	}
	if _, done := t.Answers[askID]; !done {
		t.Answers[askID] = askAnswer{At: at}
	}
	l.save()
}

func (l *liveCards) answered(channel, thread, askID string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	t, ok := l.threads[threadKey(channel, thread)]
	if !ok {
		return false
	}
	_, done := t.Answers[askID]
	return done
}

func (l *liveCards) tapOption(channel, thread, ts, line string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	t, ok := l.threads[threadKey(channel, thread)]
	if !ok || t.Reply == nil || t.Reply.TS != ts {
		return false
	}
	t.Reply.Tapped = line
	return true
}

func (l *liveCards) redraw(ctx context.Context, api slackAPI, channel, thread string, now time.Time) (map[string]bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	t, ok := l.threads[threadKey(channel, thread)]
	if !ok {
		return nil, nil
	}
	redrawn, err := l.draw(ctx, api, t, now)
	l.save()
	return redrawn, err
}

func (l *liveCards) draw(ctx context.Context, api slackAPI, t *liveThread, now time.Time) (map[string]bool, error) {
	redrawn := map[string]bool{}
	var errs []error
	if t.Root != nil {
		text, blocks := t.rootMessage(now, l.refs)
		if err := l.update(ctx, api, t, &t.Root, text, blocks); err != nil {
			errs = append(errs, err)
		} else if t.Root != nil {
			redrawn[t.Root.TS] = true
		}
	}
	if t.Reply != nil {
		text, blocks := t.replyMessage(now, l.refs)
		if err := l.update(ctx, api, t, &t.Reply, text, blocks); err != nil {
			errs = append(errs, err)
		} else if t.Reply != nil {
			redrawn[t.Reply.TS] = true
		}
	}
	t.Drawn, t.Stale = now, len(errs) > 0
	return redrawn, errors.Join(errs...)
}

func (l *liveCards) update(ctx context.Context, api slackAPI, t *liveThread, m **liveMessage, text string, blocks []slack.Block) error {
	ctx, cancel := context.WithTimeout(ctx, liveUpdateTimeout)
	defer cancel()
	err := updateMessage(ctx, api, t.Channel, (*m).TS, text, blocks)
	if err != nil && goneCardCodes[errorCode(err)] {
		l.log.Printf("live card %s/%s: %s; no longer tracking it", t.Channel, (*m).TS, errorCode(err))
		*m = nil
		return nil
	}
	if err != nil {
		return fmt.Errorf("redraw card %s/%s: %w", t.Channel, (*m).TS, err)
	}
	return nil
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
		for _, m := range []*liveMessage{t.Root, t.Reply} {
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
		if t.cites(map[string]bool{key: true}) {
			return true
		}
	}
	return false
}

func (l *liveCards) host(name string, cfg config) *refHost {
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
	changed := map[string]bool{}
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
	return max(next.Sub(now), time.Second)
}

func (l *liveCards) redrawChanged(ctx context.Context, api slackAPI, changed map[string]bool, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	dirty := false
	for _, k := range slices.Sorted(maps.Keys(l.threads)) {
		t := l.threads[k]
		if !t.Stale && !t.cites(changed) && !t.expiredSinceDrawn(now) {
			continue
		}
		dirty = true
		if _, err := l.draw(ctx, api, t, now); err != nil {
			l.log.Printf("live card %s: %v", k, err)
		}
	}
	if dirty {
		l.save()
	}
}
