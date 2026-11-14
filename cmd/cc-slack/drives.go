package main

import (
	"context"
)

type sessionDrive struct {
	name        string
	coordinator bool
}

type driveMates struct {
	drives map[string]sessionDrive
	holder string
}

func loadDriveMates(ctx context.Context, threads *dmThreads, holder string, subjectIDs []string) (driveMates, error) {
	drives, err := threads.drives(ctx, append([]string{holder}, subjectIDs...))
	return driveMates{drives: drives, holder: holder}, err
}

func (d driveMates) holds(w watch) bool {
	_, drive := d.drives[w.SubjectID]
	return drive && w.SubjectID == d.holder && (w.Kind == kindDM || w.Relayed)
}

func (d driveMates) same(a, b string) bool {
	drive, ok := d.drives[a]
	return ok && drive.name == d.drives[b].name
}

func (d driveMates) leaves(w watch, inThread []watch) bool {
	posted, foreign := false, false
	for _, o := range inThread {
		switch {
		case o.Kind != kindDM:
		case o.SubjectID == w.SubjectID || o.PID == w.PID:
			posted = true
		case !d.same(o.SubjectID, w.SubjectID):
			foreign = true
		case w.SubjectID == d.holder && !d.drives[o.SubjectID].coordinator:
			return true
		}
	}
	return foreign && !posted
}
