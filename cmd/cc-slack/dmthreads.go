package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/yasyf/cc-interact/agent"
	"github.com/yasyf/cc-interact/store"
)

var dmThreadsSchema = store.Schema{DDL: `
CREATE TABLE dm_threads (
  channel_id TEXT NOT NULL,
  thread_ts  TEXT NOT NULL,
  subject_id TEXT NOT NULL,
  PRIMARY KEY (channel_id, thread_ts, subject_id)
);
CREATE INDEX idx_dm_threads_subject ON dm_threads(channel_id, subject_id);
`}

type dmThreads struct {
	st *store.Store
}

func dmThreadsPath() string { return filepath.Join(appPaths().StateDir(), "dm-threads.db") }

func openDMThreads(ctx context.Context, path string) (*dmThreads, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("DM threads dir: %w", err)
	}
	st, err := store.Open(ctx, path, dmThreadsSchema, store.WithUnsupportedSchema(store.ArchiveUnsupportedSchema))
	if err != nil {
		return nil, fmt.Errorf("open DM threads %s: %w", path, err)
	}
	return &dmThreads{st: st}, nil
}

func (d *dmThreads) Close() error { return d.st.Close() }

func (d *dmThreads) bind(ctx context.Context, channelID, threadTS, subjectID string) error {
	return d.bindAll(ctx, [][3]string{{channelID, threadTS, subjectID}})
}

func (d *dmThreads) unbind(ctx context.Context, channelID, threadTS, subjectID string) (bool, error) {
	res, err := d.st.DB().ExecContext(ctx, `DELETE FROM dm_threads WHERE channel_id = ? AND thread_ts = ? AND subject_id = ?`,
		channelID, threadTS, subjectID)
	if err != nil {
		return false, fmt.Errorf("unbind DM thread %s/%s: %w", channelID, threadTS, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("unbind DM thread %s/%s: %w", channelID, threadTS, err)
	}
	return n > 0, nil
}

func (d *dmThreads) bound(ctx context.Context, channelID string, threads []string) (map[string][]string, error) {
	out := map[string][]string{}
	if len(threads) == 0 {
		return out, nil
	}
	rows, err := d.st.DB().QueryContext(ctx, `SELECT thread_ts, subject_id FROM dm_threads
		WHERE channel_id = ? AND thread_ts IN (SELECT value FROM json_each(?)) ORDER BY rowid`, channelID, string(mustJSON(threads)))
	if err != nil {
		return nil, fmt.Errorf("find the sessions of %d DM threads: %w", len(threads), err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var thread, subjectID string
		if err := rows.Scan(&thread, &subjectID); err != nil {
			return nil, fmt.Errorf("scan a DM thread's session: %w", err)
		}
		out[thread] = append(out[thread], subjectID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate DM threads' sessions: %w", err)
	}
	return out, nil
}

func (d *dmThreads) sessionThreads(ctx context.Context, channelID string, subjectIDs ...string) ([]string, error) {
	if len(subjectIDs) == 0 {
		return nil, nil
	}
	rows, err := d.st.DB().QueryContext(ctx, `SELECT thread_ts FROM dm_threads
		WHERE channel_id = ? AND subject_id IN (SELECT value FROM json_each(?)) ORDER BY rowid DESC`, channelID, string(mustJSON(subjectIDs)))
	if err != nil {
		return nil, fmt.Errorf("find session threads: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var threads []string
	for rows.Next() {
		var thread string
		if err := rows.Scan(&thread); err != nil {
			return nil, fmt.Errorf("scan session thread: %w", err)
		}
		threads = append(threads, thread)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate session threads: %w", err)
	}
	return threads, nil
}

func (d *dmThreads) convert(ctx context.Context, db *sql.DB, now time.Time) error {
	rows, err := db.QueryContext(ctx, `SELECT subject_id, channel_id, thread_ts, status FROM watches WHERE kind = 'dm' AND thread_ts <> '' ORDER BY id`)
	if err != nil {
		return fmt.Errorf("find per-message DM watches: %w", err)
	}
	type subscriber struct{ subjectID, channelID string }
	var bindings [][3]string
	subscribers := map[subscriber]string{}
	var order []subscriber
	for rows.Next() {
		var subjectID, channelID, thread, status string
		if err := rows.Scan(&subjectID, &channelID, &thread, &status); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan a per-message DM watch: %w", err)
		}
		bindings = append(bindings, [3]string{channelID, thread, subjectID})
		s := subscriber{subjectID, channelID}
		prior, seen := subscribers[s]
		if !seen {
			order = append(order, s)
			subscribers[s] = ""
		}
		if status == "active" || status == "orphaned" && prior != "active" {
			subscribers[s] = status
		}
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close per-message DM watches: %w", err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate per-message DM watches: %w", err)
	}
	if len(bindings) == 0 {
		return nil
	}
	if err := d.bindAll(ctx, bindings); err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin DM subscription tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, s := range order {
		status := subscribers[s]
		if status == "" {
			continue
		}
		var held int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM watches WHERE subject_id = ? AND kind = 'dm' AND channel_id = ? AND thread_ts = '' AND status = ?`,
			s.subjectID, s.channelID, status).Scan(&held); err != nil {
			return fmt.Errorf("find the DM subscription of %s: %w", s.subjectID, err)
		}
		if held > 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO watches(subject_id, owner_agent_id, kind, channel_id, thread_ts, status, created_at)
			VALUES (?, ?, 'dm', ?, '', ?, ?)`, s.subjectID, agent.TopLevel, s.channelID, status, now.Unix()); err != nil {
			return fmt.Errorf("subscribe %s to the DM: %w", s.subjectID, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM watches WHERE kind = 'dm' AND thread_ts <> ''`); err != nil {
		return fmt.Errorf("drop per-message DM watches: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit DM subscriptions: %w", err)
	}
	return nil
}

func (d *dmThreads) bindAll(ctx context.Context, bindings [][3]string) error {
	tx, err := d.st.DB().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin DM thread tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, b := range bindings {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO dm_threads(channel_id, thread_ts, subject_id) VALUES (?, ?, ?)`, b[0], b[1], b[2]); err != nil {
			return fmt.Errorf("bind DM thread %s/%s: %w", b[0], b[1], err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit DM threads: %w", err)
	}
	return nil
}

func subscribeDM(ctx context.Context, tx *sql.Tx, subjectID, dmChannelID string, now time.Time) error {
	subscribed, err := countActive(ctx, tx, `SELECT COUNT(*) FROM watches WHERE subject_id = ? AND kind = 'dm' AND channel_id = ? AND status = 'active'`,
		subjectID, dmChannelID)
	if err != nil || subscribed {
		return err
	}
	_, err = addWatch(ctx, tx, subjectID, agent.TopLevel, kindDM, dmChannelID, "", now)
	return err
}

func dmSubscriptions(ctx context.Context, db *sql.DB, dmChannelID string, subjectIDs []string) ([]watch, error) {
	if len(subjectIDs) == 0 {
		return nil, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT w.id, w.subject_id, w.owner_agent_id, w.kind, w.channel_id, w.thread_ts, s.claude_pid
		FROM watches w JOIN subjects s ON s.id = w.subject_id
		WHERE w.kind = 'dm' AND w.status = 'active' AND w.channel_id = ? AND w.subject_id IN (SELECT value FROM json_each(?))
		ORDER BY w.id`, dmChannelID, string(mustJSON(subjectIDs)))
	if err != nil {
		return nil, fmt.Errorf("find DM subscriptions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var watches []watch
	for rows.Next() {
		var w watch
		if err := rows.Scan(&w.ID, &w.SubjectID, &w.Owner, &w.Kind, &w.ChannelID, &w.ThreadTS, &w.PID); err != nil {
			return nil, fmt.Errorf("scan DM subscription: %w", err)
		}
		watches = append(watches, w)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate DM subscriptions: %w", err)
	}
	return watches, nil
}
