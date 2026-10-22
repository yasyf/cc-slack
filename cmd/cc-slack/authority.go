package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/slack-go/slack"

	"github.com/yasyf/cc-interact/daemon"
)

const (
	approvePost    = "post"
	approveEdit    = "edit"
	approveDelete  = "delete"
	approveCreate  = "create"
	approveArchive = "archive"
	approveInvite  = "invite"
	approveTopic   = "topic"
	approveJoin    = "join"
	owedUpdateRule = "owed-update"
	reachOnce      = "once"
)

var maybeApplied = []string{"fatal_error", "internal_error", "request_timeout", "service_unavailable"}

type ticketSpend struct {
	TicketID    string `json:"ticket_id"`
	Tree        string `json:"ticket_tree"`
	Agent       string `json:"ticket_agent"`
	Call        string `json:"ticket_call"`
	Fingerprint string `json:"ticket_fingerprint"`
}

type write struct {
	action    string
	named     string
	channelID string
	threadTS  string
	text      string
	users     []string
}

type hookTicket struct {
	ID    string            `json:"id"`
	Place map[string]string `json:"place"`
	Reach string            `json:"reach"`
	Rule  string            `json:"rule"`
}

type receipt struct {
	TicketID   string `json:"ticket_id"`
	ChannelID  string `json:"channel_id"`
	TS         string `json:"ts"`
	Permalink  string `json:"permalink"`
	WatchID    int64  `json:"watch_id,omitempty"`
	WatchError string `json:"watch_error,omitempty"`
}

type commandRunner func(ctx context.Context, bin string, args ...string) ([]byte, error)

func execCommand(ctx context.Context, bin string, args ...string) ([]byte, error) {
	var stderr bytes.Buffer
	c := exec.CommandContext(ctx, bin, args...) //nolint:gosec // G204: argv to a binary, never a shell
	c.Stdin, c.Stderr = os.Stdin, &stderr
	out, err := c.Output()
	var exitErr *exec.ExitError
	if reason := strings.TrimSpace(stderr.String()); errors.As(err, &exitErr) && reason != "" {
		return out, errors.New(strings.TrimPrefix(reason[strings.LastIndex(reason, "\n")+1:], "Error: "))
	}
	return out, err
}

func claudeCwd(ctx context.Context, pid int) (string, error) {
	out, err := exec.CommandContext(ctx, "/usr/sbin/lsof", "-a", "-p", strconv.Itoa(pid), "-d", "cwd", "-Fn").Output()
	if err != nil {
		return "", fmt.Errorf("read the working directory of claude %d: %w", pid, err)
	}
	for line := range strings.Lines(string(out)) {
		if dir, ok := strings.CutPrefix(strings.TrimSpace(line), "n"); ok {
			return dir, nil
		}
	}
	return "", fmt.Errorf("lsof named no working directory for claude %d", pid)
}

func (a *app) mintsTickets(ctx context.Context, pid int, hook string) bool {
	if pid == 0 {
		return true
	}
	dir, err := a.claudeCwd(ctx, pid)
	if err != nil {
		return true
	}
	_, err = os.Stat(filepath.Join(dir, ".claude", "hooks", hook))
	return !errors.Is(err, fs.ErrNotExist)
}

func (t hookTicket) once() bool { return t.Reach == reachOnce }

func (w write) target() string {
	where := w.named
	if w.threadTS != "" {
		where += "/" + w.threadTS
	}
	switch {
	case w.action == approveEdit:
		return "an edit in " + where
	case w.action == approveDelete:
		return "a deletion in " + where
	case w.action != approvePost:
		return "a channel " + w.action + " of " + w.named
	case w.threadTS == "":
		return "a post in " + where
	}
	return "a reply in " + where
}

func (a *app) ticketPlace(ctx context.Context, api slackAPI, id identity, w write) (string, string, error) {
	if w.action != approvePost && w.action != approveEdit && w.action != approveDelete {
		return w.named, w.threadTS, nil
	}
	peer, err := a.dmPeer(ctx, api, id, w.named)
	if err != nil {
		return "", "", fmt.Errorf("read whether %s is a DM before spending a ticket on it: %w", w.named, err)
	}
	if peer == "" {
		return w.named, w.threadTS, nil
	}
	return peer, "", nil
}

func (a *app) spendTicket(ctx context.Context, api slackAPI, id identity, session string, s ticketSpend, w write) (hookTicket, error) {
	if s.TicketID == "" {
		return hookTicket{}, fmt.Errorf("no ticket covers this %s: the permission hook names one on an approved write, so call it through a hooked tool and get the user's permission", w.action)
	}
	channel, thread, err := a.ticketPlace(ctx, api, id, w)
	if err != nil {
		return hookTicket{}, err
	}
	out, err := a.captHook(ctx, id.CaptHookPath, "ticket", "spend", s.TicketID,
		"--place", "channel="+channel, "--place", "thread="+thread,
		"--tree", s.Tree, "--session", session, "--agent", s.Agent, "--call", s.Call,
		"--fingerprint", s.Fingerprint, "--summary", w.target())
	if err != nil {
		return hookTicket{}, fmt.Errorf("ticket %s does not cover this %s: %w", s.TicketID, w.action, err)
	}
	var spent struct {
		Ticket hookTicket `json:"ticket"`
	}
	if err := json.Unmarshal(out, &spent); err != nil {
		return hookTicket{}, fmt.Errorf("read the spend of ticket %s: %w", s.TicketID, err)
	}
	return spent.Ticket, nil
}

func (a *app) authorize(hc daemon.HandlerCtx, api slackAPI, id identity, s ticketSpend, w write) (hookTicket, *receipt, error) {
	ctx, db := hc.Ctx, hc.DB
	if s.TicketID == "" && !a.mintsTickets(ctx, hc.Env.ClaudePID, id.Config.permissionHook()) {
		return hookTicket{}, nil, nil
	}
	t, err := a.spendTicket(ctx, api, id, hc.Env.Session, s, w)
	if err != nil {
		return t, nil, err
	}
	if t.once() {
		r, found, err := loadReceipt(ctx, db, t.ID)
		if err != nil {
			return t, nil, err
		}
		if found && r.TS == "" {
			return t, nil, uncertain(t.ID, w)
		}
		if found {
			return t, &r, nil
		}
	}
	if err := a.checkOwedUpdate(ctx, db, api, id, t, w); err != nil {
		return t, nil, errors.New(a.handBack(ctx, db, id, s, t, false, err.Error()))
	}
	return t, nil, nil
}

func refusedBySlack(err error) bool {
	if errors.As(err, new(inviteNeededError)) || errors.As(err, new(*slack.RateLimitedError)) {
		return true
	}
	var resp slack.SlackErrorResponse
	return errors.As(err, &resp) && !slices.Contains(maybeApplied, resp.Err)
}

func (a *app) slackFailed(hc daemon.HandlerCtx, id identity, s ticketSpend, t hookTicket, act action, err error) string {
	if !refusedBySlack(err) {
		return fallback(act, err)
	}
	return a.handBack(hc.Ctx, hc.DB, id, s, t, true, fallback(act, err))
}

func (a *app) handBack(ctx context.Context, db *sql.DB, id identity, s ticketSpend, t hookTicket, claimed bool, refusal string) string {
	var defects []string
	if claimed && t.once() {
		if _, err := db.ExecContext(ctx, `DELETE FROM receipts WHERE ticket_id = ? AND ts = ''`, t.ID); err != nil {
			defects = append(defects, fmt.Sprintf("clearing receipt %s failed: %v", t.ID, err))
		}
	}
	if t.ID == "" {
		return refusal + "; nothing was sent"
	}
	if _, err := a.captHook(ctx, id.CaptHookPath, "ticket", "release", t.ID, "--call", s.Call); err != nil {
		defects = append(defects, fmt.Sprintf("releasing ticket %s failed: %v", t.ID, err))
	}
	if len(defects) > 0 {
		return fmt.Sprintf("%s; nothing was sent, but %s. This is a cc-slack hook defect, never a reason to re-ask the user: report this message verbatim to the root, which spawns a hook-fix lane, and retry the same write once the fix lands",
			refusal, strings.Join(defects, " and "))
	}
	return fmt.Sprintf("%s; nothing was sent, so ticket %s still covers it", refusal, t.ID)
}

func uncertain(ticketID string, w write) error {
	return fmt.Errorf("ticket %s was spent on this exact %s and left no receipt: the write may have reached Slack, so read the thread and send it again under a fresh ticket only if it is not there", ticketID, w.action)
}

func loadReceipt(ctx context.Context, db *sql.DB, ticketID string) (receipt, bool, error) {
	r := receipt{TicketID: ticketID}
	err := db.QueryRowContext(ctx, `SELECT channel_id, ts, permalink, watch_id, watch_error FROM receipts WHERE ticket_id = ?`, ticketID).
		Scan(&r.ChannelID, &r.TS, &r.Permalink, &r.WatchID, &r.WatchError)
	if errors.Is(err, sql.ErrNoRows) {
		return r, false, nil
	}
	if err != nil {
		return r, false, fmt.Errorf("look up receipt %s: %w", ticketID, err)
	}
	return r, true, nil
}

func (a *app) claimReceipt(ctx context.Context, db *sql.DB, t hookTicket, w write) error {
	if !t.once() {
		return nil
	}
	res, err := db.ExecContext(ctx, `INSERT OR IGNORE INTO receipts(ticket_id, created_at) VALUES (?, ?)`, t.ID, a.now().Unix())
	if err != nil {
		return fmt.Errorf("claim receipt %s: %w", t.ID, err)
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return uncertain(t.ID, w)
	}
	return nil
}

func (a *app) saveReceipt(ctx context.Context, db *sql.DB, r receipt) error {
	_, err := db.ExecContext(ctx, `UPDATE receipts SET channel_id = ?, ts = ?, permalink = ? WHERE ticket_id = ?`,
		r.ChannelID, r.TS, r.Permalink, r.TicketID)
	if err != nil {
		return fmt.Errorf("record receipt %s: %w", r.TicketID, err)
	}
	return nil
}

func (a *app) attachWatchReceipt(ctx context.Context, db *sql.DB, r receipt) error {
	_, err := db.ExecContext(ctx, `UPDATE receipts SET watch_id = ?, watch_error = ? WHERE ticket_id = ?`, r.WatchID, r.WatchError, r.TicketID)
	if err != nil {
		return fmt.Errorf("record watch on receipt %s: %w", r.TicketID, err)
	}
	return nil
}

func (a *app) checkOwedUpdate(ctx context.Context, db *sql.DB, api slackAPI, id identity, t hookTicket, w write) error {
	if t.Rule != owedUpdateRule || w.action != approvePost || w.threadTS == "" {
		return nil
	}
	var read threadRead
	err := a.writeAndRetry(ctx, api, id, w.channelID, func() (err error) {
		read, err = a.cache.current(ctx, api, w.channelID, w.threadTS)
		return err
	})
	if err != nil {
		return fmt.Errorf("read thread %s/%s before a ticketed reply: %w", w.channelID, w.threadTS, err)
	}
	if len(read.msgs) == 0 {
		return nil
	}
	last := read.msgs[len(read.msgs)-1]
	if !id.fromClaude(last) || id.authored(last) {
		return nil
	}
	own, err := ownPost(ctx, db, w.channelID, last.Timestamp)
	if err != nil || own {
		return err
	}
	return fmt.Errorf("the latest message in %s/%s is from another Claude agent: ticket %s is an owed update, which never answers one, so get the owner's word to reply", w.channelID, w.threadTS, t.ID)
}
