package main

import (
	"context"
	"fmt"
	"net/url"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/slack-go/slack"
)

const (
	cardActionPrefix  = "cc-slack-card"
	cardActionsBlock  = "card-actions"
	cardLinksBlock    = "card-links"
	cardAckAction     = cardActionPrefix + ":ack"
	cardPauseAction   = cardActionPrefix + ":pause"
	cardOptionAction  = cardActionPrefix + ":opt:"
	cardLinkAction    = cardActionPrefix + ":link:"
	cardAckLabel      = "Ack"
	cardPauseLabel    = "Pause"
	cardMaxOptions    = 4
	cardMaxOptionLen  = 30
	cardMaxLinks      = 5
	cardMaxHeaderLen  = 150
	cardMaxTitleLen   = 140
	cardMaxBodyLen    = 2800
	cardMaxNameLen    = 80
	cardMaxLabelLen   = 75
	cardMaxURLLen     = 3000
	cardSessionPrefix = 8
	cardTimeLayout    = "3:04pm"
)

var cardStatuses = map[string]string{
	"info":      "ℹ️",
	"progress":  "🔄",
	"needs_you": "🙋",
	"incident":  "🚨",
	"done":      "✅",
}

type statusCard struct {
	Status  string     `json:"status"`
	Title   string     `json:"title"`
	Body    string     `json:"body"`
	Drive   string     `json:"drive"`
	Phase   string     `json:"phase"`
	Links   []cardLink `json:"links"`
	Options []string   `json:"options"`
}

type cardLink struct {
	Label string `json:"label"`
	URL   string `json:"url"`
}

type cardTag struct {
	Session  string
	Repo     string
	Branch   string
	Worktree string
}

func (c statusCard) check() error {
	if _, ok := cardStatuses[c.Status]; !ok {
		return fmt.Errorf("card status %q is not one of info, progress, needs_you, incident, done", c.Status)
	}
	if strings.TrimSpace(c.Title) == "" || utf8.RuneCountInString(c.Title) > cardMaxTitleLen {
		return fmt.Errorf("card needs a title of 1 to %d characters", cardMaxTitleLen)
	}
	if utf8.RuneCountInString(c.Drive) > cardMaxNameLen || utf8.RuneCountInString(c.Phase) > cardMaxNameLen {
		return fmt.Errorf("card drive and phase run to %d characters each", cardMaxNameLen)
	}
	if utf8.RuneCountInString(c.Body) > cardMaxBodyLen {
		return fmt.Errorf("card body runs past %d characters", cardMaxBodyLen)
	}
	if len(c.Links) > cardMaxLinks {
		return fmt.Errorf("card carries %d links, at most %d", len(c.Links), cardMaxLinks)
	}
	for _, l := range c.Links {
		u, err := url.Parse(l.URL)
		if err != nil || u.Scheme != "https" || len(l.URL) > cardMaxURLLen || l.Label == "" || utf8.RuneCountInString(l.Label) > cardMaxLabelLen {
			return fmt.Errorf("card link %q needs a label of up to %d characters and an https url of up to %d", l.URL, cardMaxLabelLen, cardMaxURLLen)
		}
	}
	if len(c.Options) > cardMaxOptions {
		return fmt.Errorf("card offers %d options, at most %d", len(c.Options), cardMaxOptions)
	}
	seen := map[string]bool{cardAckLabel: true, cardPauseLabel: true}
	for _, o := range c.Options {
		if o == "" || utf8.RuneCountInString(o) > cardMaxOptionLen {
			return fmt.Errorf("card option %q must be 1 to %d characters", o, cardMaxOptionLen)
		}
		if seen[o] {
			return fmt.Errorf("card option %q repeats a button", o)
		}
		seen[o] = true
	}
	return nil
}

func (c statusCard) emoji() string { return cardStatuses[c.Status] }

func (c statusCard) text() string {
	if c.Body == "" {
		return c.emoji() + " " + c.Title
	}
	return c.emoji() + " " + c.Title + " — " + c.Body
}

func gitCardTag(ctx context.Context, session, dir string) cardTag {
	tag := cardTag{Session: session[:min(len(session), cardSessionPrefix)]}
	git := func(args ...string) string {
		out, err := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...).Output() //nolint:gosec // G204: argv to git, never a shell
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}
	top := git("rev-parse", "--show-toplevel")
	if top == "" {
		return tag
	}
	common := git("rev-parse", "--path-format=absolute", "--git-common-dir")
	main := filepath.Dir(common)
	tag.Repo = filepath.Base(main)
	tag.Branch = git("rev-parse", "--abbrev-ref", "HEAD")
	if top != main {
		tag.Worktree = filepath.Base(top)
	}
	return tag
}

func (c statusCard) context(tag cardTag, now time.Time, stamp string) slack.Block {
	var parts []string
	if c.Drive != "" {
		parts = append(parts, "*"+c.Drive+"*")
	}
	if c.Phase != "" {
		parts = append(parts, c.Phase)
	}
	if tag.Repo != "" {
		parts = append(parts, "`"+tag.Repo+"@"+tag.Branch+"`")
	}
	if tag.Worktree != "" {
		parts = append(parts, "worktree `"+tag.Worktree+"`")
	}
	if tag.Session != "" {
		parts = append(parts, "session `"+tag.Session+"`")
	}
	parts = append(parts, stamp+now.In(pacific).Format(cardTimeLayout))
	return slack.NewContextBlock("card-tag", slack.NewTextBlockObject(slack.MarkdownType, strings.Join(parts, " · "), false, false))
}

func header(text string) slack.Block {
	if utf8.RuneCountInString(text) > cardMaxHeaderLen {
		text = string([]rune(text)[:cardMaxHeaderLen-1]) + "…"
	}
	return slack.NewHeaderBlock(slack.NewTextBlockObject(slack.PlainTextType, text, true, false))
}

func (c statusCard) links() []slack.Block {
	if len(c.Links) == 0 {
		return nil
	}
	buttons := make([]slack.BlockElement, 0, len(c.Links))
	for i, l := range c.Links {
		b := slack.NewButtonBlockElement(fmt.Sprintf("%s%d", cardLinkAction, i), "", slack.NewTextBlockObject(slack.PlainTextType, l.Label, true, false))
		b.URL = l.URL
		buttons = append(buttons, b)
	}
	return []slack.Block{slack.NewActionBlock(cardLinksBlock, buttons...)}
}

func button(actionID, label string) *slack.ButtonBlockElement {
	return slack.NewButtonBlockElement(actionID, label, slack.NewTextBlockObject(slack.PlainTextType, label, true, false))
}

func (c statusCard) actions() slack.Block {
	buttons := make([]slack.BlockElement, 0, len(c.Options)+2)
	for i, o := range c.Options {
		buttons = append(buttons, button(fmt.Sprintf("%s%d", cardOptionAction, i), o))
	}
	pause := button(cardPauseAction, cardPauseLabel)
	pause.Style = slack.StyleDanger
	buttons = append(buttons, button(cardAckAction, cardAckLabel), pause)
	return slack.NewActionBlock(cardActionsBlock, buttons...)
}

func (c statusCard) replyBlocks(tag cardTag, now time.Time) []slack.Block {
	blocks := []slack.Block{header(c.emoji() + " " + c.Title)}
	if c.Body != "" {
		blocks = append(blocks, slack.NewSectionBlock(slack.NewTextBlockObject(slack.MarkdownType, c.Body, false, false), nil, nil))
	}
	blocks = append(blocks, c.links()...)
	return append(blocks, c.actions(), c.context(tag, now, ""))
}

func (c statusCard) rootBlocks(tag cardTag, now time.Time) []slack.Block {
	name := c.Drive
	switch {
	case name != "":
	case tag.Repo != "":
		name = tag.Repo + "@" + tag.Branch
	default:
		name = "Claude session " + tag.Session
	}
	latest := "*" + c.Title + "*"
	if c.Body != "" {
		latest += "\n" + c.Body
	}
	blocks := []slack.Block{
		header(c.emoji() + " " + name),
		slack.NewSectionBlock(slack.NewTextBlockObject(slack.MarkdownType, latest, false, false), nil, nil),
	}
	blocks = append(blocks, c.links()...)
	return append(blocks, c.context(tag, now, "updated "))
}
