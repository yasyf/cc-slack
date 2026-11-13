package main

import (
	"context"
	"fmt"
	"maps"
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

func loadDriveMates(ctx context.Context, threads *dmThreads, cards *liveCards, dmChannelID string) (driveMates, error) {
	drives := cards.threadDrives(dmChannelID)
	if len(drives) == 0 {
		return nil, nil
	}
	bound, err := threads.bound(ctx, dmChannelID, slices.Collect(maps.Keys(drives)))
	if err != nil {
		return nil, fmt.Errorf("load the DM's posting sessions: %w", err)
	}
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
	for _, thread := range slices.Sorted(maps.Keys(bound)) {
		for _, subjectID := range bound[thread] {
			for _, drive := range drives[thread] {
				if a, b := find(subjectID), find("drive:"+drive); a != b {
					parent[a] = b
				}
				subjects = append(subjects, subjectID)
			}
		}
	}
	mates := driveMates{}
	for _, subjectID := range subjects {
		mates[subjectID] = find(subjectID)
	}
	return mates, nil
}

func (d driveMates) holds(w watch) bool {
	_, carded := d[w.SubjectID]
	return carded && (w.Kind == kindDM || w.Relayed)
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
