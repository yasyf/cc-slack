package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/slack-go/slack"
)

const (
	cardActionPrefix  = "cc-slack-card"
	cardActionsBlock  = "card-actions"
	cardLinksBlock    = "card-links"
	cardAskBlock      = "card-ask-"
	cardWaitBlock     = "card-wait"
	cardTappedBlock   = "card-tapped-"
	cardRowBlock      = "card-row-"
	cardOptionBlock   = "card-option-"
	cardFoldBlock     = "card-fold"
	cardAckAction     = cardActionPrefix + ":ack"
	cardFoldAction    = cardActionPrefix + ":fold"
	cardPauseAction   = cardActionPrefix + ":pause"
	cardOptionAction  = cardActionPrefix + ":opt:"
	cardLinkAction    = cardActionPrefix + ":link:"
	cardAskAction     = cardActionPrefix + ":ask:"
	cardWaitAction    = cardActionPrefix + ":wait:"
	cardExplainAction = cardActionPrefix + ":explain:"
	cardOtherAction   = cardActionPrefix + ":other:"
	cardWroteAction   = cardActionPrefix + ":wrote:"
	cardRowAction     = cardActionPrefix + ":row:"
	cardExplainLabel  = "Explain more"
	cardOtherLabel    = "Other"
	cardAnswerLabel   = "Answer"
	cardRecommended   = "(Recommended)"
	cardAckLabel      = "Ack"
	cardPauseLabel    = "Pause"
	cardPickLabel     = "Pick"
	cardMaxOptions    = 4
	cardMaxOptionLen  = cardMaxLabelLen - len(" "+cardRecommended)
	cardMaxShortWords = 2
	cardMaxLinks      = 5
	cardMaxRefs       = 10
	cardMaxSupersedes = 5
	cardMaxAsks       = 5
	cardMaxRows       = 20
	cardMaxRowButtons = 4
	cardMaxFacts      = 10
	cardMaxRollup     = 50
	cardMaxTimeline   = 5
	cardMaxHeaderLen  = 150
	cardMaxTitleLen   = 140
	cardMaxBodyLen    = 40000
	cardMaxFieldLen   = 2000
	cardMaxPRs        = 10
	cardMaxPRRows     = 8
	cardQuietBody     = 400
	cardMaxNameLen    = 80
	cardMaxLabelLen   = 75
	cardMaxStatusLen  = 40
	cardMaxDetailLen  = 200
	cardMaxNoteLen    = 300
	cardMaxSectionLen = 3000
	cardMaxSteps      = 1000
	cardMaxURLLen     = 3000
	cardProgressCells = 10
	cardSessionPrefix = 8
	cardTimeLayout    = "3:04pm"
	cardShortHead     = 7
	cardPermission    = "slack-"
	cardWaitHint      = "⏸ *The lane that asked is stopped until you answer.* Tap an option or reply with its number, or tap Other to write your own answer."
	cardWriteHint     = "⏸ *The lane that asked is stopped until you answer.* Tap Answer to write it."
	cardPRHint        = "Tap an option or reply with its number, or tap Other to write your own answer."
)

var (
	cardStatuses = map[string]string{
		"info":      "ℹ️",
		"progress":  "🔄",
		"needs_you": "🙋",
		"incident":  "🚨",
		"done":      "✅",
	}
	cardAskStyles   = map[string]slack.Style{"": slack.StyleDefault, "primary": slack.StylePrimary, "danger": slack.StyleDanger}
	askIDPattern    = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,64}$`)
	mrkdwnEscapes   = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "|", "¦")
	cardWordEscapes = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	slackToken      = regexp.MustCompile(`<[^<>\s][^<>]*>`)
	cardJargon      = []struct {
		pattern *regexp.Regexp
		mixed   bool
		fix     string
	}{
		{regexp.MustCompile(`\b[CDG][A-Z0-9]{8,}\b`), true, "a Slack channel id; name the channel as #name, or the person a direct message goes to"},
		{regexp.MustCompile(`\b\d{10}\.\d{6}\b|\bp\d{16}\b`), false, "a thread ts; say who started the thread, when, and what it is about"},
		{regexp.MustCompile(`\b[0-9a-f]{8,}\b`), true, "a hash; say what it stands for"},
		{regexp.MustCompile(`\b[a-z]+(?:-[a-z0-9]+)+-\d+\b`), false, "a lane name; say what that agent is doing for the user"},
	}
)

type statusCard struct {
	Status      string            `json:"status"`
	Title       string            `json:"title"`
	Body        string            `json:"body"`
	Drive       string            `json:"drive"`
	Phase       string            `json:"phase"`
	Links       []cardLink        `json:"links"`
	Options     []string          `json:"options"`
	Refs        []cardLink        `json:"refs,omitempty"`
	Progress    *cardProgress     `json:"progress,omitempty"`
	StartedAt   *time.Time        `json:"started_at,omitempty"`
	ETA         *time.Time        `json:"eta,omitempty"`
	Asks        []cardAsk         `json:"asks,omitempty"`
	Facts       []cardFact        `json:"facts,omitempty"`
	Rollup      *cardRollup       `json:"rollup,omitempty"`
	Waits       string            `json:"waits,omitempty"`
	Recommended string            `json:"recommended,omitempty"`
	Group       string            `json:"group,omitempty"`
	Key         string            `json:"key,omitempty"`
	Rows        []cardRow         `json:"rows,omitempty"`
	Supersedes  []string          `json:"supersedes,omitempty"`
	PRs         []cardPR          `json:"prs,omitempty"`
	Hints       map[string]string `json:"hints,omitempty"`
	Replaces    string            `json:"replaces,omitempty"`

	Images []slack.FileSummary `json:"images,omitempty"`
}

type cardRow struct {
	ID      string   `json:"id"`
	Text    string   `json:"text"`
	Buttons []string `json:"buttons"`
	PR      *cardPR  `json:"pr,omitempty"`
}

type cardPR struct {
	URL         string `json:"url"`
	Title       string `json:"title,omitempty"`
	Head        string `json:"head,omitempty"`
	Review      string `json:"review,omitempty"`
	Change      string `json:"change"`
	Plan        string `json:"plan"`
	Risk        string `json:"risk"`
	Blocks      string `json:"blocks"`
	Description string `json:"description,omitempty"`
}

type cardLink struct {
	Label string `json:"label"`
	URL   string `json:"url"`
}

type cardProgress struct {
	Step  int    `json:"step"`
	Total int    `json:"total"`
	Label string `json:"label,omitempty"`
}

type cardAsk struct {
	ID          string     `json:"id"`
	Label       string     `json:"label"`
	Description string     `json:"description,omitempty"`
	Style       string     `json:"style,omitempty"`
	Confirm     string     `json:"confirm,omitempty"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
}

type cardFact struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

type cardRollup struct {
	Lanes []rollupLane `json:"lanes,omitempty"`
	PRs   []rollupPR   `json:"prs,omitempty"`
}

type rollupLane struct {
	Name   string `json:"name"`
	Status string `json:"status,omitempty"`
	Detail string `json:"detail,omitempty"`
}

type rollupPR struct {
	URL    string `json:"url"`
	Label  string `json:"label,omitempty"`
	Status string `json:"status,omitempty"`
}

type cardTag struct {
	Session  string `json:"session"`
	Repo     string `json:"repo"`
	Branch   string `json:"branch"`
	Worktree string `json:"worktree"`
}

type askAnswer struct {
	At    time.Time `json:"at"`
	Label string    `json:"label,omitempty"`
	Other bool      `json:"other,omitempty"`
}

func runes(s string) int { return utf8.RuneCountInString(s) }

func checkURL(raw, tailnet string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || len(raw) > cardMaxURLLen {
		return false
	}
	return u.Scheme == "https" || u.Scheme == "http" && tailnet != "" && strings.HasSuffix(u.Hostname(), "."+tailnet)
}

func (c *statusCard) fill(text string) {
	c.Body = strings.Join(slices.DeleteFunc([]string{text, c.Body}, func(s string) bool { return s == "" }), "\n")
	if c.Status == "" && c.Waits != "" {
		c.Status = "needs_you"
	} else if c.Status == "" {
		c.Status = "info"
	}
	if strings.TrimSpace(c.Title) == "" {
		first, _, _ := strings.Cut(strings.TrimSpace(c.Body), "\n")
		c.Title = string([]rune(first)[:min(runes(first), cardMaxTitleLen)])
	}
	for i, o := range c.Options {
		if strings.Contains(o, cardRecommended) {
			label := strings.TrimSpace(strings.Replace(o, cardRecommended, "", 1))
			c.Options[i] = label
			if c.Recommended == "" || c.Recommended == o {
				c.Recommended = label
			}
		}
	}
}

func (c statusCard) check(now time.Time, rootOnly bool, tailnet string) error {
	return errors.Join(c.checkMirrored(now, rootOnly, tailnet), c.checkAnswers())
}

func (c statusCard) checkAnswers() error {
	switch {
	case c.Waits == "":
		return nil
	case len(c.Options) == 0:
		return errors.New("a waiting card needs its answers as options")
	case c.Recommended == "":
		return errors.New("a waiting card recommends one of its options")
	}
	return nil
}

func (c statusCard) checkMirrored(now time.Time, rootOnly bool, tailnet string) error {
	return errors.Join(
		c.checkHeading(rootOnly),
		c.checkLinks(tailnet),
		c.checkOptions(rootOnly),
		c.checkWaits(rootOnly),
		c.checkKey(rootOnly),
		c.checkRefs(),
		c.checkAsks(now),
		c.checkFacts(),
		c.checkRollup(),
		c.checkRows(rootOnly),
		c.checkPRs(),
		c.checkReplaces(),
		c.checkSupersedes(),
		c.checkPermissionWords(),
	)
}

func (c statusCard) checkPermissionWords() error {
	if !strings.HasPrefix(c.Waits, cardPermission) && !permissionCard.MatchString(c.Waits) {
		return nil
	}
	shown := []string{c.Title, c.Body}
	for _, f := range c.Facts {
		shown = append(shown, f.Label, f.Value)
	}
	text := slackToken.ReplaceAllString(strings.Join(shown, "\n"), " ")
	var errs []error
	for _, j := range cardJargon {
		found := slices.DeleteFunc(j.pattern.FindAllString(text, -1), func(m string) bool {
			return j.mixed && (!strings.ContainsAny(m, "0123456789") || !strings.ContainsFunc(m, unicode.IsLetter))
		})
		if len(found) > 0 {
			errs = append(errs, fmt.Errorf("a Slack permission card says where and to whom in words: %q is %s", found[0], j.fix))
		}
	}
	return errors.Join(errs...)
}

func (c statusCard) checkHeading(rootOnly bool) error {
	if rootOnly && c.Title == "" {
		if c.Status != "" || c.Body != "" {
			return errors.New("a root-only card without a title takes no status or body")
		}
	} else {
		if _, ok := cardStatuses[c.Status]; !ok {
			return fmt.Errorf("card status %q is not one of info, progress, needs_you, incident, done", c.Status)
		}
		if strings.TrimSpace(c.Title) == "" || runes(c.Title) > cardMaxTitleLen {
			return fmt.Errorf("card needs a title of 1 to %d characters", cardMaxTitleLen)
		}
	}
	if runes(c.Drive) > cardMaxNameLen || runes(c.Phase) > cardMaxNameLen {
		return fmt.Errorf("card drive and phase run to %d characters each", cardMaxNameLen)
	}
	if rootOnly && runes(c.Body) > cardMaxVisible {
		return fmt.Errorf("a root card's body runs past %d characters", cardMaxVisible)
	}
	return nil
}

func (c statusCard) checkPRs() error {
	if len(c.PRs) > cardMaxPRs {
		return fmt.Errorf("card carries %d PRs, at most %d", len(c.PRs), cardMaxPRs)
	}
	for _, p := range c.PRs {
		if err := p.check(); err != nil {
			return err
		}
	}
	return nil
}

func (p cardPR) check() error {
	if id, ok := parseRef(p.URL); !ok || id.Kind != refPR {
		return fmt.Errorf("card PR %q is not a https://github.com/<owner>/<repo>/pull/<n> url", p.URL)
	}
	return nil
}

func (p cardPR) fields() []cardFact {
	return []cardFact{{"What changes", p.Change}, {"Against the plan", p.Plan}, {"Risk", p.Risk}, {"Blocks landing", p.Blocks}}
}

func (c statusCard) checkReplaces() error {
	if c.Replaces == "" {
		return nil
	}
	if ref, err := parsePermalink(c.Replaces); err != nil || ref.ts == "" {
		return fmt.Errorf("card replaces %q, which is not a Slack message permalink", c.Replaces)
	}
	if c.Group != "" || c.Key != "" || len(c.Asks)+len(c.Rows) > 0 || c.Waits == "" && len(c.Options) > 0 {
		return errors.New("a card that replaces a posted message carries buttons only as a waiting card: no group, key, asks, or rows, and options need waits")
	}
	return nil
}

func (c statusCard) checkLinks(tailnet string) error {
	if len(c.Links) > cardMaxLinks {
		return fmt.Errorf("card carries %d links, at most %d", len(c.Links), cardMaxLinks)
	}
	for _, l := range c.Links {
		if !checkURL(l.URL, tailnet) || l.Label == "" || runes(l.Label) > cardMaxLabelLen {
			return fmt.Errorf("card link %q needs a label of up to %d characters and an https url, or an http url on the [links] tailnet, of up to %d", l.URL, cardMaxLabelLen, cardMaxURLLen)
		}
	}
	return nil
}

func (c statusCard) checkOptions(rootOnly bool) error {
	if rootOnly && len(c.Options) > 0 {
		return errors.New("options ride reply cards; a root-only card carries none")
	}
	if len(c.Options) > cardMaxOptions {
		return fmt.Errorf("card offers %d options, at most %d", len(c.Options), cardMaxOptions)
	}
	seen := map[string]bool{cardAckLabel: true, cardPauseLabel: true}
	for _, o := range c.Options {
		if o == "" || runes(o) > cardMaxOptionLen {
			return fmt.Errorf("card option %q must be 1 to %d characters", o, cardMaxOptionLen)
		}
		if seen[o] {
			return fmt.Errorf("card option %q repeats a button", o)
		}
		seen[o] = true
	}
	return nil
}

func (c statusCard) checkSupersedes() error {
	switch {
	case len(c.Supersedes) == 0:
		return nil
	case c.Waits == "":
		return errors.New("supersedes names the earlier posts a waiting card asks again; set waits too")
	case len(c.Supersedes) > cardMaxSupersedes:
		return fmt.Errorf("card supersedes %d posts, at most %d", len(c.Supersedes), cardMaxSupersedes)
	}
	for _, post := range c.Supersedes {
		if ref, err := parsePermalink(post); err != nil || ref.ts == "" {
			return fmt.Errorf("card supersedes %q, which is not a Slack message permalink", post)
		}
	}
	return nil
}

func (c statusCard) supersedesIn(channel string) bool {
	return !slices.ContainsFunc(c.Supersedes, func(post string) bool {
		ref, _ := parsePermalink(post)
		return ref.channelID != channel
	})
}

func (c statusCard) checkWaits(rootOnly bool) error {
	switch {
	case c.Waits == "" && c.Recommended != "":
		return errors.New("recommended marks a waiting card's answer; set waits too")
	case c.Waits == "":
		return nil
	case !askIDPattern.MatchString(c.Waits):
		return fmt.Errorf("card waits id %q must be 1 to 64 letters, digits, dots, colons, underscores, or hyphens", c.Waits)
	case rootOnly:
		return errors.New("a waiting card is a reply card; a root-only card takes no waits")
	case len(c.Asks) > 0:
		return errors.New("a waiting card asks one question; its answers go in options, not asks")
	case slices.Contains(c.Options, cardExplainLabel), slices.Contains(c.Options, cardOtherLabel):
		return fmt.Errorf("a waiting card carries its own %s and %s buttons; no option may repeat them", cardOtherLabel, cardExplainLabel)
	case slices.ContainsFunc(c.Options, func(o string) bool { return strings.Contains(o, cardRecommended) }):
		return fmt.Errorf("name the recommended answer in recommended, never as %s in an option", cardRecommended)
	case c.Recommended != "" && !slices.Contains(c.Options, c.Recommended):
		return fmt.Errorf("a waiting card recommends one of its options: recommended %q is not among them", c.Recommended)
	}
	return nil
}

func (c statusCard) checkKey(rootOnly bool) error {
	switch {
	case c.Key == "":
		return nil
	case !askIDPattern.MatchString(c.Key):
		return fmt.Errorf("card key %q must be 1 to 64 letters, digits, dots, colons, underscores, or hyphens", c.Key)
	case rootOnly || c.Waits != "" || c.Group != "":
		return errors.New("a key redraws a reply card; a root-only, waiting, or group card is redrawn by its own id")
	}
	return nil
}

func (c statusCard) checkFacts() error {
	if p := c.Progress; p != nil && (p.Total < 1 || p.Total > cardMaxSteps || p.Step < 0 || p.Step > p.Total || runes(p.Label) > cardMaxLabelLen) {
		return fmt.Errorf("card progress needs 0 <= step <= total <= %d and a label of up to %d characters", cardMaxSteps, cardMaxLabelLen)
	}
	if len(c.Facts) > cardMaxFacts {
		return fmt.Errorf("card carries %d facts, at most %d", len(c.Facts), cardMaxFacts)
	}
	for _, f := range c.Facts {
		if f.Label == "" || runes(f.Label) > cardMaxNameLen || f.Value == "" || runes(f.Value) > cardMaxFieldLen-cardMaxNameLen {
			return fmt.Errorf("card fact %q needs a label of up to %d characters and a value of up to %d", f.Label, cardMaxNameLen, cardMaxFieldLen-cardMaxNameLen)
		}
	}
	return nil
}

func (c statusCard) checkRefs() error {
	if len(c.Refs) > cardMaxRefs {
		return fmt.Errorf("card carries %d refs, at most %d", len(c.Refs), cardMaxRefs)
	}
	for _, r := range c.Refs {
		if _, ok := parseRef(r.URL); !ok {
			return fmt.Errorf("card ref %q is neither a https://github.com/<owner>/<repo>/pull/<n> nor a https://buildkite.com/<org>/<pipeline>/builds/<n> url", r.URL)
		}
		if runes(r.Label) > cardMaxLabelLen {
			return fmt.Errorf("card ref label %q runs past %d characters", r.Label, cardMaxLabelLen)
		}
	}
	return nil
}

func (c statusCard) checkAsks(now time.Time) error {
	if len(c.Asks) > cardMaxAsks {
		return fmt.Errorf("card carries %d asks, at most %d", len(c.Asks), cardMaxAsks)
	}
	ids := map[string]bool{}
	for _, k := range c.Asks {
		if !askIDPattern.MatchString(k.ID) || ids[k.ID] {
			return fmt.Errorf("card ask id %q must be unique and 1 to 64 letters, digits, dots, colons, underscores, or hyphens", k.ID)
		}
		ids[k.ID] = true
		if k.Label == "" || runes(k.Label) > cardMaxLabelLen || runes(k.Description) > cardMaxNoteLen || runes(k.Confirm) > cardMaxNoteLen {
			return fmt.Errorf("card ask %q needs a label of up to %d characters and a description and confirm of up to %d", k.ID, cardMaxLabelLen, cardMaxNoteLen)
		}
		if _, ok := cardAskStyles[k.Style]; !ok {
			return fmt.Errorf("card ask %q style %q is not primary or danger", k.ID, k.Style)
		}
		if k.ExpiresAt != nil && !k.ExpiresAt.After(now) {
			return fmt.Errorf("card ask %q expired at %s, before it was posted", k.ID, k.ExpiresAt.Format(time.RFC3339))
		}
	}
	return nil
}

func (c statusCard) checkRows(rootOnly bool) error {
	switch {
	case c.Group == "" && len(c.Rows) > 0:
		return errors.New("rows ride a group card; set group too")
	case c.Group == "":
		return nil
	case !askIDPattern.MatchString(c.Group):
		return fmt.Errorf("card group id %q must be 1 to 64 letters, digits, dots, colons, underscores, or hyphens", c.Group)
	case rootOnly || c.Waits != "" || len(c.Options) > 0 || len(c.Asks) > 0:
		return errors.New("a group card is a reply card whose rows carry its buttons; it takes no root_only, waits, options, or asks")
	case len(c.Rows) > cardMaxRows:
		return fmt.Errorf("group card carries %d rows, at most %d", len(c.Rows), cardMaxRows)
	}
	if slices.ContainsFunc(c.Rows, func(r cardRow) bool { return r.PR != nil }) && len(c.Rows) > cardMaxPRRows {
		return fmt.Errorf("group card carries %d PR rows, at most %d", len(c.Rows), cardMaxPRRows)
	}
	ids := map[string]bool{}
	for _, r := range c.Rows {
		if ids[r.ID] {
			return fmt.Errorf("card row id %q repeats", r.ID)
		}
		ids[r.ID] = true
		if err := r.check(); err != nil {
			return err
		}
	}
	return nil
}

func (r cardRow) check() error {
	if !askIDPattern.MatchString(r.ID) {
		return fmt.Errorf("card row id %q must be 1 to 64 letters, digits, dots, colons, underscores, or hyphens", r.ID)
	}
	if strings.TrimSpace(r.Text) == "" && r.PR == nil {
		return fmt.Errorf("card row %q needs a PR or text", r.ID)
	}
	if r.PR != nil {
		if err := r.PR.check(); err != nil {
			return err
		}
	}
	if len(r.Buttons) == 0 || len(r.Buttons) > cardMaxRowButtons {
		return fmt.Errorf("card row %q carries %d buttons, 1 to %d", r.ID, len(r.Buttons), cardMaxRowButtons)
	}
	seen := map[string]bool{}
	for _, b := range r.Buttons {
		if b == "" || runes(b) > cardMaxLabelLen || seen[b] {
			return fmt.Errorf("card row %q button %q must be unique and 1 to %d characters", r.ID, b, cardMaxLabelLen)
		}
		seen[b] = true
	}
	return nil
}

func (c statusCard) checkRollup() error {
	r := c.Rollup
	if r == nil {
		return nil
	}
	if len(r.Lanes) > cardMaxRollup || len(r.PRs) > cardMaxRollup {
		return fmt.Errorf("card rollup carries at most %d lanes and %d prs", cardMaxRollup, cardMaxRollup)
	}
	for _, l := range r.Lanes {
		if l.Name == "" || runes(l.Name) > cardMaxNameLen || runes(l.Status) > cardMaxStatusLen || runes(l.Detail) > cardMaxDetailLen {
			return fmt.Errorf("rollup lane %q needs a name of up to %d characters, a status of up to %d, and a detail of up to %d",
				l.Name, cardMaxNameLen, cardMaxStatusLen, cardMaxDetailLen)
		}
	}
	for _, p := range r.PRs {
		if !checkURL(p.URL, "") || runes(p.Label) > cardMaxLabelLen || runes(p.Status) > cardMaxStatusLen {
			return fmt.Errorf("rollup pr %q needs an https url, a label of up to %d characters, and a status of up to %d", p.URL, cardMaxLabelLen, cardMaxStatusLen)
		}
	}
	return nil
}

func (c statusCard) refIDs() []refID {
	var ids []refID
	for _, r := range c.Refs {
		if id, ok := parseRef(r.URL); ok {
			ids = append(ids, id)
		}
	}
	for _, p := range c.allPRs() {
		if id, ok := parseRef(p.URL); ok {
			ids = append(ids, id)
		}
	}
	if c.Rollup != nil {
		for _, p := range c.Rollup.PRs {
			if id, ok := parseRef(p.URL); ok {
				ids = append(ids, id)
			}
		}
	}
	return ids
}

func (c statusCard) allPRs() []*cardPR {
	prs := make([]*cardPR, 0, len(c.PRs)+len(c.Rows))
	for i := range c.PRs {
		prs = append(prs, &c.PRs[i])
	}
	for _, r := range c.Rows {
		if r.PR != nil {
			prs = append(prs, r.PR)
		}
	}
	return prs
}

type cardOffer struct {
	pr, head, answers string
}

func (c statusCard) offer() cardOffer {
	prs := c.allPRs()
	if len(prs) != 1 || prs[0].Head == "" {
		return cardOffer{}
	}
	id, ok := parseRef(prs[0].URL)
	if !ok || id.Kind != refPR {
		return cardOffer{}
	}
	return cardOffer{id.key(), prs[0].Head, string(mustJSON(slices.Sorted(slices.Values(c.answers()))))}
}

func (o cardOffer) same(other cardOffer) bool {
	return o != cardOffer{} && o == other
}

func (c statusCard) merged(next statusCard) statusCard {
	if next.Title != "" {
		c.Status, c.Title, c.Body = next.Status, next.Title, next.Body
	}
	c.Drive = cmp.Or(next.Drive, c.Drive)
	c.Phase = cmp.Or(next.Phase, c.Phase)
	if next.Links != nil {
		c.Links = next.Links
	}
	if next.Refs != nil {
		c.Refs = next.Refs
	}
	if next.Progress != nil {
		c.Progress = next.Progress
	}
	if next.StartedAt != nil {
		c.StartedAt = next.StartedAt
	}
	if next.ETA != nil {
		c.ETA = next.ETA
	}
	if next.Facts != nil {
		c.Facts = next.Facts
	}
	if next.Rollup != nil {
		c.Rollup = next.Rollup
	}
	c.Options, c.Asks, c.Waits, c.Recommended, c.Group, c.Rows, c.Images = nil, nil, "", "", "", nil, nil
	c.PRs, c.Hints, c.Replaces = nil, nil, ""
	return c
}

func (c statusCard) emoji() string { return cardStatuses[c.Status] }

func (c statusCard) text() string {
	if c.Body == "" {
		return c.emoji() + " " + c.Title
	}
	return clipRunes(c.emoji()+" "+c.Title+" — "+c.Body, cardMaxSectionLen)
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

type cardView struct {
	tag      cardTag
	at       time.Time
	now      time.Time
	refs     map[string]refState
	answers  map[string]askAnswer
	asks     []cardAsk
	timeline []timelineEntry
	tapped   string
	hint     string
	final    bool
	unfolded bool
}

type timelineEntry struct {
	At    time.Time `json:"at"`
	Emoji string    `json:"emoji"`
	Title string    `json:"title"`
}

func clock(t, now time.Time) string {
	t, now = t.In(pacific), now.In(pacific)
	if t.YearDay() != now.YearDay() || t.Year() != now.Year() {
		return t.Format("Jan 2 " + cardTimeLayout)
	}
	return t.Format(cardTimeLayout)
}

func span(d time.Duration) string {
	d = d.Round(time.Minute)
	switch {
	case d < time.Minute:
		return "under a minute"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
}

func mrkdwn(blockID, text string) slack.Block {
	return slack.NewContextBlock(blockID, slack.NewTextBlockObject(slack.MarkdownType, text, false, false))
}

func section(blockID, text string) slack.Block {
	return slack.NewSectionBlock(slack.NewTextBlockObject(slack.MarkdownType, text, false, false), nil, nil, slack.SectionBlockOptionBlockID(blockID))
}

func bulleted(head string, lines []string) string {
	out := head
	for i, l := range lines {
		more := fmt.Sprintf("\n+%d more", len(lines)-i)
		if runes(out)+1+runes(l)+runes(more) > cardMaxSectionLen {
			return out + more
		}
		out += "\n" + l
	}
	return out
}

func slackLink(target, label string) string {
	return "<" + target + "|" + mrkdwnEscapes.Replace(label) + ">"
}

func (c statusCard) context(tag cardTag, now time.Time, stamp string) slack.Block {
	var parts []string
	if c.Drive != "" {
		parts = append(parts, c.Drive)
	} else if tag.Repo != "" {
		parts = append(parts, tag.Repo)
	}
	if c.Phase != "" {
		parts = append(parts, c.Phase)
	}
	parts = append(parts, stamp+now.In(pacific).Format(cardTimeLayout))
	return mrkdwn("card-tag", strings.Join(parts, " · "))
}

func clipRunes(text string, limit int) string {
	if runes(text) <= limit {
		return text
	}
	return string([]rune(text)[:limit-1]) + "…"
}

func header(text string) slack.Block {
	return slack.NewHeaderBlock(slack.NewTextBlockObject(slack.PlainTextType, clipRunes(strings.TrimSpace(text), cardMaxHeaderLen), true, false))
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

type cardPick struct {
	actionID, value, option, label string
}

func (c statusCard) picks() []cardPick {
	answers := c.answers()
	picks := make([]cardPick, 0, len(answers))
	for i, o := range answers {
		p := cardPick{actionID: fmt.Sprintf("%s%d", cardOptionAction, i), value: o, option: o, label: o}
		if c.Waits != "" {
			p.actionID = fmt.Sprintf("%s%d:%s", cardWaitAction, i, c.Waits)
			p.value = string(mustJSON(askTap{ID: c.Waits, Label: o, Question: c.Title, Options: answers}))
		}
		if o == c.Recommended {
			p.label += " " + cardRecommended
		}
		picks = append(picks, p)
	}
	return picks
}

func (c statusCard) listed() bool {
	return len(c.Hints) > 0 || slices.ContainsFunc(c.Options, func(o string) bool { return len(strings.Fields(o)) > cardMaxShortWords })
}

func (c statusCard) optionRows(live bool) []slack.Block {
	if len(c.Options) == 0 || !c.listed() {
		return nil
	}
	picks := c.picks()
	rows := make([]slack.Block, 0, len(picks))
	for i, p := range picks {
		line := fmt.Sprintf("*%d. %s*", i+1, mrkdwnEscapes.Replace(p.option))
		if p.option == c.Recommended {
			line += " _" + cardRecommended + "_"
		}
		parts := splitSection(line+"\n"+c.Hints[p.option], cardMaxSectionLen)
		if c.Hints[p.option] == "" {
			parts = []string{line}
		}
		var pick *slack.Accessory
		if live {
			b := slack.NewButtonBlockElement(p.actionID, p.value, slack.NewTextBlockObject(slack.PlainTextType, cardPickLabel, true, false))
			if p.option == c.Recommended {
				b.Style = slack.StylePrimary
			}
			pick = slack.NewAccessory(b)
		}
		rows = append(rows, slack.NewSectionBlock(slack.NewTextBlockObject(slack.MarkdownType, parts[0], false, false), nil, pick, slack.SectionBlockOptionBlockID(fmt.Sprintf("%s%d", cardOptionBlock, i))))
		for _, more := range parts[1:] {
			rows = append(rows, slack.NewSectionBlock(slack.NewTextBlockObject(slack.MarkdownType, more, false, false), nil, nil))
		}
	}
	return rows
}

func (c statusCard) inlinePicks() []cardPick {
	if c.listed() {
		return nil
	}
	return c.picks()
}

func (c statusCard) actions() slack.Block {
	picks := c.inlinePicks()
	buttons := make([]slack.BlockElement, 0, len(picks)+2)
	for _, p := range picks {
		buttons = append(buttons, slack.NewButtonBlockElement(p.actionID, p.value, slack.NewTextBlockObject(slack.PlainTextType, p.label, true, false)))
	}
	pause := button(cardPauseAction, cardPauseLabel)
	pause.Style = slack.StyleDanger
	buttons = append(buttons, button(cardAckAction, cardAckLabel), pause)
	return slack.NewActionBlock(cardActionsBlock, buttons...)
}

func (c statusCard) answers() []string {
	if c.Recommended == "" {
		return slices.Clone(c.Options)
	}
	rest := slices.DeleteFunc(slices.Clone(c.Options), func(o string) bool { return o == c.Recommended })
	return append([]string{c.Recommended}, rest...)
}

func (c statusCard) waitActions() slack.Block {
	picks := c.inlinePicks()
	answers := c.answers()
	buttons := make([]slack.BlockElement, 0, len(picks)+2)
	for _, p := range picks {
		b := slack.NewButtonBlockElement(p.actionID, p.value, slack.NewTextBlockObject(slack.PlainTextType, p.label, true, false))
		if p.option == c.Recommended {
			b.Style = slack.StylePrimary
		}
		buttons = append(buttons, b)
	}
	other := string(mustJSON(askTap{ID: c.Waits, Label: cardOtherLabel, Question: c.Title, Options: answers}))
	explain := string(mustJSON(askTap{ID: c.Waits, Label: cardExplainLabel, Question: c.Title}))
	write := cardOtherLabel
	if len(answers) == 0 {
		write = cardAnswerLabel
	}
	buttons = append(buttons,
		slack.NewButtonBlockElement(cardOtherAction+c.Waits, other, slack.NewTextBlockObject(slack.PlainTextType, write, true, false)),
		slack.NewButtonBlockElement(cardExplainAction+c.Waits, explain, slack.NewTextBlockObject(slack.PlainTextType, cardExplainLabel, true, false)))
	return slack.NewActionBlock(cardActionsBlock, buttons...)
}

func (c statusCard) waitHint() string {
	if len(c.allPRs()) > 0 {
		return cardPRHint
	}
	if len(c.Options) == 0 {
		return cardWriteHint
	}
	return cardWaitHint
}

func (c statusCard) progress(now time.Time) []slack.Block {
	var parts []string
	if p := c.Progress; p != nil {
		filled := p.Step * cardProgressCells / p.Total
		bar := fmt.Sprintf("`%s%s` *%d/%d*", strings.Repeat("▰", filled), strings.Repeat("▱", cardProgressCells-filled), p.Step, p.Total)
		if p.Label != "" {
			bar += " " + p.Label
		}
		parts = append(parts, bar)
	}
	if c.StartedAt != nil {
		parts = append(parts, "started "+clock(*c.StartedAt, now)+", running "+span(now.Sub(*c.StartedAt)))
	}
	if c.ETA != nil {
		parts = append(parts, "ETA "+clock(*c.ETA, now))
	}
	if len(parts) == 0 {
		return nil
	}
	return []slack.Block{mrkdwn("card-progress", strings.Join(parts, " · "))}
}

func (c statusCard) facts() []slack.Block {
	if len(c.Facts) == 0 {
		return nil
	}
	fields := make([]*slack.TextBlockObject, 0, len(c.Facts))
	for _, f := range c.Facts {
		fields = append(fields, slack.NewTextBlockObject(slack.MarkdownType, "*"+mrkdwnEscapes.Replace(f.Label)+"*\n"+mrkdwnLine(f.Value), false, false))
	}
	return []slack.Block{slack.NewSectionBlock(nil, fields, nil, slack.SectionBlockOptionBlockID("card-facts"))}
}

func refLine(target, label string, id refID, state refState, status string) string {
	line := "• " + slackLink(target, cmp.Or(label, id.defaultLabel()))
	if status != "" {
		line += " · " + mrkdwnEscapes.Replace(status)
	}
	if live := state.text(); live != "" && (status == "" || state.State != "closed") {
		line += " · " + live
	}
	return line
}

func (c statusCard) refs(v cardView) []slack.Block {
	if len(c.Refs) == 0 {
		return nil
	}
	lines := make([]string, 0, len(c.Refs))
	for _, r := range c.Refs {
		id, _ := parseRef(r.URL)
		lines = append(lines, refLine(r.URL, r.Label, id, v.refs[id.key()], ""))
	}
	return []slack.Block{section("card-refs", bulleted("*PRs and builds*", lines))}
}

func (c statusCard) rollup(v cardView) []slack.Block {
	r := c.Rollup
	if r == nil {
		return nil
	}
	var blocks []slack.Block
	if len(r.Lanes) > 0 {
		lines := make([]string, 0, len(r.Lanes))
		for _, l := range r.Lanes {
			line := "• *" + mrkdwnEscapes.Replace(l.Name) + "*"
			for _, part := range []string{l.Status, l.Detail} {
				if part != "" {
					line += " · " + mrkdwnEscapes.Replace(part)
				}
			}
			lines = append(lines, line)
		}
		blocks = append(blocks, section("card-lanes", bulleted(fmt.Sprintf("*Lanes* (%d)", len(r.Lanes)), lines)))
	}
	if len(r.PRs) > 0 {
		lines := make([]string, 0, len(r.PRs))
		for _, p := range r.PRs {
			id, ok := parseRef(p.URL)
			label := p.Label
			if label == "" && ok {
				label = id.defaultLabel()
			}
			lines = append(lines, refLine(p.URL, cmp.Or(label, p.URL), id, v.refs[id.key()], p.Status))
		}
		blocks = append(blocks, section("card-prs", bulleted(fmt.Sprintf("*PRs* (%d)", len(r.PRs)), lines)))
	}
	return blocks
}

func (k cardAsk) expired(now time.Time) bool { return k.ExpiresAt != nil && !now.Before(*k.ExpiresAt) }

func (k cardAsk) value() string {
	var exp int64
	if k.ExpiresAt != nil {
		exp = k.ExpiresAt.Unix()
	}
	return string(mustJSON(askTap{ID: k.ID, Label: k.Label, Expires: exp}))
}

func (k cardAsk) block(v cardView) slack.Block {
	id, label := cardAskBlock+k.ID, mrkdwnEscapes.Replace(k.Label)
	if ans, ok := v.answers[k.ID]; ok {
		return mrkdwn(id, "✓ *"+label+"* · answered "+clock(ans.At, v.now))
	}
	if k.expired(v.now) {
		return mrkdwn(id, "⌛ *"+label+"* · expired "+clock(*k.ExpiresAt, v.now))
	}
	text := "*" + label + "*"
	if k.Description != "" {
		text += "\n" + k.Description
	}
	if k.ExpiresAt != nil {
		text += "\n_expires " + clock(*k.ExpiresAt, v.now) + "_"
	}
	b := slack.NewButtonBlockElement(cardAskAction+k.ID, k.value(), slack.NewTextBlockObject(slack.PlainTextType, k.Label, true, false))
	b.Style = cardAskStyles[k.Style]
	if k.Confirm != "" {
		confirm := slack.NewConfirmationBlockObject(
			slack.NewTextBlockObject(slack.PlainTextType, k.Label, true, false),
			slack.NewTextBlockObject(slack.PlainTextType, k.Confirm, true, false),
			slack.NewTextBlockObject(slack.PlainTextType, "Yes", true, false),
			slack.NewTextBlockObject(slack.PlainTextType, "Cancel", true, false))
		if k.Style == "danger" {
			confirm.WithStyle(slack.StyleDanger)
		}
		b.Confirm = confirm
	}
	return slack.NewSectionBlock(slack.NewTextBlockObject(slack.MarkdownType, text, false, false), nil, slack.NewAccessory(b), slack.SectionBlockOptionBlockID(id))
}

func (r cardRow) blocks(v cardView) []slack.Block {
	blocks := make([]slack.Block, 0, len(r.Buttons))
	if r.PR != nil {
		blocks = r.PR.blocks(v, cardQuietBody)
	}
	text := r.Text
	ans, answered := v.answers[r.ID]
	if answered {
		text = strings.TrimSpace(text + "\n✓ *" + mrkdwnEscapes.Replace(ans.Label) + "* · answered " + clock(ans.At, v.now))
	}
	if text != "" {
		for _, part := range splitSection(text, cardMaxSectionLen) {
			blocks = append(blocks, slack.NewSectionBlock(slack.NewTextBlockObject(slack.MarkdownType, part, false, false), nil, nil))
		}
	}
	if answered {
		return blocks
	}
	buttons := make([]slack.BlockElement, 0, len(r.Buttons))
	for i, label := range r.Buttons {
		b := slack.NewButtonBlockElement(fmt.Sprintf("%s%d:%s", cardRowAction, i, r.ID), string(mustJSON(askTap{ID: r.ID, Label: label})),
			slack.NewTextBlockObject(slack.PlainTextType, label, true, false))
		if i == 0 {
			b.Style = slack.StylePrimary
		}
		buttons = append(buttons, b)
	}
	return append(blocks, slack.NewActionBlock(cardRowBlock+r.ID, buttons...))
}

func (p cardPR) blocks(v cardView, limit int) []slack.Block {
	id, _ := parseRef(p.URL)
	line := "*" + slackLink(p.URL, fmt.Sprintf("%s PR %d", id.Repo, id.Number)) + "*"
	if p.Title != "" {
		line += " " + plainMrkdwn(p.Title)
	}
	if p.Head != "" {
		line += " `" + p.Head[:min(len(p.Head), cardShortHead)] + "`"
	}
	if live := v.refs[id.key()].text(); live != "" && p.Review == "" {
		line += " · " + live
	}
	if p.Review != "" {
		line += "\n" + plainMrkdwn(p.Review)
	}
	facts := p.fields()
	fields := make([]*slack.TextBlockObject, 0, len(facts))
	blocks := []slack.Block{slack.NewSectionBlock(slack.NewTextBlockObject(slack.MarkdownType, line, false, false), nil, nil)}
	var long []string
	for _, f := range facts {
		text := "*" + f.Label + "*\n" + plainMrkdwn(f.Value)
		switch {
		case strings.TrimSpace(f.Value) == "":
		case runes(text) > cardMaxFieldLen:
			long = append(long, splitSection(text, cardMaxSectionLen)...)
		default:
			fields = append(fields, slack.NewTextBlockObject(slack.MarkdownType, text, false, false))
		}
	}
	if len(fields) > 0 {
		blocks = append(blocks, slack.NewSectionBlock(nil, fields, nil))
	}
	for _, text := range long {
		blocks = append(blocks, slack.NewSectionBlock(slack.NewTextBlockObject(slack.MarkdownType, text, false, false), nil, nil))
	}
	shown, _ := p.description(limit)
	if shown == "" {
		return blocks
	}
	return append(blocks, bodyPages(shown, cardMaxBlocks, cardMaxVisible, false)[0]...)
}

func (p cardPR) description(limit int) (string, string) {
	if runes(p.Description) <= limit {
		return p.Description, ""
	}
	shown := sectionCut([]rune(p.Description), limit)
	return strings.TrimSpace(shown), strings.TrimSpace(strings.TrimPrefix(p.Description, shown))
}

func (p cardPR) folded(limit int) [][]slack.Block {
	_, rest := p.description(limit)
	if rest == "" {
		return nil
	}
	id, _ := parseRef(p.URL)
	return bodyPages(fmt.Sprintf("*%s*\n\n%s", slackLink(p.URL, fmt.Sprintf("%s PR %d, the rest of its description", id.Repo, id.Number)), rest), cardMaxPageBlock, cardMaxVisible, true)
}

func plainMrkdwn(text string) string {
	return mrkdwnLine(cardWordEscapes.Replace(text))
}

func askBlocks(asks []cardAsk, v cardView) []slack.Block {
	blocks := make([]slack.Block, 0, min(len(asks), cardMaxAsks)+1)
	for i, k := range asks {
		if i == cardMaxAsks {
			blocks = append(blocks, mrkdwn("card-asks-more", fmt.Sprintf("+%d more open asks", len(asks)-i)))
			break
		}
		blocks = append(blocks, k.block(v))
	}
	return blocks
}

func timelineBlock(entries []timelineEntry, now time.Time) []slack.Block {
	if len(entries) == 0 {
		return nil
	}
	lines := make([]string, 0, cardMaxTimeline)
	size := 0
	for i := len(entries) - 1; i >= 0 && len(lines) < cardMaxTimeline; i-- {
		e := entries[i]
		line := clock(e.At, now) + " " + e.Emoji + " " + mrkdwnEscapes.Replace(e.Title)
		if size+runes(line)+1 > cardMaxSectionLen {
			break
		}
		lines, size = append(lines, line), size+runes(line)+1
	}
	return []slack.Block{mrkdwn("card-timeline", strings.Join(lines, "\n"))}
}

func (c statusCard) gainsRows(previous statusCard) bool {
	return slices.ContainsFunc(c.Rows, func(r cardRow) bool {
		return !slices.ContainsFunc(previous.Rows, func(o cardRow) bool { return o.ID == r.ID })
	})
}

func (c statusCard) replyBlocks(v cardView) []slack.Block {
	blocks, _ := c.layout(v)
	return blocks
}

func (c statusCard) layout(v cardView) ([]slack.Block, [][]slack.Block) {
	lead := []slack.Block{header(c.emoji() + " " + c.Title)}
	var fold [][]slack.Block
	for _, p := range c.PRs {
		lead = append(lead, p.blocks(v, cardQuietBody)...)
		fold = append(fold, p.folded(cardQuietBody)...)
	}
	for _, r := range c.Rows {
		if r.PR != nil {
			fold = append(fold, r.PR.folded(cardQuietBody)...)
		}
	}
	tail := c.tail(v)
	if c.Body != "" {
		body := bodyPages(c.Body, cardMaxBlocks-len(lead)-len(tail)-1, cardMaxVisible, true)
		lead = append(lead, body[0]...)
		fold = append(body[1:], fold...)
	}
	if len(fold) > 0 && !v.unfolded && c.Waits == "" && v.tapped == "" {
		tail = slices.Insert(tail, len(tail)-1, slack.Block(slack.NewActionBlock(cardFoldBlock, button(cardFoldAction, cardExplainLabel))))
	}
	return append(lead, tail...), fold
}

func (c statusCard) tail(v cardView) []slack.Block {
	blocks := make([]slack.Block, 0, len(c.Images)+len(c.Options)+len(c.Rows)+cardMaxFacts)
	for _, image := range c.Images {
		blocks = append(blocks, imageBlock(image))
	}
	blocks = append(blocks, c.progress(v.now)...)
	blocks = append(blocks, c.facts()...)
	blocks = append(blocks, c.refs(v)...)
	blocks = append(blocks, c.rollup(v)...)
	blocks = append(blocks, askBlocks(c.Asks, v)...)
	blocks = append(blocks, c.links()...)
	blocks = append(blocks, c.optionRows(v.tapped == "" && !v.final)...)
	if c.Group != "" {
		for _, r := range c.Rows {
			blocks = append(blocks, r.blocks(v)...)
		}
		return append(blocks, c.context(v.tag, v.at, ""))
	}
	if v.tapped != "" {
		return append(blocks, mrkdwn(cardTappedBlock+cardActionsBlock, v.tapped), c.context(v.tag, v.at, ""))
	}
	if v.final || c.refsOnly() {
		return append(blocks, c.context(v.tag, v.at, ""))
	}
	if c.Waits != "" {
		return append(blocks, mrkdwn(cardWaitBlock, cmp.Or(v.hint, c.waitHint())), c.waitActions(), c.context(v.tag, v.at, ""))
	}
	return append(blocks, c.actions(), c.context(v.tag, v.at, ""))
}

func (c statusCard) refsOnly() bool {
	return len(c.Refs) > 0 && c.Body == "" && c.Waits == "" && c.Progress == nil && c.Rollup == nil &&
		len(c.Options)+len(c.Asks)+len(c.Links)+len(c.Facts)+len(c.Images)+len(c.PRs) == 0
}

func (c statusCard) rootBlocks(v cardView) []slack.Block {
	name := c.Drive
	switch {
	case name != "":
	case v.tag.Repo != "":
		name = v.tag.Repo + "@" + v.tag.Branch
	default:
		name = "Claude session " + v.tag.Session
	}
	blocks := []slack.Block{header(c.emoji() + " " + name)}
	if c.Title != "" {
		latest := "*" + c.Title + "*"
		if c.Body != "" {
			latest += "\n" + c.Body
		}
		blocks = append(blocks, bodyPages(latest, cardMaxBlocks, cardMaxBodyLen, false)[0]...)
	}
	blocks = append(blocks, c.progress(v.now)...)
	blocks = append(blocks, c.facts()...)
	blocks = append(blocks, c.refs(v)...)
	blocks = append(blocks, c.rollup(v)...)
	blocks = append(blocks, askBlocks(v.asks, v)...)
	blocks = append(blocks, c.links()...)
	blocks = append(blocks, timelineBlock(v.timeline, v.now)...)
	return append(blocks, c.context(v.tag, v.at, "updated "))
}
