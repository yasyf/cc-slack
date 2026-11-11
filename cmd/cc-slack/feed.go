package main

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/slack-go/slack"

	"github.com/yasyf/cc-interact/event"
	"github.com/yasyf/cc-interact/store"
)

const (
	eventFeed = "slack.feed"

	feedAlerts   = "alerts"
	feedReleases = "releases"
	feedPosts    = "posts"

	monitorEventType = "monitor_thread"
	monitorKey       = "monitor:"
	sentryKey        = "sentry:"
	editedSubtype    = "message_changed"
	alertQuiet       = 30 * time.Minute
)

var feedKinds = []string{feedAlerts, feedReleases, feedPosts}

var (
	monitorPrefix = regexp.MustCompile(`^(?:\*Monitor alert summary:\*|[A-Z][A-Za-z-]*(?: [A-Z][A-Za-z-]*)?:)\s*`)
	sentryIssue   = regexp.MustCompile(`\.sentry\.io/issues/(\d+)`)
	emojiCode     = regexp.MustCompile(`:[a-z0-9_+-]+:`)
	releaseName   = regexp.MustCompile(`^(?:Hotfix )?[a-z]+-[a-z]+ `)
	bareLink      = regexp.MustCompile(`<([^<>|\s][^<>|]*)>`)
	plainMarks    = strings.NewReplacer("*", "", "`", "", "&amp;", "&", "&lt;", "<", "&gt;", ">")
)

var feedSchema = store.Schema{DDL: `
CREATE TABLE feeds (
  watch_id INTEGER PRIMARY KEY,
  kinds    TEXT NOT NULL,
  match    TEXT NOT NULL
);
CREATE TABLE alerts (
  channel_id TEXT NOT NULL,
  key        TEXT NOT NULL,
  name       TEXT NOT NULL,
  link       TEXT NOT NULL,
  opened_ts  TEXT NOT NULL,
  latest_ts  TEXT NOT NULL,
  quiet_ts   TEXT NOT NULL,
  refired    INTEGER NOT NULL,
  PRIMARY KEY (channel_id, key)
);
CREATE TABLE releases (
  channel_id TEXT NOT NULL,
  ts         TEXT NOT NULL,
  PRIMARY KEY (channel_id, ts)
);
`}

type feed struct {
	kinds []string
	match string
}

func (f feed) wants(it feedItem) bool {
	return slices.Contains(f.kinds, it.kind) && strings.Contains(strings.ToLower(it.line), strings.ToLower(f.match))
}

type fedWatch struct {
	watch
	feed
}

type feedItem struct {
	kind    string
	key     string
	line    string
	release *releasePost
}

type releasePost struct {
	Name    string `json:"name"`
	Line    string `json:"line"`
	Link    string `json:"link"`
	running bool
}

type feedPayload struct {
	Type    string `json:"type"`
	WatchID int64  `json:"watch_id"`
	Line    string `json:"line"`
}

type alert struct {
	channelID string
	key       string
	name      string
	link      string
	opened    string
	latest    string
	quiet     string
	refired   int
}

func (a alert) closed() (feedItem, bool) {
	monitor, ok := strings.CutPrefix(a.key, monitorKey)
	if !ok {
		return feedItem{}, false
	}
	open := strings.TrimSuffix(max(tsTime(a.quiet).Sub(tsTime(a.opened)), time.Minute).Round(time.Minute).String(), "0s")
	refired := ""
	switch a.refired {
	case 0:
	case 1:
		refired = ", refired once"
	default:
		refired = fmt.Sprintf(", refired %d times", a.refired)
	}
	return feedItem{
		kind: feedAlerts,
		key:  a.key + ":" + a.opened + ":closed",
		line: fmt.Sprintf("ALERT closed monitor %s %q after %s%s %s", monitor, a.name, open, refired, a.link),
	}, true
}

type releaseLine struct {
	state   string
	line    string
	running bool
}

func (r releaseLine) item(ts, link string) feedItem {
	name := strings.Fields(strings.TrimPrefix(r.state, "Hotfix "))[0]
	return feedItem{kind: feedReleases, key: "release:" + ts + ":" + r.state, line: "RELEASE " + r.line + " " + link,
		release: &releasePost{Name: name, Line: r.line, Link: link, running: r.running}}
}

func parseRelease(text string) (releaseLine, bool) {
	first := firstLine(text)
	if !strings.HasPrefix(first, "*:") || !strings.HasSuffix(first, "*") {
		return releaseLine{}, false
	}
	segments := strings.Split(strings.Trim(first, "*"), " · ")
	state := plainLine(strings.Join(strings.Fields(emojiCode.ReplaceAllString(segments[0], "")), " "))
	if !releaseName.MatchString(state) {
		return releaseLine{}, false
	}
	kept := []string{state}
	running := true
	for _, s := range segments[1:] {
		running = running && !strings.HasPrefix(s, "finished ")
		if !strings.Contains(s, "<@") {
			kept = append(kept, plainLine(s))
		}
	}
	return releaseLine{state: state, line: strings.Join(kept, " · "), running: running}, true
}

func monitorName(text string) string {
	return clipRunes(plainLine(monitorPrefix.ReplaceAllString(firstLine(text), "")), botLineLimit)
}

func firstLine(text string) string {
	first, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	return strings.TrimSpace(first)
}

func plainLine(s string) string {
	return strings.TrimSpace(plainMarks.Replace(bareLink.ReplaceAllString(waitsLabel.ReplaceAllString(s, "$1"), "$1")))
}

func tsTime(ts string) time.Time {
	sec, micro := splitTS(ts)
	return time.Unix(sec, micro*1000)
}

type feedStore struct {
	st *store.Store
}

func feedsPath() string { return filepath.Join(appPaths().StateDir(), "feeds.db") }

func openFeeds(ctx context.Context, path string) (*feedStore, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("feeds dir: %w", err)
	}
	st, err := store.Open(ctx, path, feedSchema, store.WithUnsupportedSchema(store.ArchiveUnsupportedSchema))
	if err != nil {
		return nil, fmt.Errorf("open feeds %s: %w", path, err)
	}
	return &feedStore{st: st}, nil
}

func (f *feedStore) Close() error { return f.st.Close() }

func (f *feedStore) subscribe(ctx context.Context, watchID int64, kinds []string, match string) error {
	if _, err := f.st.DB().ExecContext(ctx, `INSERT INTO feeds(watch_id, kinds, match) VALUES (?, ?, ?)
		ON CONFLICT(watch_id) DO UPDATE SET kinds = excluded.kinds, match = excluded.match`, watchID, strings.Join(kinds, ","), match); err != nil {
		return fmt.Errorf("subscribe watch %d: %w", watchID, err)
	}
	return nil
}

func (f *feedStore) fed(ctx context.Context, watches []watch) ([]fedWatch, error) {
	rows, err := f.st.DB().QueryContext(ctx, `SELECT watch_id, kinds, match FROM feeds`)
	if err != nil {
		return nil, fmt.Errorf("load feeds: %w", err)
	}
	defer func() { _ = rows.Close() }()
	feeds := map[int64]feed{}
	for rows.Next() {
		var (
			id    int64
			kinds string
			fd    feed
		)
		if err := rows.Scan(&id, &kinds, &fd.match); err != nil {
			return nil, fmt.Errorf("scan feed: %w", err)
		}
		fd.kinds = strings.Split(kinds, ",")
		feeds[id] = fd
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate feeds: %w", err)
	}
	var out []fedWatch
	for _, w := range watches {
		if fd, ok := feeds[w.ID]; ok {
			out = append(out, fedWatch{watch: w, feed: fd})
		}
	}
	return out, nil
}

func (f *feedStore) alert(ctx context.Context, channelID, key string) (alert, bool, error) {
	a := alert{channelID: channelID, key: key}
	err := f.st.DB().QueryRowContext(ctx, `SELECT name, link, opened_ts, latest_ts, quiet_ts, refired FROM alerts WHERE channel_id = ? AND key = ?`,
		channelID, key).Scan(&a.name, &a.link, &a.opened, &a.latest, &a.quiet, &a.refired)
	if errors.Is(err, sql.ErrNoRows) {
		return a, false, nil
	}
	if err != nil {
		return a, false, fmt.Errorf("load alert %s/%s: %w", channelID, key, err)
	}
	return a, true, nil
}

func (f *feedStore) save(ctx context.Context, a alert) error {
	if _, err := f.st.DB().ExecContext(ctx, `INSERT OR REPLACE INTO alerts(channel_id, key, name, link, opened_ts, latest_ts, quiet_ts, refired)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, a.channelID, a.key, a.name, a.link, a.opened, a.latest, a.quiet, a.refired); err != nil {
		return fmt.Errorf("save alert %s/%s: %w", a.channelID, a.key, err)
	}
	return nil
}

func (f *feedStore) monitor(ctx context.Context, channelID, monitorID, transition, name, link, ts string) (feedItem, bool, error) {
	a, found, err := f.alert(ctx, channelID, monitorKey+monitorID)
	if err != nil {
		return feedItem{}, false, err
	}
	recovery := strings.Contains(transition, "recovery")
	switch {
	case !found && recovery:
		return feedItem{}, false, nil
	case !found:
		a.name, a.link, a.opened, a.latest = name, link, ts, ts
		it := feedItem{kind: feedAlerts, key: a.key + ":" + ts, line: fmt.Sprintf("ALERT open monitor %s %q (%s) %s", monitorID, name, transition, link)}
		return it, true, f.save(ctx, a)
	case !tsLess(a.latest, ts):
		return feedItem{}, false, nil
	case recovery:
		a.quiet = cmp.Or(a.quiet, ts)
	case a.quiet != "":
		a.quiet = ""
		a.refired++
	}
	a.latest = ts
	return feedItem{}, false, f.save(ctx, a)
}

func (f *feedStore) sentry(ctx context.Context, channelID, issue, title, link, ts string) (feedItem, bool, error) {
	a, found, err := f.alert(ctx, channelID, sentryKey+issue)
	if err != nil || (found && !tsLess(a.latest, ts)) {
		return feedItem{}, false, err
	}
	if found {
		a.refired++
		a.latest, a.quiet = ts, ts
		return feedItem{}, false, f.save(ctx, a)
	}
	a.name, a.link, a.opened, a.latest, a.quiet = title, link, ts, ts, ts
	return feedItem{kind: feedAlerts, key: a.key + ":" + ts, line: fmt.Sprintf("ALERT sentry issue %s %q %s", issue, title, link)}, true, f.save(ctx, a)
}

func (f *feedStore) quiet(ctx context.Context) ([]alert, error) {
	rows, err := f.st.DB().QueryContext(ctx, `SELECT channel_id, key, name, link, opened_ts, latest_ts, quiet_ts, refired FROM alerts WHERE quiet_ts <> ''`)
	if err != nil {
		return nil, fmt.Errorf("load quiet alerts: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []alert
	for rows.Next() {
		var a alert
		if err := rows.Scan(&a.channelID, &a.key, &a.name, &a.link, &a.opened, &a.latest, &a.quiet, &a.refired); err != nil {
			return nil, fmt.Errorf("scan alert: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate alerts: %w", err)
	}
	return out, nil
}

func (f *feedStore) forget(ctx context.Context, a alert) error {
	if _, err := f.st.DB().ExecContext(ctx, `DELETE FROM alerts WHERE channel_id = ? AND key = ?`, a.channelID, a.key); err != nil {
		return fmt.Errorf("close alert %s/%s: %w", a.channelID, a.key, err)
	}
	return nil
}

func (f *feedStore) release(ctx context.Context, channelID, ts string, running bool) error {
	query := `DELETE FROM releases WHERE channel_id = ? AND ts = ?`
	if running {
		query = `INSERT OR IGNORE INTO releases(channel_id, ts) VALUES (?, ?)`
	}
	if _, err := f.st.DB().ExecContext(ctx, query, channelID, ts); err != nil {
		return fmt.Errorf("track release %s/%s: %w", channelID, ts, err)
	}
	return nil
}

func (f *feedStore) running(ctx context.Context) (map[string][]string, error) {
	rows, err := f.st.DB().QueryContext(ctx, `SELECT channel_id, ts FROM releases ORDER BY ts`)
	if err != nil {
		return nil, fmt.Errorf("load running releases: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string][]string{}
	for rows.Next() {
		var channelID, ts string
		if err := rows.Scan(&channelID, &ts); err != nil {
			return nil, fmt.Errorf("scan release: %w", err)
		}
		out[channelID] = append(out[channelID], ts)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate releases: %w", err)
	}
	return out, nil
}

func (p *poller) edited(channelIDs ...string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, channelID := range channelIDs {
		p.edits[channelID] = true
	}
}

func (p *poller) takeEdits() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	edits := slices.Sorted(maps.Keys(p.edits))
	clear(p.edits)
	return edits
}

func (p *poller) feedItem(ctx context.Context, api slackAPI, id identity, channelID string, m slack.Message) (feedItem, bool, error) {
	text := messageText(m)
	thread := cmp.Or(m.ThreadTimestamp, m.Timestamp)
	link := id.permalink(channelID, m.Timestamp, thread)
	if monitorID, ok := m.Metadata.EventPayload["monitor_id"].(float64); ok && m.Metadata.EventType == monitorEventType {
		transition, _ := m.Metadata.EventPayload["transition_type"].(string)
		return p.feeds.monitor(ctx, channelID, strconv.FormatFloat(monitorID, 'f', -1, 64), transition, monitorName(text), link, m.Timestamp)
	}
	if thread != m.Timestamp {
		return feedItem{}, false, nil
	}
	if issue := sentryIssue.FindStringSubmatch(text); issue != nil && m.BotID != "" {
		return p.feeds.sentry(ctx, channelID, issue[1], clipRunes(plainLine(firstLine(text)), botLineLimit), link, m.Timestamp)
	}
	if r, ok := parseRelease(text); ok && m.BotID != "" {
		return r.item(m.Timestamp, link), true, p.feeds.release(ctx, channelID, m.Timestamp, r.running)
	}
	line := fmt.Sprintf("POST %s: %s %s", p.cache.name(ctx, api, m), clipRunes(plainLine(firstLine(text)), botLineLimit), link)
	return feedItem{kind: feedPosts, key: "post:" + m.Timestamp, line: line}, true, nil
}

func (p *poller) routeFeed(ctx context.Context, db *sql.DB, api slackAPI, id identity, fed []fedWatch, m slack.Message) error {
	if len(fed) == 0 {
		return nil
	}
	it, ok, err := p.feedItem(ctx, api, id, fed[0].ChannelID, m)
	if err != nil || !ok {
		return err
	}
	if err := p.deliverFeed(ctx, db, fed, it); err != nil {
		return err
	}
	p.handRelease(ctx, id, fed, it)
	return nil
}

func (p *poller) handRelease(ctx context.Context, id identity, fed []fedWatch, it feedItem) {
	if it.release == nil || len(id.Config.PRLinks.CardCommand) == 0 || !slices.ContainsFunc(fed, func(fw fedWatch) bool { return fw.wants(it) }) || !p.handing(it.release) {
		return
	}
	if _, err := runCardCommand(ctx, id.Config.PRLinks.CardCommand, map[string]any{"release": it.release}); err != nil {
		p.log.Printf("hand release %s to the card command: %v", it.release.Name, err)
	}
}

func (p *poller) handing(r *releasePost) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.handed[r.Link] == r.Line {
		return false
	}
	if r.running {
		p.handed[r.Link] = r.Line
	} else {
		delete(p.handed, r.Link)
	}
	return true
}

func (p *poller) deliverFeed(ctx context.Context, db *sql.DB, fed []fedWatch, it feedItem) error {
	for _, fw := range fed {
		if !fw.wants(it) {
			continue
		}
		msg := feedPayload{WatchID: fw.ID, Line: it.line}
		d := delivery{
			base:   eventFeed,
			origin: event.OriginSystem,
			dedup:  fmt.Sprintf("feed:%s:%s:%s", fw.Owner, fw.ChannelID, it.key),
			line:   it.line,
			payload: func(typ string) []byte {
				msg.Type = typ
				return mustJSON(msg)
			},
		}
		if err := p.deliver(ctx, db, fw.watch, d); err != nil {
			return err
		}
	}
	return nil
}

func onChannel(fed []fedWatch, channelID string) []fedWatch {
	return slices.DeleteFunc(slices.Clone(fed), func(fw fedWatch) bool { return fw.ChannelID != channelID })
}

func (p *poller) feedTick(ctx context.Context, db *sql.DB, api slackAPI, id identity, watches []watch) (time.Time, error) {
	fed, err := p.feeds.fed(ctx, watches)
	if err != nil {
		return time.Time{}, err
	}
	if err := p.rereadReleases(ctx, db, api, id, fed); err != nil {
		return time.Time{}, err
	}
	return p.closeQuiet(ctx, db, fed)
}

func (p *poller) rereadReleases(ctx context.Context, db *sql.DB, api slackAPI, id identity, fed []fedWatch) error {
	edits := p.takeEdits()
	running, err := p.feeds.running(ctx)
	if err != nil {
		p.edited(edits...)
		return err
	}
	for i, channelID := range edits {
		on := onChannel(fed, channelID)
		if len(on) == 0 {
			continue
		}
		if err := p.rereadChannel(ctx, db, api, id, on, channelID, running[channelID]); err != nil {
			p.edited(edits[i:]...)
			return err
		}
	}
	return nil
}

func (p *poller) rereadChannel(ctx context.Context, db *sql.DB, api slackAPI, id identity, on []fedWatch, channelID string, running []string) error {
	for _, ts := range running {
		msgs, _, _, err := api.GetConversationRepliesContext(ctx, &slack.GetConversationRepliesParameters{ChannelID: channelID, Timestamp: ts, Limit: 1})
		if err != nil {
			if errorCode(err) != "thread_not_found" {
				return fmt.Errorf("reread release %s/%s: %w", channelID, ts, err)
			}
			if err := p.feeds.release(ctx, channelID, ts, false); err != nil {
				return err
			}
			continue
		}
		r, ok := parseRelease(messageText(msgs[0]))
		if err := p.feeds.release(ctx, channelID, ts, ok && r.running); err != nil {
			return err
		}
		if !ok {
			continue
		}
		it := r.item(ts, id.permalink(channelID, ts, ts))
		if err := p.deliverFeed(ctx, db, on, it); err != nil {
			return err
		}
		p.handRelease(ctx, id, on, it)
	}
	return nil
}

func (p *poller) closeQuiet(ctx context.Context, db *sql.DB, fed []fedWatch) (time.Time, error) {
	quiet, err := p.feeds.quiet(ctx)
	if err != nil {
		return time.Time{}, err
	}
	now := p.now()
	var next time.Time
	for _, a := range quiet {
		if due := tsTime(a.quiet).Add(alertQuiet); due.After(now) {
			next = earliest(next, due)
			continue
		}
		if it, closes := a.closed(); closes {
			if err := p.deliverFeed(ctx, db, onChannel(fed, a.channelID), it); err != nil {
				return time.Time{}, err
			}
		}
		if err := p.feeds.forget(ctx, a); err != nil {
			return time.Time{}, err
		}
	}
	return next, nil
}
