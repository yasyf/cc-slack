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

	replaces cardRef
	changed  string
}

type replacedCard struct {
	old, fresh cardRef
	changed    string
}

type skippedCard struct {
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	Reason string `json:"reason"`
}

func (s skippedCard) ref() refID {
	r, _ := parseRepo(s.Repo)
	r.Number = s.Number
	return r
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

func (a *app) attachTapPRs(hc daemon.HandlerCtx, sub subject.Subject, id identity, channelID, postThread, postTS string, b opBody, asked []string) error {
	if len(b.TapPRs) == 0 || len(id.Config.PRLinks.CardCommand) == 0 || !cardsAt(id, channelID) {
		return nil
	}
	ctx := context.WithoutCancel(hc.Ctx)
	prs := a.tapPRs(ctx, id, hc.Env.Scope, b)
	warning, undrawn := a.attachPRCards(ctx, hc.DB, sub, id, channelID, postThread, postTS, prs)
	notePostWarning(hc.Ctx, warning)
	link := id.permalink(channelID, postTS, postThread)
	var said []string
	for _, raw := range slices.Compact(slices.Sorted(slices.Values(b.TapPRs))) {
		pr, _ := parseRef(raw)
		why, missed := undrawn[pr.key()]
		if !slices.ContainsFunc(prs, func(p linkedPR) bool { return p.ref.key() == pr.key() }) {
			why, missed = "cc-slack could not read it from GitHub", true
		}
		if !missed {
			continue
		}
		a.poller.log.Printf("tap PR %s drew no card beside %s: %s", raw, link, why)
		if slices.Contains(asked, raw) {
			said = append(said, raw+": "+why)
		}
	}
	if len(said) == 0 {
		return nil
	}
	return fmt.Errorf("posted %s, so do not post it again, but drew no card for tap_prs %s", link, strings.Join(said, "; "))
}

func (a *app) attachPRCards(ctx context.Context, db *sql.DB, sub subject.Subject, id identity, channelID, postThread, postTS string, prs []linkedPR) (string, map[string]string) {
	for _, pr := range prs {
		if pr.state.terminal() {
			a.poller.log.Printf("tap PR %s draws no card: it is %s", prURL(pr.ref), pr.state.State)
		}
	}
	asked, standing, waiting := a.cards.toCard(channelID, postThread, prs)
	if len(asked)+len(standing) == 0 {
		return "", nil
	}
	beside := cardBeside{}
	switch {
	case channelID != id.DMChannelID:
	case postThread != postTS || !a.cards.live(channelID, postTS):
		beside.thread = postThread
	default:
		beside = cardBeside{thread: postTS, carry: true}
	}
	placed, undrawn := a.postPRCards(ctx, db, sub, id, channelID, prCardThread(id, channelID, postThread), beside, asked, standing)
	for _, pr := range a.cards.uncarded(placed, waiting) {
		if _, said := undrawn[pr.key()]; !said {
			undrawn[pr.key()] = "its card was not posted; the daemon log says why"
		}
	}
	if !beside.carry {
		return "", undrawn
	}
	return a.approvesCarried(ctx, id, cardRef{channelID, postTS, postTS}, placed, waiting), undrawn
}

func (l *liveCards) toCard(channelID, postThread string, prs []linkedPR) (asked []refID, standing []cardRef, waiting []refID) {
	l.mu.Lock()
	defer l.mu.Unlock()
	post := l.draft(channelID, postThread)
	for _, pr := range prs {
		keys := map[string]bool{pr.ref.key(): true}
		if pr.state.terminal() || post.shows(keys) {
			continue
		}
		waiting = append(waiting, pr.ref)
		open := func(t *liveThread, m *liveMessage) bool { return m.cites(keys) && t.open(m) }
		if card, found := l.rootPRCard(channelID, open); found {
			if !slices.Contains(standing, card) {
				standing = append(standing, card)
			}
			continue
		}
		asked = append(asked, pr.ref)
		l.refs[pr.ref.key()] = pr.state
	}
	return asked, standing, waiting
}

type cardBeside struct {
	thread string
	carry  bool
}

func (a *app) postPRCards(ctx context.Context, db *sql.DB, sub subject.Subject, id identity, channelID, thread string, beside cardBeside, asked []refID, standing []cardRef) ([]cardRef, map[string]string) {
	placed := standing
	undrawn := map[string]string{}
	if len(asked) > 0 {
		provided, skipped, err := askPRCards(ctx, id, asked)
		why := "the card command answered no card for it"
		if err != nil {
			a.poller.log.Printf("PR cards for %s/%s not asked for: %v", channelID, thread, err)
			why = err.Error()
		}
		for _, pr := range asked {
			undrawn[pr.key()] = why
		}
		for _, p := range provided {
			for _, pr := range p.Card.refIDs() {
				delete(undrawn, pr.key())
			}
		}
		for _, s := range skipped {
			a.poller.log.Printf("PR card for %s#%d not drawn: %s", s.Repo, s.Number, s.Reason)
			undrawn[s.ref().key()] = s.Reason
		}
		cards, shown, replaced := a.placePRCards(ctx, db, sub, id, channelID, thread, provided)
		a.tellShown(ctx, id, shown)
		for _, r := range replaced {
			a.markReplaced(ctx, id, r)
		}
		placed = append(placed, cards...)
	}
	if beside.thread == "" {
		return placed, undrawn
	}
	for _, card := range placed {
		if err := a.showPRCard(ctx, db, id, beside, card); err != nil {
			a.poller.log.Printf("PR card %s is not shown under %s: %v", card.ts, beside.thread, err)
		}
	}
	return placed, undrawn
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
		if changed := a.cards.changedSinceAnswer(posted, p.Card); changed != "" {
			p.replaces, p.changed = posted, changed
			rest = append(rest, p)
			continue
		}
		if err := a.pictureRedrawnPRCard(ctx, id, posted, &p.Card); err != nil {
			a.poller.log.Printf("PR card %s stays as drawn: %v", posted.ts, err)
			a.tellRefused(ctx, id, p, err)
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

func (a *app) placePRCards(ctx context.Context, db *sql.DB, sub subject.Subject, id identity, channelID, thread string, provided []providedCard) ([]cardRef, []map[string]any, []replacedCard) {
	provided, placed, shown := a.redrawPostedPRCards(ctx, id, channelID, thread, provided)
	var replaced []replacedCard
	cards := a.cards
	cards.mu.Lock()
	defer cards.mu.Unlock()
	now := a.now()
	for _, p := range provided {
		if offered, found := cards.pickOffering(channelID, p.Card.offer()); p.Card.Waits != "" && p.changed == "" && found {
			placed, shown = append(placed, offered), append(shown, shownCard(p))
			continue
		}
		if asking, found := cards.pickAsking(channelID, p.Card.offer().pr); p.Card.Waits == "" && found {
			placed, shown = append(placed, asking), append(shown, shownCard(p))
			continue
		}
		pictures, err := a.uploadPRCardPictures(ctx, id, &p.Card)
		if err != nil {
			a.poller.log.Printf("PR card for %s#%d refused: its pictures did not upload: %v", p.Repo, p.Number, err)
			a.tellRefused(ctx, id, p, err)
			continue
		}
		t := cards.draft(channelID, thread)
		if p.changed != "" {
			delete(t.Answers, p.Card.Waits)
		}
		m := &liveMessage{Card: p.Card, At: now, Final: p.Card.Waits == ""}
		text, blocks := t.replyMessage(m, now, cards.canvas())
		res, err := a.post(ctx, db, id, channelID, thread, text, blocks, nil, m.Card.entities(m.Card.prEntities(t.view(m, now, cards.canvas()))...)...)
		if err != nil {
			a.poller.log.Printf("PR card for %s#%d in %s/%s not posted: %v", p.Repo, p.Number, channelID, thread, err)
			return placed, shown, replaced
		}
		m.TS = res.TS
		t.Thread = cmp.Or(thread, res.TS)
		a.showPRCardPictures(ctx, db, id, t, m, pictures, now, cards.canvas())
		notePostWarning(ctx, a.linesCarried(ctx, id, cardRef{channelID, t.Thread, m.TS}, nil, &p.Card))
		t.PRCards = append(t.PRCards, m)
		t.Updated = now
		cards.commit(t)
		fresh := cardRef{channelID, t.Thread, m.TS}
		placed, shown = append(placed, fresh), append(shown, shownCard(p))
		if p.changed != "" {
			replaced = append(replaced, replacedCard{p.replaces, fresh, p.changed})
		}
		if thread != "" {
			continue
		}
		if err := a.bindSessionThread(ctx, db, sub, id, res.TS); err != nil {
			a.poller.log.Printf("PR card for %s#%d at %s: its thread goes unwatched: %v", p.Repo, p.Number, res.TS, err)
		}
	}
	return placed, shown, replaced
}

func (a *app) markReplaced(ctx context.Context, id identity, r replacedCard) {
	answered := ""
	a.cards.markReply(r.old.channel, r.old.thread, r.old.ts, func(m *liveMessage) { answered, m.ReplacedBy = m.Tapped, r.fresh.ts })
	line := "↪ *Replaced:* " + mrkdwnEscapes.Replace(clipRunes(r.changed, cardMaxReasonLen)) + " · " +
		slackLink(id.permalink(r.fresh.channel, r.fresh.ts, r.fresh.thread), "answer the new card") + "\nYour answer to this version: " + answered
	if err := a.poller.markOption(ctx, id, r.old, line); err != nil {
		a.poller.log.Printf("PR card %s, replaced by %s, still shows its answer as standing: %v", r.old.ts, r.fresh.ts, err)
	}
}

func (a *app) showPRCard(ctx context.Context, db *sql.DB, id identity, beside cardBeside, card cardRef) error {
	if beside.carry {
		if !a.cards.carry(card.channel, beside.thread, card.ts, a.now()) {
			return nil
		}
		_, err := a.cards.redraw(ctx, a.newAPI(id.Token), card.channel, beside.thread, beside.thread, a.now())
		return err
	}
	if beside.thread == card.thread {
		return nil
	}
	return a.copyCardInto(ctx, db, id, beside.thread, card)
}

func (a *app) copyCardInto(ctx context.Context, db *sql.DB, id identity, thread string, card cardRef) error {
	if a.cards.hasCopy(card.channel, thread, liveCardCopy{Card: card.ts}) {
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
	res, err := a.post(ctx, db, id, card.channel, thread, copyLead(blocks, answers), blocks, nil, a.cards.workObjectsOf(card.channel, card.ts, a.now())...)
	if err != nil {
		return err
	}
	a.cards.rememberCopy(card.channel, thread, liveCardCopy{Card: card.ts, TS: res.TS}, a.now())
	return nil
}

func prURLs(prs []refID) string {
	urls := make([]string, 0, len(prs))
	for _, pr := range prs {
		urls = append(urls, prURL(pr))
	}
	return strings.Join(urls, ", ")
}

func (a *app) approvesCarried(ctx context.Context, id identity, post cardRef, placed []cardRef, waiting []refID) string {
	link := id.permalink(post.channel, post.ts, post.thread)
	var held []slack.Block
	for _, ts := range append([]string{post.ts}, a.cards.continued(post.channel, post.thread)...) {
		message, err := messageAt(ctx, a.newAPI(id.Token), post.channel, post.thread, ts)
		if err != nil {
			return fmt.Sprintf("NO APPROVE READ: %s lists %s for his tap, and reading it back from Slack failed, so whether it carries Approve is unknown: %v", link, prURLs(waiting), err)
		}
		held = append(held, message.Blocks.BlockSet...)
	}
	var missing []string
	for _, card := range placed {
		if a.cards.awaitsTap(card.channel, card.ts) && !approvesOn(held, card.ts) {
			missing = append(missing, id.permalink(card.channel, card.ts, card.thread))
		}
	}
	if len(missing) > 0 {
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

func (p cardPR) lines() []cardFact {
	shown := p.visible()
	var lines []cardFact
	for _, f := range shown.fields(cardView{}) {
		if strings.TrimSpace(f.Value) != "" {
			lines = append(lines, cardFact{f.Label, "*" + f.Label + "*"})
		}
	}
	if strings.TrimSpace(shown.ShowMe) != "" {
		lines = append(lines, cardFact{"Show me", "*Show me*"})
	}
	return lines
}

func linesLacking(held []slack.Block, card statusCard) []string {
	want, labels := map[string]int{}, map[string]string{}
	for _, pr := range card.allPRs() {
		for _, line := range pr.lines() {
			want[line.Value]++
			labels[line.Value] = line.Label
		}
	}
	said := sectionText(held)
	var missing []string
	for marker, n := range want {
		if strings.Count(said, marker) < n {
			missing = append(missing, labels[marker])
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

func askPRCards(ctx context.Context, id identity, prs []refID) ([]providedCard, []skippedCard, error) {
	asked := make([]map[string]any, 0, len(prs))
	for _, pr := range prs {
		asked = append(asked, map[string]any{"repo": pr.Owner + "/" + pr.Repo, "number": pr.Number})
	}
	out, err := runCardCommand(ctx, id.Config.PRLinks.CardCommand, map[string]any{"prs": asked})
	if err != nil {
		return nil, nil, err
	}
	var answer struct {
		Cards   []providedCard `json:"cards"`
		Skipped []skippedCard  `json:"skipped"`
	}
	if err := json.Unmarshal(out, &answer); err != nil {
		return nil, nil, fmt.Errorf("decode %s: %w", id.Config.PRLinks.CardCommand[0], err)
	}
	return answer.Cards, answer.Skipped, nil
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
	provided, skipped, err := askPRCards(ctx, id, []refID{pr})
	if err != nil {
		return err
	}
	if len(skipped) > 0 {
		return fmt.Errorf("the card command drew no card for %s: %s", prURL(pr), skipped[0].Reason)
	}
	if len(provided) == 0 {
		return a.poller.markOption(ctx, id, card, cardNoLongerWaits)
	}
	if err := provided[0].Card.check(a.now(), false, id.Config.Links.Tailnet); err != nil {
		a.tellRefused(ctx, id, provided[0], err)
		return err
	}
	if changed := a.cards.changedSinceAnswer(card, provided[0].Card); changed != "" {
		return fmt.Errorf("%s changed since he answered it (%s), so the next post naming it in tap_prs draws its new card", prURL(pr), changed)
	}
	if err := a.pictureRedrawnPRCard(ctx, id, card, &provided[0].Card); err != nil {
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

func (a *app) uploadPRCardPictures(ctx context.Context, id identity, card *statusCard) ([]slack.FileSummary, error) {
	if err := hashPRCardPictures(card); err != nil || card.ImagesHash == "" {
		return nil, err
	}
	if bucket := id.Config.Pictures; bucket.enabled() {
		var err error
		card.Images, err = bucket.upload(ctx, id.TeamID, card.Pictures)
		return nil, err
	}
	return uploadFiles(ctx, a.newAPI(id.Token), card.Pictures)
}

func hashPRCardPictures(card *statusCard) error {
	images, err := allImages(card.Pictures)
	if err != nil {
		return err
	}
	if !images {
		return errors.New("a PR card's pictures are PNG, JPEG, GIF, or WebP images")
	}
	card.ImagesHash, err = hashFiles(card.Pictures)
	return err
}

func (a *app) pictureRedrawnPRCard(ctx context.Context, id identity, card cardRef, drawn *statusCard) error {
	if err := hashPRCardPictures(drawn); err != nil || drawn.ImagesHash == "" {
		return err
	}
	if shown := a.cards.prCardImages(card, drawn.ImagesHash); shown != nil {
		drawn.Images = shown
		return nil
	}
	if bucket := id.Config.Pictures; bucket.enabled() {
		var err error
		drawn.Images, err = bucket.upload(ctx, id.TeamID, drawn.Pictures)
		return err
	}
	files, err := uploadFiles(ctx, a.newAPI(id.Token), drawn.Pictures)
	if err != nil {
		return err
	}
	*drawn, _ = a.shareCardPictures(ctx, a.poller.plane.DB(), id, *drawn, files)
	return nil
}

func (a *app) showPRCardPictures(ctx context.Context, db *sql.DB, id identity, t *liveThread, m *liveMessage, pictures []slack.FileSummary, now time.Time, d canvas) {
	if len(pictures) == 0 {
		return
	}
	shown, shared := a.shareCardPictures(ctx, db, id, m.Card, pictures)
	if !shared {
		return
	}
	m.Card = shown
	text, blocks := t.replyMessage(m, now, d)
	if err := whileProcessing(ctx, func() error {
		return updateMessage(ctx, a.newAPI(id.Token), t.Channel, m.TS, text, blocks, m.Card.entities(m.Card.prEntities(t.view(m, now, d))...)...)
	}); err != nil {
		a.poller.log.Printf("PR card %s posted without its pictures: %v", m.TS, err)
	}
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
