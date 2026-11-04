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

func (o *owedReplies) reacted(ctx context.Context, ts string) error {
	if _, err := o.st.DB().ExecContext(ctx, `DELETE FROM owed_replies WHERE ts = ?`, ts); err != nil {
		return fmt.Errorf("answer reply %s by reaction: %w", ts, err)
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

func othersCardThreads(ctx context.Context, db *sql.DB, dmChannelID string, hold dmHold) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT w.thread_ts FROM watches w JOIN subjects s ON s.id = w.subject_id
		WHERE w.channel_id = ? AND w.kind = 'dm' AND w.status = 'active'
		GROUP BY w.thread_ts HAVING SUM(w.subject_id = ? OR s.claude_pid = ?) = 0`, dmChannelID, hold.subject, hold.pid)
	if err != nil {
		return nil, fmt.Errorf("find other sessions' card threads: %w", err)
	}
	defer func() { _ = rows.Close() }()
	threads := map[string]bool{}
	for rows.Next() {
		var thread string
		if err := rows.Scan(&thread); err != nil {
			return nil, fmt.Errorf("scan card thread: %w", err)
		}
		threads[thread] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate card threads: %w", err)
	}
	return threads, nil
}
