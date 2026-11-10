package main

import (
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
	opDriveCards daemon.Op = "drive-cards"
	opCanvas     daemon.Op = "canvas"
)

var goneCanvasCodes = map[string]bool{"canvas_not_found": true, "canvas_deleted": true}

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

type driveCardsResult struct {
	TeamURL   string          `json:"team_url"`
	Questions []driveQuestion `json:"questions"`
	PRCards   []drivePRCard   `json:"pr_cards"`
}

type canvasResult struct {
	CanvasID string `json:"canvas_id"`
	URL      string `json:"url"`
	Created  bool   `json:"created"`
}

// openDriveCards lists the waiting cards that name drive and are still unanswered, and every PR card still waiting
// on a tap, a waiting status card that carries PR rows included, oldest first, each with its permalink.
func (l *liveCards) openDriveCards(drive string, link func(channel, ts, thread string) string) driveCardsResult {
	l.mu.Lock()
	defer l.mu.Unlock()
	res := driveCardsResult{Questions: []driveQuestion{}, PRCards: []drivePRCard{}}
	prCard := func(t *liveThread, m *liveMessage) drivePRCard {
		listed := m.Card.allPRs()
		prs := make([]string, 0, len(listed))
		for _, pr := range listed {
			prs = append(prs, pr.URL)
		}
		return drivePRCard{prs, m.At, link(t.Channel, m.TS, t.Thread), m.Card}
	}
	for _, t := range l.threads {
		for _, m := range t.Waiting {
			switch {
			case !t.open(m):
			case m.Card.Drive == drive:
				res.Questions = append(res.Questions, driveQuestion{m.Card.Title, m.Card.Options, m.Card.Recommended, m.At, link(t.Channel, m.TS, t.Thread), m.Card})
			case m.Card.Drive == "" && len(m.Card.allPRs()) > 0:
				res.PRCards = append(res.PRCards, prCard(t, m))
			}
		}
		for _, m := range t.PRCards {
			if m.Card.Waits != "" && t.open(m) {
				res.PRCards = append(res.PRCards, prCard(t, m))
			}
		}
	}
	slices.SortFunc(res.Questions, func(a, b driveQuestion) int { return a.Asked.Compare(b.Asked) })
	slices.SortFunc(res.PRCards, func(a, b drivePRCard) int { return a.Asked.Compare(b.Asked) })
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
	listed := a.cards.openDriveCards(b.Name, id.permalink)
	listed.TeamURL = id.TeamURL
	return ok(listed)
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
	if b.FileID != "" {
		err := api.EditCanvasContext(hc.Ctx, slack.EditCanvasParams{CanvasID: b.FileID, Changes: []slack.CanvasChange{{Operation: "replace", DocumentContent: page}}})
		var refused slack.SlackErrorResponse
		switch {
		case err == nil:
			return ok(canvasResult{CanvasID: b.FileID, URL: canvasURL(id, b.FileID)})
		case !errors.As(err, &refused) || !goneCanvasCodes[refused.Err] || b.Name == "":
			return fail(fallback(actionWrite, err))
		}
	}
	canvasID, err := api.TabCanvasContext(hc.Ctx, id.DMChannelID, b.Name, page)
	if err != nil {
		return fail(fallback(actionWrite, err))
	}
	return ok(canvasResult{CanvasID: canvasID, URL: canvasURL(id, canvasID), Created: true})
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
	var drive string
	c := &cobra.Command{
		Use:   "drive-cards",
		Short: "Print the open question cards that name a drive and the PR cards still waiting on a tap, from stored state",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return runOp(c, d, f, opDriveCards, map[string]any{"name": drive})
		},
	}
	f.bind(c)
	c.Flags().StringVar(&drive, "drive", "", "the drive whose question cards to list")
	_ = c.MarkFlagRequired("drive")
	return c
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
	c.Flags().StringVar(&canvas, "canvas", "", "the canvas to redraw; one Slack no longer has is created again when --title is set")
	return c
}
