package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"time"

	"github.com/yasyf/cc-interact/daemon"
)

const presentBinary = "cc-present"

const questionOnBoard = "a question goes on the cc-present board first: put the card there with cc-present update-block, then run cc-present send. This card was posted anyway."

type boardCard struct {
	Waits       string    `json:"waits"`
	Title       string    `json:"title"`
	Body        string    `json:"body,omitempty"`
	Options     []string  `json:"options"`
	Recommended string    `json:"recommended"`
	Permalink   string    `json:"permalink"`
	PRs         []boardPR `json:"prs,omitempty"`
}

type boardPR struct {
	Repo   string `json:"repo"`
	Number int    `json:"number"`
}

type boardAnswer struct {
	Waits     string    `json:"waits"`
	Label     string    `json:"label"`
	Other     bool      `json:"other,omitempty"`
	At        time.Time `json:"at"`
	Permalink string    `json:"permalink"`
}

type boardDecision struct {
	Row       string    `json:"row"`
	Label     string    `json:"label"`
	At        time.Time `json:"at"`
	Permalink string    `json:"permalink"`
}

func presentSync(ctx context.Context, session, event string, payload any) error {
	bin, err := exec.LookPath(presentBinary)
	if errors.Is(err, exec.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	c := exec.CommandContext(ctx, bin, "slack-sync", event, "--session", session) //nolint:gosec // G204: argv to the cc-present binary on PATH, never a shell
	c.Stdin = bytes.NewReader(mustJSON(payload))
	c.Env = append(os.Environ(), "CLAUDE_CODE_SESSION_ID="+session)
	if out, err := c.CombinedOutput(); err != nil {
		return fmt.Errorf("%s slack-sync %s: %w: %s", presentBinary, event, err, clip(string(out)))
	}
	return nil
}

func mirrorPosted(ctx context.Context, session string, op daemon.Op, body, reply json.RawMessage) error {
	if session == "" || op != opDMStatus {
		return nil
	}
	var req struct {
		Card     *statusCard `json:"card"`
		Text     string      `json:"text"`
		Mirrored bool        `json:"mirrored"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return err
	}
	if req.Card == nil || req.Card.Waits == "" || req.Mirrored {
		return nil
	}
	req.Card.fill(req.Text)
	var posted struct {
		Permalink string `json:"permalink"`
	}
	if err := json.Unmarshal(reply, &posted); err != nil {
		return err
	}
	c := req.Card
	card := boardCard{Waits: c.Waits, Title: c.Title, Body: c.Body, Options: c.answers(), Recommended: c.Recommended, Permalink: posted.Permalink}
	for _, r := range c.Refs {
		if id, ok := parseRef(r.URL); ok && id.Kind == refPR {
			card.PRs = append(card.PRs, boardPR{Repo: id.Owner + "/" + id.Repo, Number: id.Number})
		}
	}
	return presentSync(ctx, session, "posted", card)
}

func watchSessions(ctx context.Context, db *sql.DB, watches []watch) ([]string, error) {
	var sessions []string
	for _, w := range watches {
		var session sql.NullString
		if err := db.QueryRowContext(ctx, `SELECT session_id FROM subjects WHERE id = ?`, w.SubjectID).Scan(&session); err != nil {
			return sessions, fmt.Errorf("find the session of subject %s: %w", w.SubjectID, err)
		}
		if session.Valid && !slices.Contains(sessions, session.String) {
			sessions = append(sessions, session.String)
		}
	}
	return sessions, nil
}

func (p *poller) forwardAnswer(ctx context.Context, db *sql.DB, watches []watch, ans boardAnswer) {
	sessions, err := watchSessions(ctx, db, watches)
	if err != nil {
		p.log.Printf("waiting ask %s: the board was not told: %v", ans.Waits, err)
	}
	for _, session := range sessions {
		if err := presentSync(ctx, session, "answered", ans); err != nil {
			p.log.Printf("waiting ask %s: the board was not told: %v", ans.Waits, err)
		}
	}
}

func (p *poller) decideOnBoard(ctx context.Context, db *sql.DB, watches []watch, d boardDecision) error {
	sessions, err := watchSessions(ctx, db, watches)
	if err != nil {
		return err
	}
	if len(sessions) == 0 {
		return errNoStatusSession
	}
	var errs []error
	for _, session := range sessions {
		err := presentSync(ctx, session, "decided", d)
		if err == nil {
			return nil
		}
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func handPostedQuestion(b opBody) bool {
	if b.Card == nil || b.Card.Waits == "" || b.Mirrored || b.RootOnly {
		return false
	}
	_, err := exec.LookPath(presentBinary)
	return err == nil
}
