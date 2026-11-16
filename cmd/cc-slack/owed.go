package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

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
	ThreadTS      string `json:"thread_ts"`
	TS            string `json:"ts"`
	Text          string `json:"text"`
	Permalink     string `json:"permalink"`
	ChannelID     string `json:"channel_id,omitempty"`
	HolderSession string `json:"holder_session,omitempty"`
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

func (o *owedReplies) answeredIn(ctx context.Context, threadTS, ts string) error {
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

func dmHolder(ctx context.Context, db *sql.DB, dmChannelID, channelID string) (dmHold, error) {
	w, session, err := holderWatch(ctx, db, dmChannelID, channelID)
	if err != nil || w.ID == 0 {
		return dmHold{}, err
	}
	return dmHold{subject: w.SubjectID, session: session, pid: w.PID}, nil
}

func dmHolderWatch(ctx context.Context, db *sql.DB, dmChannelID, channelID string) (watch, bool, error) {
	w, _, err := holderWatch(ctx, db, dmChannelID, channelID)
	return w, w.ID != 0, err
}

func holderWatch(ctx context.Context, db *sql.DB, dmChannelID, channelID string) (watch, string, error) {
	rows, err := db.QueryContext(ctx, `SELECT w.id, w.subject_id, w.owner_agent_id, w.kind, w.channel_id, w.thread_ts, s.claude_pid, s.session_id
		FROM watches w JOIN subjects s ON s.id = w.subject_id
		WHERE w.channel_id IN (?, ?) AND w.thread_ts = '' AND w.kind = 'channel' AND w.status = 'active' ORDER BY w.id`, dmChannelID, channelID)
	if err != nil {
		return watch{}, "", fmt.Errorf("find the holder of %s: %w", channelID, err)
	}
	defer func() { _ = rows.Close() }()
	var watches []watch
	sessions := map[int64]string{}
	for rows.Next() {
		var (
			w       watch
			session string
		)
		if err := rows.Scan(&w.ID, &w.SubjectID, &w.Owner, &w.Kind, &w.ChannelID, &w.ThreadTS, &w.PID, &session); err != nil {
			return watch{}, "", fmt.Errorf("scan a holder watch of %s: %w", channelID, err)
		}
		watches = append(watches, w)
		sessions[w.ID] = session
	}
	if err := rows.Err(); err != nil {
		return watch{}, "", fmt.Errorf("iterate the holder watches of %s: %w", channelID, err)
	}
	held := holderIn(watches, dmChannelID, channelID)
	return held, sessions[held.ID], nil
}

func othersCardThreads(ctx context.Context, db *sql.DB, threads *dmThreads, dmChannelID string, replies []owedReply, hold dmHold, now time.Time) (map[string]bool, error) {
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
	mates, err := loadDriveMates(ctx, threads, hold.subject, subjects)
	if err != nil {
		return nil, err
	}
	untaken, err := loadUntaken(ctx, db, subjects)
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
			if !live || untaken.deaf(subjectID, now) {
				continue
			}
			other = true
			held = held || subjectID == hold.subject || pid == hold.pid || mates.same(subjectID, hold.subject)
		}
		others[thread] = other && !held
	}
	return others, nil
}

func heldReplies(ctx context.Context, db *sql.DB, id identity, replies []owedReply) error {
	sessions := map[string]string{}
	for i, r := range replies {
		ref, err := parsePermalink(r.Permalink)
		if err != nil {
			return err
		}
		session, known := sessions[ref.channelID]
		if !known {
			hold, err := dmHolder(ctx, db, id.DMChannelID, ref.channelID)
			if err != nil {
				return err
			}
			session, sessions[ref.channelID] = hold.session, hold.session
		}
		replies[i].ChannelID, replies[i].HolderSession = ref.channelID, session
	}
	return nil
}
