package main

import (
	"context"
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
