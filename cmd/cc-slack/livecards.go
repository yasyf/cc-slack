package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log"
	"maps"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/slack-go/slack"
	"github.com/yasyf/cc-interact/store"
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
	carriedMoreText   = "More to answer from the message above"
)

var goneCardCodes = map[string]bool{"message_not_found": true, "cant_update_message": true, "channel_not_found": true}

func liveCardsPath() string { return filepath.Join(appPaths().StateDir(), "live-cards.db") }

type liveMessage struct {
	TS     string     `json:"ts"`
	Card   statusCard `json:"card"`
	Tag    cardTag    `json:"tag"`
	At     time.Time  `json:"at"`
	Tapped string     `json:"tapped,omitempty"`
	Hint   string     `json:"hint,omitempty"`

	ReplacedBy string `json:"replaced_by,omitempty"`

	Final    bool `json:"final,omitempty"`
	Unfolded bool `json:"unfolded,omitempty"`
	Edited   bool `json:"edited,omitempty"`
	Ended    bool `json:"ended,omitempty"`
}

type liveThread struct {
	Channel   string                  `json:"channel"`
	Thread    string                  `json:"thread"`
	Root      *liveMessage            `json:"root,omitempty"`
	Reply     *liveMessage            `json:"reply,omitempty"`
	Waiting   map[string]*liveMessage `json:"waiting,omitempty"`
	Groups    map[string]*liveMessage `json:"groups,omitempty"`
	Keyed     map[string]*liveMessage `json:"keyed,omitempty"`
	PRCards   []*liveMessage          `json:"pr_cards,omitempty"`
	Copies    []liveCardCopy          `json:"copies,omitempty"`
	Carries   []string                `json:"carries,omitempty"`
	Continued []string                `json:"continued,omitempty"`
	Timeline  []timelineEntry         `json:"timeline,omitempty"`
	Asks      []cardAsk               `json:"asks,omitempty"`
	Answers   map[string]askAnswer    `json:"answers,omitempty"`
	Updated   time.Time               `json:"updated"`
	Drawn     time.Time               `json:"drawn"`
	Stale     bool                    `json:"stale,omitempty"`
}

type liveCardCopy struct {
	Card string `json:"card"`
	TS   string `json:"ts"`
	Link bool   `json:"link,omitempty"`
}

type askingCard struct {
	ref   cardRef
	title string
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

func (t *liveThread) carryAnswers(from *liveThread) {
	answers := map[string]askAnswer{}
	maps.Copy(answers, from.Answers)
	maps.Copy(answers, t.Answers)
	t.Answers = answers
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

type canvas struct {
	refs     map[string]refState
	pictures pictureConfig
}

func (t *liveThread) view(m *liveMessage, now time.Time, d canvas) cardView {
	v := cardView{tag: m.Tag, at: m.At, now: now, refs: d.refs, answers: t.Answers, tapped: m.Tapped, hint: m.Hint, final: m.Final, unfolded: m.Unfolded, pictures: d.pictures}
	if m == t.Root {
		v.asks, v.timeline = t.openAsks(now), t.Timeline
	}
	return v
}

func (t *liveThread) rootMessage(now time.Time, d canvas) (string, []slack.Block) {
	c := t.Root.Card
	text := c.text()
	if c.Title == "" {
		text = cmp.Or(c.Drive, "Status")
	}
	return text, c.rootBlocks(t.view(t.Root, now, d))
}

func (t *liveThread) replyMessage(m *liveMessage, now time.Time, d canvas) (string, []slack.Block) {
	v := t.view(m, now, d)
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
	return !answered && m.Tapped == "" && m.ReplacedBy == ""
}

func (t *liveThread) asks(m *liveMessage) bool {
	return m.Card.Waits != "" && t.open(m)
}

func (t *liveThread) retired(m *liveMessage) bool {
	return m.ReplacedBy != "" || t.withdrawn(m)
}

func (t *liveThread) withdrawn(m *liveMessage) bool {
	return m.ReplacedBy == "" && t.Answers[m.Card.Waits].Withdrawn
}

func (t *liveThread) revives(m *liveMessage, drawn statusCard) bool {
	return t.withdrawn(m) && drawn.Waits != "" && drawn.Waits == m.Card.Waits
}

func (t *liveThread) shows(keys map[string]bool) bool {
	return t.cites(keys) || slices.ContainsFunc(t.PRCards, func(m *liveMessage) bool { return m.cites(keys) && t.asks(m) })
}

func (t *liveThread) changedSinceAnswer(m *liveMessage, drawn statusCard) string {
	if drawn.Waits == "" || drawn.Waits != m.Card.Waits || t.open(m) || m.Tapped == cardNoLongerWaits {
		return ""
	}
	return drawn.changedSince(m.Card)
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
	for _, ans := range t.Answers {
		if !ans.UndoUntil.IsZero() {
			next = earliest(next, ans.UndoUntil)
		}
	}
	return next
}

func (t *liveThread) answered(waits string) []*liveMessage {
	return slices.DeleteFunc(t.messages(), func(m *liveMessage) bool { return m == nil || m.Card.Waits != waits })
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
	recard func(ctx context.Context, id identity, card cardRef, pr refID)
	wakeCh chan struct{}
	dueCh  chan time.Time

	pictures atomic.Pointer[pictureConfig]

	st           *store.Store
	dirtyThreads map[string]bool
	dirtyRefs    map[string]bool
	mu           sync.Mutex
	threads      map[string]*liveThread
	refs         map[string]refState
	hosts        map[string]*refHost
	drawing      map[string]*sync.Mutex
	undrawn      map[string]map[string]bool
}

func (l *liveCards) canvas() canvas {
	d := canvas{refs: l.refs}
	if p := l.pictures.Load(); p != nil {
		d.pictures = *p
	}
	return d
}

func (l *liveCards) usePictures(p pictureConfig) { l.pictures.Store(&p) }

func (l *liveCards) rememberCopy(channel, thread string, copied liveCardCopy, at time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	t := l.draft(channel, thread)
	t.Copies = append(t.Copies, copied)
	t.Updated = at
	l.commit(t)
}

func (l *liveCards) carry(channel, thread, card string, at time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	t := l.draft(channel, thread)
	if slices.Contains(t.Carries, card) {
		return false
	}
	t.Carries = append(t.Carries, card)
	t.Updated = at
	l.commit(t)
	return true
}

func (l *liveCards) keepCarried(channel, thread string, keys map[string]bool, at time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	t := l.draft(channel, thread)
	kept := slices.DeleteFunc(slices.Clone(t.Carries), func(card string) bool {
		_, m := l.liveMessage(channel, card)
		return !m.cites(keys)
	})
	if len(kept) == len(t.Carries) {
		return
	}
	t.Carries, t.Updated = kept, at
	l.commit(t)
}

func (l *liveCards) live(channel, ts string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	t, ok := l.threads[threadKey(channel, ts)]
	return ok && slices.ContainsFunc(append(t.messages(), t.Root), func(m *liveMessage) bool { return m != nil && m.TS == ts })
}

func (l *liveCards) carriersOf(drawn *liveThread, cards map[string]bool) []cardRef {
	var carriers []cardRef
	for _, t := range l.threads {
		if t.Channel == drawn.Channel && t.Thread != drawn.Thread && slices.ContainsFunc(t.Carries, func(card string) bool { return cards[card] }) {
			carriers = append(carriers, cardRef{t.Channel, t.Thread, t.Thread})
		}
	}
	return carriers
}

func (l *liveCards) carried(t *liveThread, now time.Time) ([]slack.Block, []slack.WorkObjectEntity) {
	var rows []slack.Block
	var prs []slack.WorkObjectEntity
	for _, ts := range t.Carries {
		ct, m := l.liveMessage(t.Channel, ts)
		if m == nil {
			continue
		}
		_, card := ct.replyMessage(m, now, l.canvas())
		if row := carriedCard(card, permalinkRef{channelID: t.Channel, ts: m.TS, threadTS: ct.Thread}); row != nil {
			rows = append(rows, row)
			prs = append(prs, m.Card.prEntities(ct.view(m, now, l.canvas()))...)
		}
	}
	return rows, prs
}

func (l *liveCards) workObjectsOf(channel, ts string, now time.Time) []slack.MsgOption {
	l.mu.Lock()
	defer l.mu.Unlock()
	t, m := l.liveMessage(channel, ts)
	if m == nil || m.Tapped != "" {
		return nil
	}
	return m.Card.entities(m.Card.prEntities(t.view(m, now, l.canvas()))...)
}

func (l *liveCards) withCarried(t *liveThread, blocks []slack.Block, now time.Time) ([]slack.Block, [][]slack.Block, []slack.WorkObjectEntity) {
	rows, prs := l.carried(t, now)
	head := min(len(rows), max(0, cardMaxBlocks-len(blocks)))
	return append(blocks, rows[:head]...), slices.Collect(slices.Chunk(rows[head:], cardMaxBlocks)), prs
}

func (l *liveCards) continueCarried(ctx context.Context, api slackAPI, channel, thread string, held []string, pages [][]slack.Block) ([]string, error) {
	kept := make([]string, 0, len(pages))
	for n, page := range pages {
		if n < len(held) {
			err := updateMessage(ctx, api, channel, held[n], carriedMoreText, page)
			if err == nil {
				kept = append(kept, held[n])
				continue
			}
			if !goneCardCodes[errorCode(err)] {
				return append(kept, held[n:]...), err
			}
		}
		if err := checkBlocks(page); err != nil {
			return append(kept, held[min(n, len(held)):]...), err
		}
		id, err := loadIdentity(ctx, l.plane.DB())
		if err != nil {
			return append(kept, held[min(n, len(held)):]...), err
		}
		_, ts, err := api.PostMessageContext(atStep(ctx, fmt.Sprintf("carried:%s:%d", thread, n+1)), channel, id.postOptions(carriedMoreText, thread, slack.MsgOptionBlocks(page...))...)
		if err != nil {
			return append(kept, held[min(n+1, len(held)):]...), err
		}
		kept = append(kept, ts)
	}
	for at, ts := range held[min(len(pages), len(held)):] {
		if _, _, err := api.DeleteMessageContext(ctx, channel, ts); err != nil && !goneCardCodes[errorCode(err)] {
			return append(kept, held[len(pages)+at:]...), err
		}
	}
	return kept, nil
}

func (l *liveCards) continued(channel, thread string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if t, ok := l.threads[threadKey(channel, thread)]; ok {
		return slices.Clone(t.Continued)
	}
	return nil
}

func (l *liveCards) awaitsTap(channel, ts string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	t, m := l.liveMessage(channel, ts)
	return m == nil || t.asks(m)
}

func (l *liveCards) openCard(channel, ts string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	t, m := l.liveMessage(channel, ts)
	return m != nil && t.asks(m)
}

func (l *liveCards) holdsOpenCard(channel, thread string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	t, ok := l.threads[threadKey(channel, thread)]
	return ok && slices.ContainsFunc(t.messages(), func(m *liveMessage) bool { return m != nil && t.asks(m) })
}

func (l *liveCards) liveMessage(channel, ts string) (*liveThread, *liveMessage) {
	for _, t := range l.threads {
		if t.Channel != channel {
			continue
		}
		for _, m := range t.messages() {
			if m != nil && m.TS == ts {
				return t, m
			}
		}
	}
	return nil, nil
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
			if c.Card == card && !c.Link {
				copies = append(copies, cardRef{channel, t.Thread, c.TS})
			}
		}
	}
	return copies
}

func (l *liveCards) showsIn(channel, thread string, card cardRef) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.shows(channel, thread, "", card)
}

func (l *liveCards) askingCards(channel string) []askingCard {
	l.mu.Lock()
	defer l.mu.Unlock()
	var open []askingCard
	for _, t := range l.threads {
		if t.Channel != channel {
			continue
		}
		for _, m := range t.messages() {
			if m != nil && t.asks(m) {
				open = append(open, askingCard{cardRef{channel, t.Thread, m.TS}, m.Card.Title})
			}
		}
	}
	return open
}

func (l *liveCards) cardAt(channel, ts string) (cardRef, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.storedCard(channel, ts)
}

func (l *liveCards) titleOf(channel, ts string) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, m := l.liveMessage(channel, ts)
	return m.Card.Title
}

func (l *liveCards) storedCard(channel, ts string) (cardRef, bool) {
	for _, t := range l.threads {
		if t.Channel == channel && slices.ContainsFunc(append(t.messages(), t.Root), func(m *liveMessage) bool { return m != nil && m.TS == ts }) {
			return cardRef{channel, t.Thread, ts}, true
		}
	}
	return cardRef{}, false
}

func (l *liveCards) drawnAt(channel, ts string) (cardRef, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if card, found := l.storedCard(channel, ts); found {
		return card, true
	}
	return l.copySource(channel, ts)
}

func (l *liveCards) copySource(channel, ts string) (cardRef, bool) {
	for _, t := range l.threads {
		if at := slices.IndexFunc(t.Copies, func(c liveCardCopy) bool { return c.TS == ts && !c.Link }); t.Channel == channel && at >= 0 {
			return l.storedCard(channel, t.Copies[at].Card)
		}
	}
	return cardRef{}, false
}

func (l *liveCards) located(a cardActionPush) (cardActionPush, bool) {
	card, copied := a.onCard()
	l.mu.Lock()
	defer l.mu.Unlock()
	stored, found := l.storedCard(a.Channel, card.MessageTS)
	if !found && !copied {
		stored, found = l.copySource(a.Channel, card.MessageTS)
	}
	if found {
		card.MessageTS, card.ThreadTS = stored.ts, stored.thread
	}
	return card, found || !copied
}

func (l *liveCards) retire(posts []string, by, line string) []string {
	var changed []string
	for _, post := range posts {
		ref, _ := parsePermalink(post)
		if t, m := l.liveMessage(ref.channelID, ref.ts); m != nil && m.ReplacedBy == "" {
			changed = append(changed, threadKey(t.Channel, t.Thread))
			m.ReplacedBy, m.Tapped = by, cmp.Or(m.Tapped, line)
		}
	}
	return changed
}

func (l *liveCards) prune() []string {
	var removed []string
	cutoff := l.now().Add(-liveCardIdle)
	var settled []string
	for k, t := range l.threads {
		switch {
		case t.asking():
		case t.Updated.Before(cutoff):
			delete(l.threads, k)
			removed = append(removed, k)
		default:
			settled = append(settled, k)
		}
	}
	slices.SortFunc(settled, func(a, b string) int { return l.threads[a].Updated.Compare(l.threads[b].Updated) })
	for _, k := range settled[:max(0, min(len(settled), len(l.threads)-liveMaxThreads))] {
		delete(l.threads, k)
		removed = append(removed, k)
	}
	return removed
}

func (t *liveThread) asking() bool {
	for _, m := range t.Waiting {
		if t.asks(m) {
			return true
		}
	}
	return slices.ContainsFunc(t.PRCards, t.asks)
}

func (l *liveCards) draft(channel, thread string) *liveThread {
	if t, ok := l.threads[threadKey(channel, thread)]; ok && thread != "" {
		c := *t
		c.Timeline, c.Asks, c.Answers, c.Waiting, c.Groups, c.Keyed = slices.Clone(t.Timeline), slices.Clone(t.Asks), maps.Clone(t.Answers), maps.Clone(t.Waiting), maps.Clone(t.Groups), maps.Clone(t.Keyed)
		c.PRCards, c.Copies, c.Carries = slices.Clone(t.PRCards), slices.Clone(t.Copies), slices.Clone(t.Carries)
		return &c
	}
	return &liveThread{Channel: channel, Thread: thread}
}

type heldCard struct {
	thread *liveThread
	card   *liveMessage
}

func (l *liveCards) holding(channel string, threads []string, card statusCard) *liveThread {
	prior, _ := l.holders(channel, threads, card)
	return prior
}

func (l *liveCards) holders(channel string, threads []string, card statusCard) (*liveThread, []heldCard) {
	if card.Waits == "" && card.Group == "" && card.Key == "" {
		return nil, nil
	}
	holds := func(t *liveThread) bool {
		_, answered := t.Answers[card.Waits]
		return t.kept(card) != nil || (card.Waits != "" && answered)
	}
	if !card.driveQuestion() {
		return l.sessionThread(channel, threads, holds), nil
	}
	var held []*liveThread
	for _, t := range l.threads {
		if t.Channel == channel && holds(t) {
			held = append(held, t)
		}
	}
	if len(held) == 0 {
		return nil, nil
	}
	slices.SortFunc(held, func(a, b *liveThread) int { return cmp.Compare(b.heldAt(card.Waits), a.heldAt(card.Waits)) })
	var older []heldCard
	for _, t := range held[1:] {
		if m := t.Waiting[card.Waits]; m != nil && t.asks(m) {
			older = append(older, heldCard{t, m})
		}
	}
	return held[0], older
}

func (t *liveThread) heldAt(waits string) string {
	if m := t.Waiting[waits]; m != nil {
		return m.TS
	}
	return t.Thread
}

func (l *liveCards) reword(channel string, threads []string, card statusCard) (cardRef, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	t := l.holding(channel, threads, card)
	if t == nil {
		return cardRef{}, false
	}
	asked := t.kept(card)
	if _, done := t.Answers[card.Waits]; asked == nil || !done {
		return cardRef{}, false
	}
	card.Status, card.Replaces, card.Images = asked.Card.Status, "", asked.Card.Images
	if !sameCard(card.unworded(), asked.Card.unworded()) {
		return cardRef{}, false
	}
	ts := asked.TS
	for _, m := range t.messages() {
		if m != nil && m.TS == ts {
			m.Card, m.Edited = card, false
		}
	}
	t.Updated = l.now()
	l.save([]string{threadKey(t.Channel, t.Thread)}, nil)
	return cardRef{channel, t.Thread, ts}, true
}

func (l *liveCards) holds(channel string, threads []string, card statusCard) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.holding(channel, threads, card) != nil
}

func (l *liveCards) editByHand(channel, ts string, edit func() error) error {
	key := ""
	l.mu.Lock()
	for k, t := range l.threads {
		if t.Channel == channel && slices.ContainsFunc(append(t.messages(), t.Root), func(m *liveMessage) bool { return m != nil && m.TS == ts }) {
			key = k
			break
		}
	}
	l.mu.Unlock()
	if key == "" {
		return edit()
	}
	defer l.drawLock(key)()
	if err := edit(); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if t, ok := l.threads[key]; ok {
		for _, m := range append(t.messages(), t.Root) {
			if m != nil && m.TS == ts {
				m.Edited = true
			}
		}
		l.save([]string{key}, nil)
	}
	return nil
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

func (l *liveCards) rootPRCard(channel string, matches func(*liveThread, *liveMessage) bool) (cardRef, bool) {
	var newest *liveMessage
	var found cardRef
	for _, t := range l.threads {
		if t.Channel != channel {
			continue
		}
		for _, m := range t.PRCards {
			if m.TS == t.Thread && matches(t, m) && (newest == nil || m.TS > newest.TS) {
				newest, found = m, cardRef{t.Channel, t.Thread, m.TS}
			}
		}
	}
	return found, newest != nil
}

func (l *liveCards) posted(channel, thread string, card statusCard) (same cardRef, found bool, others []cardRef) {
	keys := card.prKeys()
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, t := range l.threads {
		if t.Channel != channel || thread != "" && t.Thread != thread {
			continue
		}
		for _, m := range t.PRCards {
			if !m.cites(keys) || t.retired(m) && !t.revives(m, card) || thread == "" && m.TS != t.Thread {
				continue
			}
			ref := cardRef{t.Channel, t.Thread, m.TS}
			if maps.Equal(m.Card.prKeys(), keys) && (!found || ref.ts > same.ts) {
				if found {
					others = append(others, same)
				}
				same, found = ref, true
				continue
			}
			others = append(others, ref)
		}
	}
	return same, found, others
}

func (l *liveCards) retired(card cardRef) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	t, m := l.liveMessage(card.channel, card.ts)
	return m != nil && t.retired(m)
}

func (l *liveCards) staysRetired(card cardRef, drawn statusCard) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	t, m := l.liveMessage(card.channel, card.ts)
	return m != nil && t.retired(m) && !t.revives(m, drawn)
}

func (l *liveCards) changedSinceAnswer(card cardRef, drawn statusCard) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	t, m := l.liveMessage(card.channel, card.ts)
	if m == nil {
		return ""
	}
	return t.changedSinceAnswer(m, drawn)
}

func (l *liveCards) heads(card cardRef) map[string]string {
	l.mu.Lock()
	defer l.mu.Unlock()
	heads := map[string]string{}
	_, m := l.liveMessage(card.channel, card.ts)
	if m == nil {
		return heads
	}
	for _, p := range m.Card.allPRs() {
		if id, ok := parseRef(p.URL); ok && p.Head != "" {
			heads[fmt.Sprintf("%s/%s#%d", id.Owner, id.Repo, id.Number)] = p.Head
		}
	}
	return heads
}

func (l *liveCards) linkCopy(copied cardRef) {
	l.mu.Lock()
	defer l.mu.Unlock()
	t := l.draft(copied.channel, copied.thread)
	for i := range t.Copies {
		if t.Copies[i].TS == copied.ts {
			t.Copies[i].Link = true
		}
	}
	l.commit(t)
}

func (l *liveCards) copyIn(channel, thread, card string) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	t, ok := l.threads[threadKey(channel, thread)]
	if !ok {
		return ""
	}
	at := slices.IndexFunc(t.Copies, func(c liveCardCopy) bool { return c.Card == card && !c.Link })
	if at < 0 {
		return ""
	}
	return t.Copies[at].TS
}

func (l *liveCards) pickAsking(channel, pr string) (cardRef, bool) {
	for _, t := range l.threads {
		if t.Channel != channel {
			continue
		}
		for _, m := range t.Waiting {
			if pr != "" && m.Card.offer().pr == pr && t.open(m) {
				return cardRef{t.Channel, t.Thread, m.TS}, true
			}
		}
	}
	return cardRef{}, false
}

func (l *liveCards) pickOffering(channel string, offer cardOffer) (cardRef, bool) {
	for _, t := range l.threads {
		if t.Channel != channel {
			continue
		}
		for _, m := range t.Waiting {
			if offer.same(m.Card.offer()) {
				return cardRef{t.Channel, t.Thread, m.TS}, true
			}
		}
	}
	return cardRef{}, false
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

func (l *liveCards) dropReplaced(into *liveThread, replaces string, placed *liveMessage) []string {
	var changed []string
	if placed.TS != replaces {
		return nil
	}
	for _, t := range append(slices.Collect(maps.Values(l.threads)), into) {
		for _, m := range t.messages() {
			if t.Channel == into.Channel && m != nil && m != placed && m.TS == placed.TS {
				t.drop(m)
				changed = append(changed, threadKey(t.Channel, t.Thread))
			}
		}
	}
	return changed
}

func (l *liveCards) commit(t *liveThread, changed ...string) {
	t.Drawn = l.now()
	l.threads[threadKey(t.Channel, t.Thread)] = t
	changed = append(changed, threadKey(t.Channel, t.Thread))
	changed = append(changed, l.prune()...)
	l.save(changed, nil)
	l.wake()
}

func (l *liveCards) wake() {
	select {
	case l.wakeCh <- struct{}{}:
	default:
	}
}

func (l *liveCards) claim(channel, thread, askID string, ans askAnswer) (bool, askAnswer, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	t, ok := l.threads[threadKey(channel, thread)]
	if !ok {
		t = &liveThread{Channel: channel, Thread: thread, Updated: l.now()}
		l.threads[threadKey(channel, thread)] = t
	}
	if prior, done := t.Answers[askID]; done {
		return false, prior, nil
	}
	if t.Answers == nil {
		t.Answers = map[string]askAnswer{}
	}
	t.Answers[askID] = ans
	l.save([]string{threadKey(channel, thread)}, nil)
	if !ans.UndoUntil.IsZero() {
		select {
		case l.dueCh <- ans.UndoUntil:
		default:
		}
	}
	return true, ans, nil
}

type undoneAnswer struct {
	card       cardRef
	waits      string
	answer     askAnswer
	question   string
	supersedes []string
}

func (l *liveCards) undo(channel, tap string) []undoneAnswer {
	return l.undoWhere(func(t *liveThread, ans askAnswer, _ []*liveMessage) bool {
		return t.Channel == channel && ans.Tap == tap
	})
}

func (l *liveCards) undoReply(card cardRef, words string) []undoneAnswer {
	return l.undoWhere(func(t *liveThread, ans askAnswer, answered []*liveMessage) bool {
		return t.Channel == card.channel && t.Thread == card.thread && ans.Other && !ans.Withdrawn && ans.Label == words &&
			slices.ContainsFunc(answered, func(m *liveMessage) bool { return m.TS == card.ts })
	})
}

func (l *liveCards) undoWhere(match func(t *liveThread, ans askAnswer, answered []*liveMessage) bool) []undoneAnswer {
	l.mu.Lock()
	defer l.mu.Unlock()
	var undone []undoneAnswer
	var changed []string
	for _, t := range l.threads {
		for waits, ans := range t.Answers {
			answered := t.answered(waits)
			if !match(t, ans, answered) {
				continue
			}
			delete(t.Answers, waits)
			changed = append(changed, threadKey(t.Channel, t.Thread))
			for _, m := range answered {
				m.Tapped = ""
				if !slices.ContainsFunc(undone, func(u undoneAnswer) bool { return u.card == cardRef{t.Channel, t.Thread, m.TS} }) {
					undone = append(undone, undoneAnswer{cardRef{t.Channel, t.Thread, m.TS}, waits, ans, m.Card.Title, m.Card.Supersedes})
				}
			}
		}
	}
	l.save(changed, nil)
	return undone
}

type closedUndo struct {
	card cardRef
	line string
}

func (l *liveCards) closeUndos(now time.Time) []closedUndo {
	l.mu.Lock()
	defer l.mu.Unlock()
	var closed []closedUndo
	var changed []string
	for _, t := range l.threads {
		for waits, ans := range t.Answers {
			if ans.UndoUntil.IsZero() || now.Before(ans.UndoUntil) {
				continue
			}
			ans.Tap, ans.UndoUntil = "", time.Time{}
			t.Answers[waits] = ans
			changed = append(changed, threadKey(t.Channel, t.Thread))
			for _, m := range t.answered(waits) {
				closed = append(closed, closedUndo{cardRef{t.Channel, t.Thread, m.TS}, m.Tapped})
			}
		}
	}
	l.save(changed, nil)
	return slices.CompactFunc(closed, func(a, b closedUndo) bool { return a.card == b.card })
}

func (l *liveCards) undoing(card cardRef) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	t, ok := l.threads[threadKey(card.channel, card.thread)]
	if !ok {
		return ""
	}
	for waits, ans := range t.Answers {
		if l.now().Before(ans.UndoUntil) && slices.ContainsFunc(t.answered(waits), func(m *liveMessage) bool { return m.TS == card.ts }) {
			return waits
		}
	}
	return ""
}

func (l *liveCards) stillAnswered(card cardRef, q questionAnswer) bool {
	ans, done := l.answer(card.channel, card.thread, q.Waits)
	return done && ans.Label == q.Label && ans.At.Truncate(time.Second).Equal(slackTime(q.ActionTS))
}

func (l *liveCards) release(channel, thread, askID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if t, ok := l.threads[threadKey(channel, thread)]; ok {
		if _, answered := t.Answers[askID]; answered {
			delete(t.Answers, askID)
			l.save([]string{threadKey(channel, thread)}, nil)
		}
	}
}

func (l *liveCards) shownImages(channel string, card statusCard) []cardImage {
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

func (l *liveCards) prCardImages(card cardRef, hash string) []cardImage {
	l.mu.Lock()
	defer l.mu.Unlock()
	t, ok := l.threads[threadKey(card.channel, card.thread)]
	if !ok {
		return nil
	}
	for _, m := range t.PRCards {
		if m.TS == card.ts && m.Card.ImagesHash == hash {
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
	if ans, done := t.Answers[askID]; done && !ans.Drawn {
		ans.Drawn = true
		t.Answers[askID] = ans
		l.save([]string{threadKey(channel, thread)}, nil)
	}
}

func (l *liveCards) oweTap(channel, thread, askID string, tap *owedTap) {
	l.mu.Lock()
	defer l.mu.Unlock()
	t, ok := l.threads[threadKey(channel, thread)]
	if !ok {
		return
	}
	if ans, done := t.Answers[askID]; done && (ans.Owed != nil || tap != nil) {
		ans.Owed = tap
		t.Answers[askID] = ans
		l.save([]string{threadKey(channel, thread)}, nil)
	}
}

type dueTap struct {
	card cardRef
	tap  owedTap
}

func (l *liveCards) owedTaps(now time.Time, running time.Duration) ([]dueTap, time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var (
		due  []dueTap
		next time.Time
	)
	var changed []string
	rescheduled := map[*owedTap]bool{}
	for _, t := range l.threads {
		for _, ans := range t.Answers {
			switch {
			case ans.Owed == nil:
			case ans.Owed.Due.After(now):
				next = earliest(next, ans.Owed.Due)
				if rescheduled[ans.Owed] {
					changed = append(changed, threadKey(t.Channel, t.Thread))
				}
			default:
				due = append(due, dueTap{cardRef{t.Channel, t.Thread, ans.Owed.Card}, *ans.Owed})
				ans.Owed.Due = now.Add(running)
				rescheduled[ans.Owed] = true
				changed = append(changed, threadKey(t.Channel, t.Thread))
			}
		}
	}
	l.save(changed, nil)
	return due, next
}

func (l *liveCards) answerDelivered(channel, thread, askID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	t, ok := l.threads[threadKey(channel, thread)]
	if !ok {
		return
	}
	if ans, done := t.Answers[askID]; done && ans.Undelivered != "" {
		ans.Undelivered = ""
		t.Answers[askID] = ans
		l.save([]string{threadKey(channel, thread)}, nil)
	}
}

func (l *liveCards) threadAbout(channel string, threads []string, waits string) (string, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	card := statusCard{Waits: waits}
	for _, t := range l.threads {
		if m := t.Waiting[waits]; t.Channel == channel && m != nil && m.Card.driveQuestion() {
			card.Drive = m.Card.Drive
		}
	}
	if t := l.holding(channel, threads, card); t != nil {
		return t.Thread, true
	}
	return "", false
}

func (l *liveCards) waitingAt(channel, thread, ts string) (string, liveMessage, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	t, ok := l.threads[threadKey(channel, thread)]
	if !ok {
		return "", liveMessage{}, false
	}
	for waits, m := range t.Waiting {
		if m.TS == ts {
			return waits, *m, true
		}
	}
	return "", liveMessage{}, false
}

func (l *liveCards) stillAsks(channel, thread, waits, question string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if t, ok := l.threads[threadKey(channel, thread)]; ok && t.Waiting[waits] != nil && t.Waiting[waits].Card.Title != question {
		return errCardMoved
	}
	return nil
}

func (l *liveCards) prCardAt(channel, thread, ts string) (string, liveMessage, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	t, ok := l.threads[threadKey(channel, thread)]
	if !ok {
		return "", liveMessage{}, false
	}
	for _, m := range t.PRCards {
		if m.TS == ts && m.Card.Waits != "" && len(m.Card.refIDs()) > 0 {
			return m.Card.Waits, *m, true
		}
	}
	return "", liveMessage{}, false
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

func (l *liveCards) lastAnswer(channel, thread string) time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	var last time.Time
	if t, ok := l.threads[threadKey(channel, thread)]; ok {
		for _, ans := range t.Answers {
			if !ans.Withdrawn && ans.Undelivered == "" && ans.At.After(last) {
				last = ans.At
			}
		}
	}
	return last
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
			_, pages := m.Card.layout(t.view(m, now, l.canvas()))
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
	carrier  bool
	held     []string
	more     [][]slack.Block
}

func (l *liveCards) write(t *liveThread, m *liveMessage, now time.Time, root bool) cardWrite {
	text, blocks := t.replyMessage(m, now, l.canvas())
	prs := m.Card.prEntities(t.view(m, now, l.canvas()))
	if root {
		text, blocks = t.rootMessage(now, l.canvas())
		prs = nil
	}
	w := cardWrite{m: m, ts: m.TS, text: text, blocks: blocks}
	if m.TS == t.Thread {
		var carried []slack.WorkObjectEntity
		w.blocks, w.more, carried = l.withCarried(t, blocks, now)
		w.carrier, w.held = true, slices.Clone(t.Continued)
		prs = append(prs, carried...)
	}
	if m.Tapped == "" {
		w.entities = m.Card.entities(prs...)
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
	return slices.DeleteFunc(writes, func(w cardWrite) bool { return w.m.Edited })
}

func (l *liveCards) flush(ctx context.Context, api slackAPI, t *liveThread, writes []cardWrite, now time.Time, whole bool) (map[string]bool, error) {
	redrawn := map[string]bool{}
	var gone []*liveMessage
	var errs []error
	var continued []string
	carried := false
	for _, w := range writes {
		err := l.update(ctx, api, t.Channel, w)
		if err == nil && w.carrier {
			held := w.held
			if carried {
				held = continued
			}
			var more error
			continued, more = l.continueCarried(ctx, api, t.Channel, t.Thread, held, w.more)
			carried = true
			if more != nil {
				errs = append(errs, fmt.Errorf("continue card %s/%s in its thread: %w", t.Channel, w.ts, more))
			}
		}
		switch {
		case err != nil && goneCardCodes[errorCode(err)]:
			l.log.Printf("live card %s/%s: %s; no longer tracking it", t.Channel, w.ts, errorCode(err))
			gone = append(gone, w.m)
		case err != nil:
			errs = append(errs, fmt.Errorf("redraw card %s/%s: %w", t.Channel, w.ts, err))
		default:
			redrawn[w.ts] = true
		}
		if _, limited := errors.AsType[*slack.RateLimitedError](err); limited {
			break
		}
	}
	l.mu.Lock()
	for _, m := range gone {
		t.drop(m)
	}
	if carried {
		t.Continued = continued
	}
	if whole {
		t.Drawn, t.Stale = now, len(errs) > 0
	}
	l.save([]string{threadKey(t.Channel, t.Thread)}, nil)
	carriers := l.carriersOf(t, redrawn)
	l.mu.Unlock()
	for _, carrier := range carriers {
		if _, err := l.redraw(ctx, api, carrier.channel, carrier.thread, carrier.ts, now); err != nil {
			errs = append(errs, fmt.Errorf("redraw card %s/%s carrying a redrawn card: %w", carrier.channel, carrier.ts, err))
		}
	}
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
	removed := l.prune()
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
	var removedRefs []string
	for k := range l.refs {
		if !seen[k] && !l.cited(k) {
			delete(l.refs, k)
			removedRefs = append(removedRefs, k)
		}
	}
	l.save(removed, removedRefs)
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
		return l.client.pullRequests(ctx, refs)
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
		due := l.now().Add(l.tick(ctx))
		timer := time.NewTimer(due.Sub(l.now()))
	waiting:
		for {
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-l.wakeCh:
				timer.Stop()
				break waiting
			case at := <-l.dueCh:
				if at.Before(due) {
					due = at
					timer.Reset(due.Sub(l.now()))
				}
			case <-timer.C:
				break waiting
			}
		}
	}
}

func (l *liveCards) tick(ctx context.Context) time.Duration {
	id, err := loadIdentity(ctx, l.plane.DB())
	if err != nil {
		return refMaxBackoff
	}
	now := l.now()
	l.redrawClosedUndos(ctx, id, l.closeUndos(now), now)
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
		var changedRefs []string
		for k, s := range states {
			if prior, known := l.refs[k]; !known || prior != s {
				moved[k] = s.movedFrom(l.refs[k])
				l.refs[k] = s
				changed[k] = true
				changedRefs = append(changedRefs, k)
			}
		}
		l.save(nil, changedRefs)
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
	maps.Copy(changed, l.collapseEnded())
	if until := l.redrawChanged(ctx, l.newAPI(id.Token), changed, now); !until.IsZero() {
		next = earliest(next, until)
	}
	l.recardMoved(ctx, id, moved)
	return max(next.Sub(now), time.Second)
}

func (l *liveCards) collapseEnded() map[string]bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	ended := map[string]bool{}
	var changed []string
	for _, t := range l.threads {
		for _, m := range t.PRCards {
			if m.Ended || m.Card.ended(l.refs) == "" {
				continue
			}
			m.Ended = true
			changed = append(changed, threadKey(t.Channel, t.Thread))
			for _, id := range m.Card.refIDs() {
				ended[id.key()] = true
			}
		}
	}
	l.save(changed, nil)
	return ended
}

func (l *liveCards) redrawClosedUndos(ctx context.Context, id identity, closed []closedUndo, now time.Time) {
	api := l.newAPI(id.Token)
	for _, c := range closed {
		if _, err := l.redraw(ctx, api, c.card.channel, c.card.thread, c.card.ts, now); err != nil {
			l.log.Printf("card %s: its ten minutes to undo closed, but it still shows Undo: %v", c.card.ts, err)
		}
		l.markCopies(ctx, id, c.card, c.line)
	}
}

func (l *liveCards) redrawChanged(ctx context.Context, api slackAPI, changed map[string]bool, now time.Time) time.Time {
	l.mu.Lock()
	keys := slices.Sorted(maps.Keys(l.threads))
	l.mu.Unlock()
	ctx = withoutRateWait(ctx)
	for i, k := range keys {
		rate, limited := errors.AsType[*slack.RateLimitedError](l.redrawMoved(ctx, api, k, changed, now))
		if !limited {
			continue
		}
		l.mu.Lock()
		for _, k := range keys[i:] {
			if l.undrawn[k] == nil {
				l.undrawn[k] = map[string]bool{}
			}
			maps.Copy(l.undrawn[k], changed)
		}
		l.mu.Unlock()
		l.log.Printf("live cards: Slack limits chat.update for %s, so %d threads redraw after it", rate.RetryAfter, len(keys)-i)
		return now.Add(rate.RetryAfter)
	}
	return time.Time{}
}

func (l *liveCards) redrawMoved(ctx context.Context, api slackAPI, key string, changed map[string]bool, now time.Time) error {
	defer l.drawLock(key)()
	l.mu.Lock()
	t, ok := l.threads[key]
	if !ok {
		l.mu.Unlock()
		return nil
	}
	if owed := l.undrawn[key]; owed != nil {
		merged := maps.Clone(owed)
		maps.Copy(merged, changed)
		changed = merged
	}
	whole := t.Stale || t.cites(changed) || t.expiredSinceDrawn(now)
	var writes, prCards []cardWrite
	if whole {
		writes = l.plan(t, now, "")
	}
	for _, m := range t.PRCards {
		if m.cites(changed) && !m.Edited {
			prCards = append(prCards, l.write(t, m, now, false))
		}
	}
	l.mu.Unlock()
	var errs []error
	if whole {
		if _, err := l.flush(ctx, api, t, writes, now, true); err != nil {
			l.log.Printf("live card %s: %v", key, err)
			errs = append(errs, err)
		}
	}
	if len(prCards) > 0 {
		if _, err := l.flush(ctx, api, t, prCards, now, false); err != nil {
			l.log.Printf("live card %v", err)
			errs = append(errs, err)
		}
	}
	err := errors.Join(errs...)
	if _, limited := errors.AsType[*slack.RateLimitedError](err); !limited {
		l.mu.Lock()
		delete(l.undrawn, key)
		l.mu.Unlock()
	}
	return err
}

func (l *liveCards) recardMoved(ctx context.Context, id identity, moved map[string]bool) {
	if len(id.Config.PRLinks.CardCommand) == 0 {
		return
	}
	for _, posted := range l.postedPRCards(true) {
		if slices.ContainsFunc(posted.prs, func(pr refID) bool { return moved[pr.key()] }) {
			l.recard(ctx, id, posted.card, posted.prs[0])
		}
	}
}

type postedPRCard struct {
	card cardRef
	prs  []refID
}

func (p postedPRCard) carries(pr refID) bool {
	return slices.ContainsFunc(p.prs, func(r refID) bool { return r.key() == pr.key() })
}

func (l *liveCards) postedPRCards(withdrawn bool) []postedPRCard {
	l.mu.Lock()
	defer l.mu.Unlock()
	var posted []postedPRCard
	for _, t := range l.threads {
		for _, m := range t.PRCards {
			if ids := m.Card.refIDs(); len(ids) > 0 && (!t.retired(m) || withdrawn && t.withdrawn(m)) {
				posted = append(posted, postedPRCard{cardRef{t.Channel, t.Thread, m.TS}, ids})
			}
		}
	}
	return posted
}

func (l *liveCards) openPRCards(pr refID) []offeredCard {
	l.mu.Lock()
	defer l.mu.Unlock()
	var open []offeredCard
	for _, t := range l.threads {
		for _, m := range t.PRCards {
			if t.asks(m) && m.cites(map[string]bool{pr.key(): true}) {
				open = append(open, offeredCard{ref: cardRef{t.Channel, t.Thread, m.TS}, waits: m.Card.Waits, question: m.Card.Title})
			}
		}
	}
	return open
}

// uncarded lists the PRs in waiting that none of the cards carries.
func (l *liveCards) uncarded(cards []cardRef, waiting []refID) []refID {
	l.mu.Lock()
	defer l.mu.Unlock()
	carried := map[string]bool{}
	for _, card := range cards {
		if _, m := l.liveMessage(card.channel, card.ts); m != nil {
			for _, id := range m.Card.refIDs() {
				carried[id.key()] = true
			}
		}
	}
	return slices.DeleteFunc(slices.Clone(waiting), func(pr refID) bool { return carried[pr.key()] })
}

// cardsListing lists every card that lists pr among its PRs, its own PR cards aside.
func (l *liveCards) cardsListing(pr refID) []cardRef {
	l.mu.Lock()
	defer l.mu.Unlock()
	var listing []cardRef
	for _, t := range l.threads {
		for _, m := range t.messages() {
			if m == nil || slices.Contains(t.PRCards, m) {
				continue
			}
			card := cardRef{t.Channel, t.Thread, m.TS}
			if !slices.Contains(listing, card) && slices.ContainsFunc(m.Card.allPRs(), func(p *cardPR) bool { r, ok := parseRef(p.URL); return ok && r.key() == pr.key() }) {
				listing = append(listing, card)
			}
		}
	}
	return listing
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
	revived := t.revives(m, drawn)
	if t.retired(m) && !revived {
		l.mu.Unlock()
		return nil
	}
	if revived {
		delete(t.Answers, m.Card.Waits)
		m.Tapped = ""
	}
	switch {
	case drawn.Waits == "":
		drawn.Waits, drawn.Options, drawn.Recommended = m.Card.Waits, m.Card.Options, m.Card.Recommended
		if t.asks(m) {
			m.Tapped = cardNoLongerWaits
		}
	case m.Tapped == cardNoLongerWaits || drawn.Waits != m.Card.Waits:
		m.Tapped = ""
	}
	m.Card, m.Final, m.Edited = drawn, drawn.Waits == "", false
	t.Updated = now
	w := l.write(t, m, now, false)
	l.mu.Unlock()
	if _, err := l.flush(ctx, api, t, []cardWrite{w}, now, false); err != nil || !revived {
		return err
	}
	if err := unmarkWithdrawn(ctx, api, card); err != nil {
		l.log.Printf("PR card %s is back in place, but its withdrawn mark stays: %v", card.ts, err)
	}
	return nil
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

func (l *liveCards) threadWaits(channel, thread string) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if waits := l.waitsAt(channel, thread); waits != "" {
		return waits
	}
	t, ok := l.threads[threadKey(channel, thread)]
	if !ok {
		return ""
	}
	for _, c := range t.Copies {
		if c.TS == thread {
			return l.waitsAt(channel, c.Card)
		}
	}
	return ""
}

func (l *liveCards) waitsAt(channel, ts string) string {
	t, ok := l.threads[threadKey(channel, ts)]
	if !ok {
		return ""
	}
	for _, m := range append(t.messages(), t.Root) {
		if m != nil && m.TS == ts {
			return m.Card.Waits
		}
	}
	return ""
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

func (l *liveCards) waitingPR(channel, thread, waits string) (refID, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	t, ok := l.threads[threadKey(channel, thread)]
	if !ok || t.Waiting[waits] == nil {
		return refID{}, false
	}
	if ids := t.Waiting[waits].Card.refIDs(); len(ids) > 0 {
		return ids[0], true
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
