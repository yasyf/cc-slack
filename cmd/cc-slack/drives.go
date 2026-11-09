package main

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
)

type driveMates map[string]string

func (l *liveCards) threadDrives(channel string) map[string][]string {
	l.mu.Lock()
	defer l.mu.Unlock()
	drives := map[string][]string{}
	for _, t := range l.threads {
		if t.Channel != channel {
			continue
		}
		for _, m := range append(t.messages(), t.Root) {
			if m != nil && m.Card.Drive != "" {
				drives[t.Thread] = append(drives[t.Thread], m.Card.Drive)
			}
		}
	}
	return drives
}

func loadDriveMates(ctx context.Context, db *sql.DB, cards *liveCards, dmChannelID string) (driveMates, error) {
	drives := cards.threadDrives(dmChannelID)
	if len(drives) == 0 {
		return nil, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT DISTINCT subject_id, thread_ts FROM watches WHERE channel_id = ? AND kind = 'dm'`, dmChannelID)
	if err != nil {
		return nil, fmt.Errorf("load the DM's posting sessions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	parent := map[string]string{}
	find := func(x string) string {
		root := x
		for parent[root] != "" {
			root = parent[root]
		}
		for x != root {
			x, parent[x] = parent[x], root
		}
		return root
	}
	var subjects []string
	for rows.Next() {
		var subjectID, thread string
		if err := rows.Scan(&subjectID, &thread); err != nil {
			return nil, fmt.Errorf("scan a posting session: %w", err)
		}
		for _, drive := range drives[thread] {
			if a, b := find(subjectID), find("drive:"+drive); a != b {
				parent[a] = b
			}
			subjects = append(subjects, subjectID)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate posting sessions: %w", err)
	}
	mates := driveMates{}
	for _, subjectID := range subjects {
		mates[subjectID] = find(subjectID)
	}
	return mates, nil
}

func (d driveMates) same(a, b string) bool {
	family, ok := d[a]
	return ok && family == d[b]
}

func (d driveMates) onAnotherSessionsCard(w watch, inThread []watch) bool {
	carded := false
	for _, o := range inThread {
		if o.Kind != kindDM {
			continue
		}
		if o.SubjectID == w.SubjectID || o.PID == w.PID {
			return false
		}
		carded = carded || !d.same(o.SubjectID, w.SubjectID)
	}
	return carded
}

func (d driveMates) takeOver(inThread []watch, holder watch) []watch {
	kept := slices.DeleteFunc(slices.Clone(inThread), func(w watch) bool {
		return w.Kind == kindDM && w.SubjectID != holder.SubjectID && w.PID != holder.PID && d.same(w.SubjectID, holder.SubjectID)
	})
	if len(kept) == len(inThread) || d.onAnotherSessionsCard(holder, kept) {
		return inThread
	}
	if slices.ContainsFunc(kept, func(o watch) bool { return o.SubjectID == holder.SubjectID && o.Owner == holder.Owner }) {
		return kept
	}
	return append(kept, holder)
}
