package main

import (
	"bytes"
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/slack-go/slack"
	"github.com/yasyf/cc-interact/daemon"
	"github.com/yasyf/cc-interact/subject"
)

const (
	prLookupPatience = 5 * time.Second
	prCardPatience   = 2 * time.Minute
)

var (
	prUnscanned   = regexp.MustCompile("(?s)```.*?```|`[^`\n]+`|<[^<>\n]*>|\\[[^\\]\n]+\\]\\(https?://[^)\\s]+\\)|https?://[^\\s<>]+")
	prMentioned   = regexp.MustCompile(`(^|[^A-Za-z0-9_.&#/-])(?:([A-Za-z0-9-]{1,39})/([A-Za-z0-9._-]{1,100}))?#([0-9]{1,9})\b`)
	prURLTrailing = regexp.MustCompile(`[.,;:!?)\]'"]+$`)
	githubRemote  = regexp.MustCompile(`github\.com[:/]([A-Za-z0-9-]+)/([A-Za-z0-9._-]+?)(?:\.git)?/?$`)
)

type prMention struct {
	start, end int
	ref        refID
	url        string
	label      string
}

type linkedPR struct {
	ref   refID
	state refState
}

type providedCard struct {
	Repo   string     `json:"repo"`
	Number int        `json:"number"`
	Card   statusCard `json:"card"`
}

func prURL(r refID) string {
	return fmt.Sprintf("https://github.com/%s/%s/pull/%d", r.Owner, r.Repo, r.Number)
}

func repoRef(owner, name string) refID {
	return refID{Kind: refPR, Owner: strings.ToLower(owner), Repo: strings.ToLower(name)}
}

func parseRepo(raw string) (refID, bool) {
	owner, name, ok := strings.Cut(raw, "/")
	if !ok || !refNamePattern.MatchString(owner) || !refNamePattern.MatchString(name) {
		return refID{}, false
	}
	return repoRef(owner, name), true
}

func prMentions(text string) []prMention {
	var out []prMention
	at := 0
	scan := func(end int) {
		for _, m := range prMentioned.FindAllStringSubmatchIndex(text[at:end], -1) {
			n, _ := strconv.Atoi(text[at+m[8] : at+m[9]])
			mention := prMention{start: at + m[3], end: at + m[1], ref: refID{Kind: refPR, Number: n}}
			if m[4] >= 0 {
				mention.ref = repoRef(text[at+m[4]:at+m[5]], text[at+m[6]:at+m[7]])
				mention.ref.Number = n
			}
			out = append(out, mention)
		}
	}
	for _, skip := range prUnscanned.FindAllStringIndex(text, -1) {
		scan(skip[0])
		at = skip[1]
		raw := text[skip[0]:skip[1]]
		if md := mdLink.FindStringSubmatch(raw); md != nil && raw[0] == '[' {
			if id, ok := parseRef(md[2]); ok && id.Kind == refPR {
				ref := repoRef(id.Owner, id.Repo)
				ref.Number = id.Number
				out = append(out, prMention{start: skip[0], end: skip[1], ref: ref, url: md[2], label: md[1]})
			}
			continue
		}
		if !strings.HasPrefix(raw, "http") {
			continue
		}
		raw = prURLTrailing.ReplaceAllString(raw, "")
		at = skip[0] + len(raw)
		if id, ok := parseRef(raw); ok && id.Kind == refPR {
			ref := repoRef(id.Owner, id.Repo)
			ref.Number = id.Number
			out = append(out, prMention{start: skip[0], end: at, ref: ref, url: raw})
		}
	}
	scan(len(text))
	return out
}

func (m prMention) candidates(holders map[int][]refID) []refID {
	if m.ref.Owner != "" {
		return []refID{m.ref}
	}
	return holders[m.ref.Number]
}

func prRepos(ctx context.Context, scope string, listed []string) []refID {
	var repos []refID
	out, err := exec.CommandContext(ctx, "git", "-C", scope, "remote", "get-url", "origin").Output() //nolint:gosec // G204: argv to git, never a shell
	if m := githubRemote.FindStringSubmatch(strings.TrimSpace(string(out))); err == nil && m != nil {
		repos = append(repos, repoRef(m[1], m[2]))
	}
	for _, raw := range listed {
		if r, _ := parseRepo(raw); !slices.Contains(repos, r) {
			repos = append(repos, r)
		}
	}
	return repos
}

func (a *app) linkPRs(ctx context.Context, id identity, scope string, texts ...string) ([]string, []linkedPR) {
	if id.Config.GitHub == nil {
		return texts, nil
	}
	mentions := make([][]prMention, len(texts))
	var bare []int
	for i, text := range texts {
		mentions[i] = prMentions(text)
		for _, m := range mentions[i] {
			if m.ref.Owner == "" && !slices.Contains(bare, m.ref.Number) {
				bare = append(bare, m.ref.Number)
			}
		}
	}
	lookup, cancel := context.WithTimeout(ctx, prLookupPatience)
	defer cancel()
	holders := a.bareHolders(lookup, id, scope, bare)
	var asked []refID
	for _, m := range slices.Concat(mentions...) {
		for _, r := range m.candidates(holders) {
			if !slices.Contains(asked, r) {
				asked = append(asked, r)
			}
		}
	}
	if len(asked) == 0 {
		return texts, nil
	}
	found, err := a.cards.client.queryPRs(lookup, a.cards.host(refHostGitHub, id.Config).token, asked)
	if err != nil {
		a.poller.log.Printf("post goes out with its PR references unlinked: %v", err)
		return texts, nil
	}
	linked := make([]string, len(texts))
	var prs []linkedPR
	for i, text := range texts {
		var out strings.Builder
		at := 0
		for _, m := range mentions[i] {
			held := slices.DeleteFunc(slices.Clone(m.candidates(holders)), func(r refID) bool { _, ok := found[r.key()]; return !ok })
			if len(held) != 1 {
				continue
			}
			pr := held[0]
			out.WriteString(text[at:m.start])
			out.WriteString(slackLink(cmp.Or(m.url, prURL(pr)), cmp.Or(m.label, fmt.Sprintf("#%d", pr.Number))))
			at = m.end
			if !slices.ContainsFunc(prs, func(p linkedPR) bool { return p.ref == pr }) {
				prs = append(prs, linkedPR{ref: pr, state: found[pr.key()].state()})
			}
		}
		out.WriteString(text[at:])
		linked[i] = out.String()
	}
	return linked, prs
}

func (a *app) bareHolders(ctx context.Context, id identity, scope string, numbers []int) map[int][]refID {
	holders := map[int][]refID{}
	if len(numbers) == 0 {
		return holders
	}
	command := id.Config.PRLinks.CardCommand
	if len(command) == 0 {
		repos := prRepos(ctx, scope, id.Config.PRLinks.Repos)
		for _, n := range numbers {
			for _, r := range repos {
				r.Number = n
				holders[n] = append(holders[n], r)
			}
		}
		return holders
	}
	out, err := runCardCommand(ctx, command, map[string]any{"numbers": numbers})
	if err != nil {
		a.poller.log.Printf("bare PR numbers %v go out unlinked: %v", numbers, err)
		return holders
	}
	var answer struct {
		PRs []struct {
			Repo   string `json:"repo"`
			Number int    `json:"number"`
		} `json:"prs"`
	}
	if err := json.Unmarshal(out, &answer); err != nil {
		a.poller.log.Printf("bare PR numbers %v go out unlinked: decode %s: %v", numbers, command[0], err)
		return holders
	}
	for _, pr := range answer.PRs {
		r, ok := parseRepo(pr.Repo)
		if !ok {
			a.poller.log.Printf("%s named %q as the repo of #%d, not owner/name", command[0], pr.Repo, pr.Number)
			continue
		}
		r.Number = pr.Number
		holders[pr.Number] = append(holders[pr.Number], r)
	}
	return holders
}

func cardsAt(id identity, channelID string) bool {
	return strings.HasPrefix(channelID, "D") || slices.Contains(id.Config.PRLinks.CardChannels, channelID)
}

func prCardThread(id identity, channelID, postThread string) string {
	if channelID == id.DMChannelID {
		return ""
	}
	return postThread
}

func (a *app) tapPRs(ctx context.Context, id identity, scope string, b opBody) []linkedPR {
	_, prs := a.linkPRs(ctx, id, scope, strings.Join(b.TapPRs, "\n"))
	return prs
}

func (a *app) attachTapPRs(hc daemon.HandlerCtx, sub subject.Subject, id identity, channelID, postThread, postTS string, b opBody) string {
	if len(b.TapPRs) == 0 {
		return ""
	}
	ctx := context.WithoutCancel(hc.Ctx)
	return a.attachPRCards(ctx, hc.DB, sub, id, channelID, postThread, postTS, a.tapPRs(ctx, id, hc.Env.Scope, b))
}

func (a *app) attachPRCards(ctx context.Context, db *sql.DB, sub subject.Subject, id identity, channelID, postThread, postTS string, prs []linkedPR) string {
	command := id.Config.PRLinks.CardCommand
	if len(command) == 0 || !cardsAt(id, channelID) {
		return ""
	}
	asked, standing, waiting := a.cards.toCard(channelID, postThread, prs)
	if len(asked)+len(standing) == 0 {
		return ""
	}
	beside := cardBeside{}
	switch {
	case channelID != id.DMChannelID:
	case postThread != postTS || !a.cards.live(channelID, postTS):
		beside.thread = postThread
	default:
		beside = cardBeside{thread: postTS, carry: true}
	}
	placed := a.postPRCards(ctx, db, sub, id, channelID, prCardThread(id, channelID, postThread), beside, asked, standing)
	if !beside.carry {
		return ""
	}
	return a.approvesCarried(ctx, id, cardRef{channelID, postTS, postTS}, placed, waiting)
}

func (l *liveCards) toCard(channelID, postThread string, prs []linkedPR) (asked []map[string]any, standing []cardRef, waiting []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	post := l.draft(channelID, postThread)
	for _, pr := range prs {
		keys := map[string]bool{pr.ref.key(): true}
		if pr.state.terminal() || post.carded(keys) {
			continue
		}
		waiting = append(waiting, fmt.Sprintf("https://github.com/%s/%s/pull/%d", pr.ref.Owner, pr.ref.Repo, pr.ref.Number))
		open := func(t *liveThread, m *liveMessage) bool { return m.cites(keys) && t.open(m) }
		if card, found := l.rootPRCard(channelID, open); found {
			standing = append(standing, card)
			continue
		}
		asked = append(asked, map[string]any{"repo": pr.ref.Owner + "/" + pr.ref.Repo, "number": pr.ref.Number})
		l.refs[pr.ref.key()] = pr.state
	}
	return asked, standing, waiting
}

type cardBeside struct {
	thread string
	carry  bool
}

func (a *app) postPRCards(ctx context.Context, db *sql.DB, sub subject.Subject, id identity, channelID, thread string, beside cardBeside, asked []map[string]any, standing []cardRef) []cardRef {
	placed := standing
	if len(asked) > 0 {
		provided, err := askPRCards(ctx, id, asked)
		if err != nil {
			a.poller.log.Printf("PR cards for %s/%s not asked for: %v", channelID, thread, err)
		}
		cards, shown := a.placePRCards(ctx, db, sub, id, channelID, thread, provided)
		a.tellShown(ctx, id, shown)
		placed = append(placed, cards...)
	}
	if beside.thread == "" {
		return placed
	}
	for _, card := range placed {
		if err := a.showPRCard(ctx, db, id, beside, card); err != nil {
			a.poller.log.Printf("PR card %s is not shown under %s: %v", card.ts, beside.thread, err)
		}
	}
	return placed
}

func (a *app) redrawPostedPRCards(ctx context.Context, id identity, channelID, thread string, provided []providedCard) (rest []providedCard, placed []cardRef, shown []map[string]any) {
	now := a.now()
	for _, p := range provided {
		if err := p.Card.check(now, false, id.Config.Links.Tailnet); err != nil {
			a.poller.log.Printf("PR card for %s#%d refused: a card command answers with a card that passes the card checks: %v", p.Repo, p.Number, err)
			a.tellRefused(ctx, id, p, err)
			continue
		}
		posted, found := a.cards.posted(channelID, thread, p.Card)
		if !found {
			rest = append(rest, p)
			continue
		}
		if err := a.cards.replacePRCard(ctx, a.newAPI(id.Token), posted, p.Card, now); err != nil {
			a.poller.log.Printf("PR card %s stays as drawn: %v", posted.ts, err)
			a.tellRefused(ctx, id, p, err)
			continue
		}
		placed, shown = append(placed, posted), append(shown, shownCard(p))
	}
	return rest, placed, shown
}

func (a *app) placePRCards(ctx context.Context, db *sql.DB, sub subject.Subject, id identity, channelID, thread string, provided []providedCard) ([]cardRef, []map[string]any) {
	provided, placed, shown := a.redrawPostedPRCards(ctx, id, channelID, thread, provided)
	cards := a.cards
	cards.mu.Lock()
	defer cards.mu.Unlock()
	now := a.now()
	for _, p := range provided {
		if offered, found := cards.pickOffering(channelID, p.Card.offer()); p.Card.Waits != "" && found {
			placed, shown = append(placed, offered), append(shown, shownCard(p))
			continue
		}
		if asking, found := cards.pickAsking(channelID, p.Card.offer().pr); p.Card.Waits == "" && found {
			placed, shown = append(placed, asking), append(shown, shownCard(p))
			continue
		}
		t := cards.draft(channelID, thread)
		m := &liveMessage{Card: p.Card, At: now, Final: p.Card.Waits == ""}
		text, blocks := t.replyMessage(m, now, cards.refs)
		res, err := a.post(ctx, db, id, channelID, thread, text, blocks, nil, m.Card.entities(m.Card.prEntities(t.view(m, now, cards.refs))...)...)
		if err != nil {
			a.poller.log.Printf("PR card for %s#%d in %s/%s not posted: %v", p.Repo, p.Number, channelID, thread, err)
			return placed, shown
		}
		m.TS = res.TS
		t.Thread = cmp.Or(thread, res.TS)
		notePostWarning(ctx, a.linesCarried(ctx, id, cardRef{channelID, t.Thread, m.TS}, nil, &p.Card))
		t.PRCards = append(t.PRCards, m)
		t.Updated = now
		cards.commit(t)
		placed, shown = append(placed, cardRef{channelID, t.Thread, m.TS}), append(shown, shownCard(p))
		if thread != "" {
			continue
		}
		if err := a.bindSessionThread(ctx, db, sub, id, res.TS); err != nil {
			a.poller.log.Printf("PR card for %s#%d at %s: its thread goes unwatched: %v", p.Repo, p.Number, res.TS, err)
		}
	}
	return placed, shown
}

func (a *app) showPRCard(ctx context.Context, db *sql.DB, id identity, beside cardBeside, card cardRef) error {
	if beside.carry {
		if !a.cards.carry(card.channel, beside.thread, card.ts, a.now()) {
			return nil
		}
		_, err := a.cards.redraw(ctx, a.newAPI(id.Token), card.channel, beside.thread, beside.thread, a.now())
		return err
	}
	if a.cards.hasCopy(card.channel, beside.thread, liveCardCopy{Card: card.ts}) {
		return nil
	}
	link := id.permalink(card.channel, card.ts, card.thread)
	ref, err := parsePermalink(link)
	if err != nil {
		return err
	}
	blocks, answers, err := a.copyBlocks(ctx, id, ref, link)
	if err != nil {
		return err
	}
	res, err := a.post(ctx, db, id, card.channel, beside.thread, copyLead(blocks, answers), blocks, nil, a.cards.workObjectsOf(card.channel, card.ts, a.now())...)
	if err != nil {
		return err
	}
	a.cards.rememberCopy(card.channel, beside.thread, liveCardCopy{Card: card.ts, TS: res.TS}, a.now())
	return nil
}

func (a *app) approvesCarried(ctx context.Context, id identity, post cardRef, placed []cardRef, waiting []string) string {
	link := id.permalink(post.channel, post.ts, post.thread)
	var held []slack.Block
	for _, ts := range append([]string{post.ts}, a.cards.continued(post.channel, post.thread)...) {
		message, err := messageAt(ctx, a.newAPI(id.Token), post.channel, post.thread, ts)
		if err != nil {
			return fmt.Sprintf("NO APPROVE READ: %s lists %s for his tap, and reading it back from Slack failed, so whether it carries Approve is unknown: %v", link, strings.Join(waiting, ", "), err)
		}
		held = append(held, message.Blocks.BlockSet...)
	}
	var missing []string
	for _, card := range placed {
		if a.cards.awaitsTap(card.channel, card.ts) && !approvesOn(held, card.ts) {
			missing = append(missing, id.permalink(card.channel, card.ts, card.thread))
		}
	}
	switch {
	case len(placed) < len(waiting):
		return fmt.Sprintf("NO APPROVE: %s lists %s for his tap, and %d of them drew no card, so it carries no Approve for them; the daemon log says why", link, strings.Join(waiting, ", "), len(waiting)-len(placed))
	case len(missing) > 0:
		return fmt.Sprintf("NO APPROVE: %s, read back from Slack, carries no button to answer the card at %s", link, strings.Join(missing, ", "))
	}
	return ""
}

func (a *app) linesCarried(ctx context.Context, id identity, post cardRef, pages []string, card *statusCard) string {
	if card == nil || !slices.ContainsFunc(card.allPRs(), func(pr *cardPR) bool { return len(pr.lines()) > 0 }) {
		return ""
	}
	link := id.permalink(post.channel, post.ts, post.thread)
	var held []slack.Block
	for _, ts := range append([]string{post.ts}, pages...) {
		message, err := messageAt(ctx, a.newAPI(id.Token), post.channel, post.thread, ts)
		if err != nil {
			return fmt.Sprintf("NO LINES READ: reading %s back from Slack failed, so whether it shows each PR's lines is unknown: %v", link, err)
		}
		held = append(held, message.Blocks.BlockSet...)
	}
	if missing := linesLacking(held, *card); len(missing) > 0 {
		return fmt.Sprintf("NO LINES: %s, read back from Slack, lacks %s for a PR it carries", link, strings.Join(missing, ", "))
	}
	return ""
}

func (p cardPR) lines() []string {
	var labels []string
	for _, f := range p.visible().fields() {
		if strings.TrimSpace(f.Value) != "" {
			labels = append(labels, f.Label)
		}
	}
	return labels
}

func linesLacking(held []slack.Block, card statusCard) []string {
	want := map[string]int{}
	for _, pr := range card.allPRs() {
		for _, label := range pr.lines() {
			want[label]++
		}
	}
	said := sectionText(held)
	var missing []string
	for label, n := range want {
		if strings.Count(said, "*"+label+"*") < n {
			missing = append(missing, label)
		}
	}
	slices.Sort(missing)
	return missing
}

func sectionText(blocks []slack.Block) string {
	var text strings.Builder
	for _, block := range blocks {
		section, ok := block.(*slack.SectionBlock)
		if !ok {
			continue
		}
		if section.Text != nil {
			text.WriteString(section.Text.Text + "\n")
		}
		for _, field := range section.Fields {
			text.WriteString(field.Text + "\n")
		}
	}
	return text.String()
}

func approvesOn(blocks []slack.Block, card string) bool {
	answersCard := func(b *slack.ButtonBlockElement) bool { return strings.HasPrefix(b.ActionID, cardCopyAction+card+":") }
	return slices.ContainsFunc(blocks, func(block slack.Block) bool {
		switch b := block.(type) {
		case *slack.ActionBlock:
			return slices.ContainsFunc(b.Elements.ElementSet, func(element slack.BlockElement) bool {
				button, isButton := element.(*slack.ButtonBlockElement)
				return isButton && answersCard(button)
			})
		case *slack.SectionBlock:
			return b.Accessory != nil && b.Accessory.ButtonElement != nil && answersCard(b.Accessory.ButtonElement)
		}
		return false
	})
}

func (a *app) redrawCopies(ctx context.Context, id identity, card cardRef) error {
	copies := a.cards.copiesOf(card.channel, card.ts)
	if len(copies) == 0 {
		return nil
	}
	link := id.permalink(card.channel, card.ts, card.thread)
	ref, err := parsePermalink(link)
	if err != nil {
		return err
	}
	blocks, answers, err := a.copyBlocks(ctx, id, ref, link)
	if err != nil {
		return err
	}
	api := a.newAPI(id.Token)
	entities := a.cards.workObjectsOf(card.channel, card.ts, a.now())
	for _, copied := range copies {
		if err := updateMessage(ctx, api, copied.channel, copied.ts, copyLead(blocks, answers), blocks, entities...); err != nil {
			return err
		}
	}
	return nil
}

func shownCard(p providedCard) map[string]any {
	return map[string]any{"repo": p.Repo, "number": p.Number, "waits": p.Card.Waits}
}

func (a *app) tellShown(ctx context.Context, id identity, shown []map[string]any) {
	if len(shown) == 0 {
		return
	}
	if _, err := runCardCommand(ctx, id.Config.PRLinks.CardCommand, map[string]any{"shown": shown}); err != nil {
		a.poller.log.Printf("PR cards stand, but the card command did not record them, so their rows may draw again: %v", err)
	}
}

func (a *app) tellRefused(ctx context.Context, id identity, p providedCard, refusal error) {
	refused := map[string]any{"repo": p.Repo, "number": p.Number, "reason": refusal.Error()}
	if _, err := runCardCommand(ctx, id.Config.PRLinks.CardCommand, map[string]any{"refused": []map[string]any{refused}}); err != nil {
		a.poller.log.Printf("PR card for %s#%d was refused, and the card command did not record it, so its row will ask again: %v", p.Repo, p.Number, err)
	}
}

func askPRCards(ctx context.Context, id identity, asked []map[string]any) ([]providedCard, error) {
	out, err := runCardCommand(ctx, id.Config.PRLinks.CardCommand, map[string]any{"prs": asked})
	if err != nil {
		return nil, err
	}
	var answer struct {
		Cards []providedCard `json:"cards"`
	}
	if err := json.Unmarshal(out, &answer); err != nil {
		return nil, fmt.Errorf("decode %s: %w", id.Config.PRLinks.CardCommand[0], err)
	}
	return answer.Cards, nil
}

// recardPR redraws a PR card in place from what the card command says now, so
// it never offers a head or a question that no longer stands; a card for a PR
// that no longer waits keeps its answer, or gives up its buttons, under the
// row's current words.
func (a *app) recardPR(ctx context.Context, id identity, card cardRef, pr refID) {
	if err := a.redrawPRCard(ctx, id, card, pr); err != nil {
		a.poller.log.Printf("PR card %s stays as drawn: %v", card.ts, err)
	}
}

func (a *app) redrawPRCard(ctx context.Context, id identity, card cardRef, pr refID) error {
	provided, err := askPRCards(ctx, id, []map[string]any{{"repo": pr.Owner + "/" + pr.Repo, "number": pr.Number}})
	if err != nil {
		return err
	}
	if len(provided) == 0 {
		return a.poller.markOption(ctx, id, card, cardNoLongerWaits)
	}
	if err := provided[0].Card.check(a.now(), false, id.Config.Links.Tailnet); err != nil {
		a.tellRefused(ctx, id, provided[0], err)
		return err
	}
	if err := a.cards.replacePRCard(ctx, a.newAPI(id.Token), card, provided[0].Card, a.now()); err != nil {
		a.tellRefused(ctx, id, provided[0], err)
		return err
	}
	a.tellShown(ctx, id, []map[string]any{shownCard(provided[0])})
	return a.redrawCopies(ctx, id, card)
}

func (a *app) describeCardPRs(ctx context.Context, id identity, c *statusCard) {
	prs := slices.DeleteFunc(c.allPRs(), func(p *cardPR) bool { return p.Title != "" && p.Description != "" })
	if len(prs) == 0 || id.Config.GitHub == nil {
		return
	}
	refs := make([]refID, 0, len(prs))
	for _, p := range prs {
		r, _ := parseRef(p.URL)
		refs = append(refs, r)
	}
	lookup, cancel := context.WithTimeout(ctx, prLookupPatience)
	defer cancel()
	found, err := a.cards.client.describePRs(lookup, a.cards.host(refHostGitHub, id.Config).token, refs)
	if err != nil {
		a.poller.log.Printf("card %q goes out without its PR descriptions: %v", c.Title, err)
		return
	}
	for i, p := range prs {
		if pr, ok := found[refs[i].key()]; ok {
			p.Title, p.Description = cmp.Or(p.Title, pr.Title), cmp.Or(p.Description, prDescription(pr.Body))
		}
	}
}

type cardCommandError struct {
	command string
	err     error
	said    string
}

func (e cardCommandError) Error() string { return e.command + ": " + e.err.Error() + ": " + e.said }

func (e cardCommandError) Unwrap() error { return e.err }

func runCardCommand(ctx context.Context, command []string, ask any) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, prCardPatience)
	defer cancel()
	cmd := exec.CommandContext(ctx, installed(command[0]), command[1:]...) //nolint:gosec // G204: the configured card command's argv, never a shell
	cmd.Stdin = bytes.NewReader(mustJSON(ask))
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if exit, ok := errors.AsType[*exec.ExitError](err); ok {
		return nil, cardCommandError{command[0], err, string(bytes.TrimSpace(exit.Stderr))}
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", command[0], err)
	}
	return out, nil
}
