package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/yasyf/cc-interact/store"
)

var owedSchema = store.Schema{DDL: `
CREATE TABLE owed_replies (
  thread_ts TEXT NOT NULL,
  ts        TEXT NOT NULL,
  text      TEXT NOT NULL,
  permalink TEXT NOT NULL,
  PRIMARY KEY (thread_ts, ts)
);
`}

type owedReply struct {
	ThreadTS  string `json:"thread_ts"`
	TS        string `json:"ts"`
	Text      string `json:"text"`
	Permalink string `json:"permalink"`
}

type owedReplies struct {
	st *store.Store
}

func owedRepliesPath() string { return filepath.Join(appPaths().StateDir(), "owed-replies.db") }

func openOwedReplies(ctx context.Context, path string) (*owedReplies, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("owed replies dir: %w", err)
	}
	st, err := store.Open(ctx, path, owedSchema, store.WithUnsupportedSchema(store.ArchiveUnsupportedSchema))
	if err != nil {
		return nil, fmt.Errorf("open owed replies %s: %w", path, err)
	}
	return &owedReplies{st: st}, nil
}

func (o *owedReplies) Close() error { return o.st.Close() }

func (o *owedReplies) owe(ctx context.Context, r owedReply) error {
	if _, err := o.st.DB().ExecContext(ctx, `INSERT OR IGNORE INTO owed_replies(thread_ts, ts, text, permalink) VALUES (?, ?, ?, ?)`,
		r.ThreadTS, r.TS, r.Text, r.Permalink); err != nil {
		return fmt.Errorf("owe reply %s: %w", r.TS, err)
	}
	return nil
}

func (o *owedReplies) answered(ctx context.Context, threadTS, ts string) error {
	if _, err := o.st.DB().ExecContext(ctx, `DELETE FROM owed_replies WHERE thread_ts = ? AND ts < ?`, threadTS, ts); err != nil {
		return fmt.Errorf("answer thread %s: %w", threadTS, err)
	}
	return nil
}

var answeringReactions = map[string]bool{"+1": true, "thumbsup": true}

func (o *owedReplies) answer(ctx context.Context, ts string) error {
	if _, err := o.st.DB().ExecContext(ctx, `DELETE FROM owed_replies WHERE ts = ?`, ts); err != nil {
		return fmt.Errorf("answer reply %s: %w", ts, err)
	}
	return nil
}

func (o *owedReplies) forget(ctx context.Context, threadTS string) error {
	if _, err := o.st.DB().ExecContext(ctx, `DELETE FROM owed_replies WHERE thread_ts = ?`, threadTS); err != nil {
		return fmt.Errorf("forget thread %s: %w", threadTS, err)
	}
	return nil
}

func (o *owedReplies) list(ctx context.Context) ([]owedReply, error) {
	rows, err := o.st.DB().QueryContext(ctx, `SELECT thread_ts, ts, text, permalink FROM owed_replies ORDER BY ts`)
	if err != nil {
		return nil, fmt.Errorf("list owed replies: %w", err)
	}
	defer func() { _ = rows.Close() }()
	replies := []owedReply{}
	for rows.Next() {
		var r owedReply
		if err := rows.Scan(&r.ThreadTS, &r.TS, &r.Text, &r.Permalink); err != nil {
			return nil, fmt.Errorf("scan owed reply: %w", err)
		}
		replies = append(replies, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate owed replies: %w", err)
	}
	return replies, nil
}

type dmHold struct {
	subject string
	session string
	pid     int
}

func dmHolder(ctx context.Context, db *sql.DB, dmChannelID string) (dmHold, error) {
	var hold dmHold
	err := db.QueryRowContext(ctx, `SELECT s.id, s.session_id, s.claude_pid FROM watches w JOIN subjects s ON s.id = w.subject_id
		WHERE w.channel_id = ? AND w.thread_ts = '' AND w.kind = 'channel' AND w.status = 'active'
		ORDER BY w.id LIMIT 1`, dmChannelID).Scan(&hold.subject, &hold.session, &hold.pid)
	if errors.Is(err, sql.ErrNoRows) {
		return dmHold{}, nil
	}
	if err != nil {
		return dmHold{}, fmt.Errorf("find the DM's holder: %w", err)
	}
	return hold, nil
}

func dmHolderWatch(ctx context.Context, db *sql.DB, dmChannelID string) (watch, bool, error) {
	var w watch
	err := db.QueryRowContext(ctx, `SELECT id, subject_id, owner_agent_id, kind, channel_id, thread_ts FROM watches
		WHERE channel_id = ? AND thread_ts = '' AND kind = 'channel' AND status = 'active'
		ORDER BY id LIMIT 1`, dmChannelID).Scan(&w.ID, &w.SubjectID, &w.Owner, &w.Kind, &w.ChannelID, &w.ThreadTS)
	if errors.Is(err, sql.ErrNoRows) {
		return watch{}, false, nil
	}
	if err != nil {
		return watch{}, false, fmt.Errorf("find the DM's holder watch: %w", err)
	}
	return w, true, nil
}

func othersCardThreads(ctx context.Context, db *sql.DB, threads *dmThreads, dmChannelID string, replies []owedReply, hold dmHold, mates driveMates) (map[string]bool, error) {
	owed := make([]string, 0, len(replies))
	for _, r := range replies {
		owed = append(owed, r.ThreadTS)
	}
	bound, err := threads.bound(ctx, dmChannelID, owed)
	if err != nil {
		return nil, err
	}
	var subjects []string
	for _, posters := range bound {
		subjects = append(subjects, posters...)
	}
	subscribed, err := dmSubscriptions(ctx, db, dmChannelID, subjects)
	if err != nil {
		return nil, err
	}
	pids := map[string]int{}
	for _, w := range subscribed {
		pids[w.SubjectID] = w.PID
	}
	others := map[string]bool{}
	for thread, posters := range bound {
		other, held := false, false
		for _, subjectID := range posters {
			pid, live := pids[subjectID]
			if !live {
				continue
			}
			other = true
			held = held || subjectID == hold.subject || pid == hold.pid || mates.same(subjectID, hold.subject)
		}
		others[thread] = other && !held
	}
	return others, nil
}
