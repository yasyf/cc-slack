package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/slack-go/slack"
	"github.com/spf13/cobra"

	"github.com/yasyf/cc-interact/cmd"
	"github.com/yasyf/cc-interact/daemon"
)

const (
	opDriveCards     daemon.Op = "drive-cards"
	opCanvas         daemon.Op = "canvas"
	opWithdrawPRCard daemon.Op = "withdraw-pr-card"
)

var goneCanvasCodes = map[string]bool{"canvas_not_found": true, "canvas_deleted": true}

const fullCanvasCode = "canvas_too_large"

type driveQuestion struct {
	Question    string     `json:"question"`
	Options     []string   `json:"options"`
	Recommended string     `json:"recommended,omitempty"`
	Asked       time.Time  `json:"asked"`
	URL         string     `json:"url"`
	Card        statusCard `json:"card"`
}

type drivePRCard struct {
	PRs   []string   `json:"prs"`
	Asked time.Time  `json:"asked"`
	URL   string     `json:"url"`
	Card  statusCard `json:"card"`
}

type driveClosedCard struct {
	Waits      string     `json:"waits"`
	Title      string     `json:"title"`
	PRs        []string   `json:"prs"`
	Asked      time.Time  `json:"asked"`
	URL        string     `json:"url"`
	Answer     *askAnswer `json:"answer"`
	Tapped     string     `json:"tapped"`
	ReplacedBy string     `json:"replaced_by"`
}

type driveCardsResult struct {
	TeamURL   string            `json:"team_url"`
	Questions []driveQuestion   `json:"questions"`
	PRCards   []drivePRCard     `json:"pr_cards"`
	Closed    []driveClosedCard `json:"closed"`
}

type canvasResult struct {
	CanvasID string `json:"canvas_id"`
	URL      string `json:"url"`
	Created  bool   `json:"created"`
	Replaced string `json:"replaced,omitempty"`
}

// listDriveCards lists the waiting cards that name drive and are still unanswered and unreplaced, and every PR card still waiting
// on a tap, a waiting status card that carries PR rows included, oldest first, each with its permalink; closed holds every other
// such card the store keeps, each with the answer, tap line or replacing card that closed it.
func (l *liveCards) listDriveCards(drive string, link func(channel, ts, thread string) string) driveCardsResult {
	l.mu.Lock()
	defer l.mu.Unlock()
	res := driveCardsResult{Questions: []driveQuestion{}, PRCards: []drivePRCard{}, Closed: []driveClosedCard{}}
	prURLs := func(m *liveMessage) []string {
		listed := m.Card.allPRs()
		prs := make([]string, 0, len(listed))
		for _, pr := range listed {
			prs = append(prs, pr.URL)
		}
		return prs
	}
	closed := func(t *liveThread, m *liveMessage) driveClosedCard {
		c := driveClosedCard{Waits: m.Card.Waits, Title: m.Card.Title, PRs: prURLs(m), Asked: m.At, URL: link(t.Channel, m.TS, t.Thread), Tapped: m.Tapped}
		if ans, answered := t.Answers[m.Card.Waits]; answered {
			c.Answer = &ans
		}
		if m.ReplacedBy != "" {
			by, _ := l.storedCard(t.Channel, m.ReplacedBy)
			c.ReplacedBy = link(t.Channel, m.ReplacedBy, by.thread)
		}
		return c
	}
	for _, t := range l.threads {
		for _, m := range t.Waiting {
			switch {
			case m.Card.Drive != drive && (m.Card.Drive != "" || len(m.Card.allPRs()) == 0):
			case !t.open(m):
				res.Closed = append(res.Closed, closed(t, m))
			case m.Card.Drive == drive:
				res.Questions = append(res.Questions, driveQuestion{m.Card.Title, m.Card.Options, m.Card.Recommended, m.At, link(t.Channel, m.TS, t.Thread), m.Card})
			default:
				res.PRCards = append(res.PRCards, drivePRCard{prURLs(m), m.At, link(t.Channel, m.TS, t.Thread), m.Card})
			}
		}
		for _, m := range t.PRCards {
			switch {
			case m.Card.Waits == "":
			case t.open(m):
				res.PRCards = append(res.PRCards, drivePRCard{prURLs(m), m.At, link(t.Channel, m.TS, t.Thread), m.Card})
			default:
				res.Closed = append(res.Closed, closed(t, m))
			}
		}
	}
	slices.SortFunc(res.Questions, func(a, b driveQuestion) int { return a.Asked.Compare(b.Asked) })
	slices.SortFunc(res.PRCards, func(a, b drivePRCard) int { return a.Asked.Compare(b.Asked) })
	slices.SortFunc(res.Closed, func(a, b driveClosedCard) int { return a.Asked.Compare(b.Asked) })
	return res
}

func (a *app) handleDriveCards(hc daemon.HandlerCtx) daemon.Reply {
	b, err := decodeBody(hc)
	if err != nil {
		return fail(err.Error())
	}
	if b.Name == "" {
		return fail("drive-cards needs name, the drive whose cards to list")
	}
	id, err := loadIdentity(hc.Ctx, hc.DB)
	if err != nil {
		return fail(err.Error())
	}
	listed := a.cards.listDriveCards(b.Name, id.permalink)
	listed.TeamURL = id.TeamURL
	return ok(listed)
}

func (a *app) handleWithdrawPRCard(hc daemon.HandlerCtx) daemon.Reply {
	b, err := decodeBody(hc)
	if err != nil {
		return fail(err.Error())
	}
	pr, found := parseRef(b.PR)
	reason := strings.TrimSpace(withoutHidden(b.Reason))
	switch {
	case !found || pr.Kind != refPR:
		return fail(fmt.Sprintf("withdraw-pr-card needs pr, a pull request URL, not %q", b.PR))
	case reason == "":
		return fail("withdraw-pr-card needs reason, the line the card shows in place of its buttons")
	}
	id, err := loadIdentity(hc.Ctx, hc.DB)
	if err != nil {
		return fail(err.Error())
	}
	withdrawn, err := a.poller.withdrawPRCards(hc.Ctx, id, pr, reason)
	if err != nil {
		return fail(err.Error())
	}
	return ok(map[string][]string{"withdrawn": withdrawn})
}

func (a *app) handleCanvas(hc daemon.HandlerCtx) daemon.Reply {
	b, err := decodeBody(hc)
	if err != nil {
		return fail(err.Error())
	}
	if b.Text == "" || b.FileID == "" && b.Name == "" {
		return fail("canvas needs text, the page's markdown, and file_id, the canvas to redraw, or name, the title of a new one")
	}
	id, err := loadIdentity(hc.Ctx, hc.DB)
	if err != nil {
		return fail(err.Error())
	}
	api := a.newAPI(id.Token)
	page := slack.DocumentContent{Type: "markdown", Markdown: b.Text}
	replaced := ""
	if b.FileID != "" {
		err := api.EditCanvasContext(hc.Ctx, slack.EditCanvasParams{CanvasID: b.FileID, Changes: []slack.CanvasChange{{Operation: "replace", DocumentContent: page}}})
		var refused slack.SlackErrorResponse
		switch {
		case err == nil:
			return ok(canvasResult{CanvasID: b.FileID, URL: canvasURL(id, b.FileID)})
		case !errors.As(err, &refused) || b.Name == "":
			return fail(fallback(actionWrite, err))
		case refused.Err == fullCanvasCode:
			if err := deleteFullCanvas(hc.Ctx, api, id, b.FileID, b.Name); err != nil {
				return fail(err.Error())
			}
			replaced = b.FileID
		case !goneCanvasCodes[refused.Err]:
			return fail(fallback(actionWrite, err))
		}
	}
	canvasID, err := api.TabCanvasContext(hc.Ctx, id.DMChannelID, b.Name, page)
	if err != nil {
		return fail(fallback(actionWrite, err))
	}
	return ok(canvasResult{CanvasID: canvasID, URL: canvasURL(id, canvasID), Created: true, Replaced: replaced})
}

func deleteFullCanvas(ctx context.Context, api slackAPI, id identity, canvasID, title string) error {
	file, _, _, err := api.GetFileInfoContext(ctx, canvasID, 0, 0)
	if err != nil {
		return fmt.Errorf("canvas %s is full, and reading it to replace it failed: %w", canvasID, err)
	}
	if file.Title != title || file.User != id.BotUserID {
		return fmt.Errorf("canvas %s is full, but it is titled %q and made by %s, not this app's own %q tab, so it stays", canvasID, file.Title, file.User, title)
	}
	if err := api.DeleteCanvasContext(ctx, canvasID); err != nil {
		return fmt.Errorf("canvas %s is full, and deleting it failed: %w", canvasID, err)
	}
	return nil
}

func canvasURL(id identity, canvasID string) string {
	return fmt.Sprintf("%sdocs/%s/%s", id.TeamURL, id.TeamID, canvasID)
}

// TabCanvasContext calls canvases.create itself because slack-go sends no channel_id, which tabs the new canvas in that conversation.
func (c *apiClient) TabCanvasContext(ctx context.Context, channelID, title string, page slack.DocumentContent) (string, error) {
	content, err := json.Marshal(page)
	if err != nil {
		return "", err
	}
	form := url.Values{"channel_id": {channelID}, "title": {title}, "document_content": {string(content)}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint+"canvases.create", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req) //nolint:gosec // G704: the request goes to the Slack API endpoint
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		retry, _ := strconv.Atoi(resp.Header.Get("Retry-After"))
		return "", &slack.RateLimitedError{RetryAfter: time.Duration(retry) * time.Second}
	}
	var answer struct {
		slack.SlackResponse
		CanvasID string `json:"canvas_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&answer); err != nil {
		return "", fmt.Errorf("canvases.create: %s: %w", resp.Status, err)
	}
	if !answer.Ok {
		return "", slack.SlackErrorResponse{Err: answer.Error, Errors: answer.Errors, ResponseMetadata: answer.ResponseMetadata}
	}
	return answer.CanvasID, nil
}

func driveCardsCmd(d cmd.Deps) *cobra.Command {
	var f sessionFlags
	var drive, waits string
	var closed, asJSON bool
	c := &cobra.Command{
		Use:   "drive-cards",
		Short: "Print a line per open question card that names a drive and per PR card still waiting on a tap, oldest first, from stored state",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			body := map[string]any{"name": drive}
			if waits == "" {
				return renderOp(c, d, f, opDriveCards, body, asJSON, func(w io.Writer, res driveCardsResult) { renderDriveCards(w, res, closed) })
			}
			raw, err := callOp(c, d, f, opDriveCards, body)
			if err != nil {
				return err
			}
			card, err := driveCard(raw, waits)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(c.OutOrStdout(), string(card))
			return err
		},
	}
	f.bind(c)
	c.Flags().StringVar(&drive, "drive", "", "the drive whose question cards to list")
	_ = c.MarkFlagRequired("drive")
	c.Flags().StringVar(&waits, "waits", "", "print the one card, open or closed, with this waits id or permalink whole as JSON")
	c.Flags().BoolVar(&closed, "closed", false, "print a line per closed card with what closed it, in place of the open ones")
	c.Flags().BoolVar(&asJSON, "json", false, "print the daemon's JSON result, every card whole")
	return c
}

func renderDriveCards(w io.Writer, res driveCardsResult, closed bool) {
	line := func(waits string, asked time.Time, url, what string) {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", waits, asked.In(pacific).Format(pacificLayout), url, what)
	}
	if closed {
		for _, c := range res.Closed {
			line(c.Waits, c.Asked, c.URL, c.Title+": "+howClosed(c))
		}
		return
	}
	for _, q := range res.Questions {
		line(q.Card.Waits, q.Asked, q.URL, q.Question)
	}
	for _, p := range res.PRCards {
		line(p.Card.Waits, p.Asked, p.URL, strings.Join(p.PRs, " "))
	}
}

func howClosed(c driveClosedCard) string {
	switch {
	case c.ReplacedBy != "":
		return "replaced by " + c.ReplacedBy
	case c.Answer == nil:
		return c.Tapped
	case c.Answer.Withdrawn:
		return "withdrawn: " + c.Answer.Label
	case c.Answer.Other:
		return "answered in words"
	}
	return strings.TrimSpace("answered " + cmp.Or(c.Answer.Label, c.Answer.Tap))
}

func driveCard(raw json.RawMessage, waits string) (json.RawMessage, error) {
	var res driveCardsResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, err
	}
	link, _, _ := strings.Cut(waits, "?")
	names := func(id, url string) bool {
		at, _, _ := strings.Cut(url, "?")
		return id == waits || at == link
	}
	for _, q := range res.Questions {
		if names(q.Card.Waits, q.URL) {
			return json.Marshal(q)
		}
	}
	for _, p := range res.PRCards {
		if names(p.Card.Waits, p.URL) {
			return json.Marshal(p)
		}
	}
	for _, c := range res.Closed {
		if names(c.Waits, c.URL) {
			return json.Marshal(c)
		}
	}
	return nil, fmt.Errorf("no card of this drive, open or closed, has waits id or permalink %s", waits)
}

func canvasCmd(d cmd.Deps) *cobra.Command {
	var f sessionFlags
	var title, canvas string
	c := &cobra.Command{
		Use:   "canvas",
		Short: "Redraw a canvas from the markdown on stdin, or create one as a tab in your DM with the app",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			page, err := io.ReadAll(c.InOrStdin())
			if err != nil {
				return err
			}
			return runOp(c, d, f, opCanvas, map[string]any{"text": string(page), "name": title, "file_id": canvas})
		},
	}
	f.bind(c)
	c.Flags().StringVar(&title, "title", "", "the title a new canvas gets, and the tab it shows as")
	c.Flags().StringVar(&canvas, "canvas", "", "the canvas to redraw; with --title, one Slack no longer has is created again, and this app's own tab of that title that Slack calls full is deleted and created again")
	return c
}
