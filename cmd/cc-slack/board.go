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
	"time"

	"github.com/yasyf/cc-interact/daemon"
)

const presentBinary = "cc-present"

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

func (p *poller) forwardAnswer(ctx context.Context, db *sql.DB, watches []watch, ans boardAnswer) {
	told := map[string]bool{}
	for _, w := range watches {
		var session sql.NullString
		if err := db.QueryRowContext(ctx, `SELECT session_id FROM subjects WHERE id = ?`, w.SubjectID).Scan(&session); err != nil {
			p.log.Printf("waiting ask %s: the board was not told: find the session of subject %s: %v", ans.Waits, w.SubjectID, err)
			continue
		}
		if !session.Valid || told[session.String] {
			continue
		}
		told[session.String] = true
		if err := presentSync(ctx, session.String, "answered", ans); err != nil {
			p.log.Printf("waiting ask %s: the board was not told: %v", ans.Waits, err)
		}
	}
}
