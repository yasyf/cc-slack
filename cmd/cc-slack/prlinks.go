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
	"sync"
	"time"

	"github.com/slack-go/slack"
	"github.com/yasyf/cc-interact/daemon"
	"github.com/yasyf/cc-interact/subject"
)

const prLookupPatience = 5 * time.Second

var prCardPatience = 2 * time.Minute

var errCardCommandUnfinished = errors.New("the card command did not finish")

var shownRecords sync.RWMutex

var (
	prUnscanned   = regexp.MustCompile("(?s)" + slackCode + "|<[^<>\n]*>|\\[[^\\]\n]+\\]\\(https?://[^)\\s]+\\)|https?://[^\\s<>]+")
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

	replaces []replacedCard
}

type replacedCard struct {
	old, fresh cardRef
	why        string
}

func regrouped(card statusCard) string {
	if n := len(card.prKeys()); n > 1 {
		return fmt.Sprintf("now one card for its stack of %d PRs", n)
	}
	return "now a card of its own"
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
	found, err := a.cards.client.queryPRs(lookup, asked)
	if err != nil {
		a.poller.log.Printf("post links the PR references GitHub answered before, and each other reference naming one PR with no GitHub state: %v", err)
	}
	linked := make([]string, len(texts))
	var prs []linkedPR
	for i, text := range texts {
		var out strings.Builder
		at := 0
		for _, m := range mentions[i] {
			held := slices.DeleteFunc(slices.Clone(m.candidates(holders)), func(r refID) bool { _, ok := found[r.key()]; return !ok })
			if len(held) == 0 && err != nil {
				held = m.candidates(holders)
			}
			if len(held) != 1 {
				continue
			}
			pr := held[0]
			out.WriteString(text[at:m.start])
			out.WriteString(slackLink(cmp.Or(m.url, prURL(pr)), cmp.Or(m.label, fmt.Sprintf("#%d", pr.Number))))
			at = m.end
			if slices.ContainsFunc(prs, func(p linkedPR) bool { return p.ref == pr }) {
				continue
			}
			var state refState
			if read, ok := found[pr.key()]; ok {
				state = read.state()
			}
			prs = append(prs, linkedPR{ref: pr, state: state})
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

func tapCardsRefused(id identity, channelID string) string {
	switch {
	case len(id.Config.PRLinks.CardCommand) == 0:
		return "this machine's cc-slack login has no [pr_links] card_command, so it draws no PR card: the config it last read sets none, or its program is not on the daemon's PATH; run `cc-slack reload --config <the config that sets it>`"
	case !cardsAt(id, channelID):
		return fmt.Sprintf("PR cards draw only in a DM or a [pr_links] card_channels channel, and %s is neither", channelID)
	}
	return ""
}

func prCardThread(id identity, channelID, postThread string) string {
	if channelID == id.DMChannelID {
		return ""
	}
	return postThread
}

func (a *app) tapPRs(ctx context.Context, id identity, scope string, b opBody) []linkedPR {
	_, prs := a.linkPRs(ctx, id, scope, strings.Join(b.TapPRs, "\n"))
	for _, raw := range b.TapPRs {
		if pr, _ := parseRef(raw); !slices.ContainsFunc(prs, func(p linkedPR) bool { return p.ref.key() == pr.key() }) {
			prs = append(prs, linkedPR{ref: pr})
		}
	}
	return prs
}

type tapCards struct {
	ask, waiting []refID
	standing     []cardRef
	provided     []providedCard
	undrawn      map[string]string
}

func (a *app) askTapCards(ctx context.Context, id identity, scope, channelID, thread string, b opBody, asked []string) (tapCards, error) {
	if len(b.TapPRs) == 0 {
		return tapCards{}, nil
	}
	if why := tapCardsRefused(id, channelID); why != "" {
		if len(asked) == 0 {
			return tapCards{}, nil
		}
		a.poller.log.Printf("tap PRs %s draw no card: %s", strings.Join(asked, ", "), why)
		return tapCards{}, fmt.Errorf("drew no card for tap_prs %s: %s", strings.Join(asked, ", "), why)
	}
	prs := a.tapPRs(ctx, id, scope, b)
	for _, pr := range prs {
		if pr.state.terminal() {
			a.poller.log.Printf("tap PR %s draws no card: it is %s", prURL(pr.ref), pr.state.State)
		}
	}
	t := tapCards{undrawn: map[string]string{}}
	t.ask, t.standing, t.waiting = a.cards.toCard(channelID, thread, b.Card, prs)
	if len(t.ask) > 0 {
		a.askCardCommand(ctx, id, channelID, thread, &t)
	}
	var said []string
	for _, raw := range slices.Compact(slices.Sorted(slices.Values(b.TapPRs))) {
		pr, _ := parseRef(raw)
		why, missed := t.undrawn[pr.key()]
		if !missed {
			continue
		}
		a.poller.log.Printf("tap PR %s draws no card: %s", raw, why)
		if slices.Contains(asked, raw) {
			said = append(said, raw+": "+why)
		}
	}
	if len(said) > 0 {
		return tapCards{}, fmt.Errorf("drew no card for tap_prs %s", strings.Join(said, "; "))
	}
	return t, nil
}

func (a *app) askCardCommand(ctx context.Context, id identity, channelID, thread string, t *tapCards) {
	provided, skipped, err := askPRCards(ctx, id, t.ask, false)
	why := "the card command answered no card for it"
	if err != nil {
		a.poller.log.Printf("PR cards for %s/%s not asked for: %v", channelID, thread, err)
		why = err.Error()
	}
	for _, pr := range t.ask {
		t.undrawn[pr.key()] = why
	}
	now := a.now()
	for _, p := range provided {
		if err := p.Card.check(now, false, id.Config.Links.Tailnet); err != nil {
			a.poller.log.Printf("PR card for %s#%d refused: a card command answers with a card that passes the card checks: %v", p.Repo, p.Number, err)
			a.tellRefused(ctx, id, p, err)
			for _, pr := range p.Card.refIDs() {
				t.undrawn[pr.key()] = "the card command's card fails the card checks: " + err.Error()
			}
			continue
		}
		for _, pr := range p.Card.refIDs() {
			delete(t.undrawn, pr.key())
		}
		t.provided = append(t.provided, p)
	}
	for _, s := range skipped {
		a.poller.log.Printf("PR card for %s#%d not drawn: %s", s.Repo, s.Number, s.Reason)
		t.undrawn[s.ref().key()] = s.Reason
	}
	uncovered := a.cards.uncarded(slices.DeleteFunc(slices.Clone(t.standing), a.cards.retired), t.ask)
	for _, pr := range t.ask {
		if !slices.ContainsFunc(uncovered, func(u refID) bool { return u.key() == pr.key() }) {
			delete(t.undrawn, pr.key())
		}
	}
}

type tapDrawn struct {
	cards []cardRef
}

func (d tapDrawn) named(id identity) (stamps, links string) {
	permalinks := make([]string, 0, len(d.cards))
	for _, card := range d.cards {
		permalinks = append(permalinks, id.permalink(card.channel, card.ts, card.thread))
	}
	return strings.Join(cardStamps(d.cards), " "), strings.Join(permalinks, " ")
}

func (a *app) attachTapPRs(hc daemon.HandlerCtx, sub subject.Subject, id identity, channelID, postThread, postTS string, t tapCards, asked []string) tapDrawn {
	if len(t.ask)+len(t.standing) == 0 {
		return tapDrawn{}
	}
	ctx := context.WithoutCancel(hc.Ctx)
	warning, placed, undrawn := a.attachPRCards(ctx, hc.DB, sub, id, channelID, postThread, postTS, t)
	notePostWarning(hc.Ctx, warning)
	link := id.permalink(channelID, postTS, postThread)
	var said []string
	for _, raw := range slices.Compact(slices.Sorted(slices.Values(asked))) {
		pr, _ := parseRef(raw)
		why, missed := undrawn[pr.key()]
		if !missed {
			continue
		}
		a.poller.log.Printf("tap PR %s drew no card beside %s: %s", raw, link, why)
		said = append(said, raw+": "+why)
	}
	if len(said) > 0 {
		notePostWarning(hc.Ctx, fmt.Sprintf("posted %s, so do not post it again, but drew no card for tap_prs %s", link, strings.Join(said, "; ")))
	}
	return tapDrawn{cards: placed}
}

func cardStamps(cards []cardRef) []string {
	stamps := make([]string, 0, len(cards))
	for _, card := range cards {
		stamps = append(stamps, card.ts)
	}
	return stamps
}

func (a *app) attachPRCards(ctx context.Context, db *sql.DB, sub subject.Subject, id identity, channelID, postThread, postTS string, t tapCards) (string, []cardRef, map[string]string) {
	beside := cardBeside{}
	switch {
	case channelID != id.DMChannelID:
	case postThread != postTS || !a.cards.live(channelID, postTS):
		beside.thread = postThread
	default:
		beside = cardBeside{thread: postTS, carry: true}
	}
	placed := a.postPRCards(ctx, db, sub, id, channelID, prCardThread(id, channelID, postThread), beside, t)
	undrawn := map[string]string{}
	for _, pr := range a.cards.uncarded(placed, t.waiting) {
		undrawn[pr.key()] = cmp.Or(t.undrawn[pr.key()], "its card was not posted; the daemon log says why")
	}
	if !beside.carry {
		return "", placed, undrawn
	}
	return a.approvesCarried(ctx, id, cardRef{channelID, postTS, postTS}, placed, t.waiting), placed, undrawn
}

func (l *liveCards) toCard(channelID, postThread string, posting *statusCard, prs []linkedPR) (asked []refID, standing []cardRef, waiting []refID) {
	l.mu.Lock()
	defer l.mu.Unlock()
	post := l.draft(channelID, postThread)
	if posting != nil {
		post.Reply = &liveMessage{Card: *posting}
	}
	named := slices.DeleteFunc(slices.Clone(prs), func(pr linkedPR) bool { return pr.state.terminal() })
	if !slices.ContainsFunc(named, func(pr linkedPR) bool { return !post.shows(map[string]bool{pr.ref.key(): true}) }) {
		return nil, nil, nil
	}
	var stood []cardRef
	for _, pr := range named {
		keys := map[string]bool{pr.ref.key(): true}
		card, _ := l.rootPRCard(channelID, func(t *liveThread, m *liveMessage) bool { return m.cites(keys) && t.asks(m) })
		waiting, stood = append(waiting, pr.ref), append(stood, card)
		if card != (cardRef{}) && !slices.Contains(standing, card) {
			standing = append(standing, card)
		}
	}
	if len(standing) == 1 && !slices.Contains(stood, cardRef{}) {
		return nil, standing, waiting
	}
	for i, pr := range named {
		if stood[i] == (cardRef{}) && pr.state != (refState{}) {
			l.refs[pr.ref.key()] = pr.state
		}
	}
	return waiting, standing, waiting
}

type cardBeside struct {
	thread string
	carry  bool
}

func (a *app) postPRCards(ctx context.Context, db *sql.DB, sub subject.Subject, id identity, channelID, thread string, beside cardBeside, t tapCards) []cardRef {
	var drawn []cardRef
	if len(t.provided) > 0 {
		cards, shown, replaced := a.placePRCards(ctx, db, sub, id, channelID, thread, t.provided)
		a.tellShown(ctx, id, shown)
		for _, r := range replaced {
			a.markReplaced(ctx, id, r)
		}
		drawn = cards
	}
	placed := slices.DeleteFunc(slices.Clone(t.standing), a.cards.retired)
	for _, card := range drawn {
		if !slices.Contains(placed, card) {
			placed = append(placed, card)
		}
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

func (a *app) redrawPostedPRCards(ctx context.Context, id identity, channelID, thread string, provided []providedCard) (rest []providedCard, placed []cardRef, shown []map[string]any, replaced []replacedCard) {
	now := a.now()
	for _, p := range provided {
		posted, found, others := a.cards.posted(channelID, thread, p.Card)
		for _, old := range others {
			p.replaces = append(p.replaces, replacedCard{old: old, why: regrouped(p.Card)})
		}
		if !found {
			rest = append(rest, p)
			continue
		}
		if changed := a.cards.changedSinceAnswer(posted, p.Card); changed != "" {
			p.replaces = append(p.replaces, replacedCard{old: posted, why: changed})
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
		if err := a.cards.redrawCopies(ctx, id, posted); err != nil {
			a.poller.log.Printf("PR card %s is redrawn, but its copies still show it as it was: %v", posted.ts, err)
		}
		placed, shown, replaced = append(placed, posted), append(shown, shownCard(p)), append(replaced, p.replacedBy(posted)...)
	}
	return rest, placed, shown, replaced
}

func (p providedCard) replacedBy(card cardRef) []replacedCard {
	replaced := slices.Clone(p.replaces)
	for i := range replaced {
		replaced[i].fresh = card
	}
	return replaced
}

func (a *app) placePRCards(ctx context.Context, db *sql.DB, sub subject.Subject, id identity, channelID, thread string, provided []providedCard) ([]cardRef, []map[string]any, []replacedCard) {
	provided, placed, shown, replaced := a.redrawPostedPRCards(ctx, id, channelID, thread, provided)
	cards := a.cards
	cards.mu.Lock()
	defer cards.mu.Unlock()
	now := a.now()
	for _, p := range provided {
		if offered, found := cards.pickOffering(channelID, p.Card.offer()); p.Card.Waits != "" && len(p.replaces) == 0 && found {
			placed, shown = append(placed, offered), append(shown, shownCard(p))
			continue
		}
		if asking, found := cards.pickAsking(channelID, p.Card.offer().pr); p.Card.Waits == "" && len(p.replaces) == 0 && found {
			placed, shown = append(placed, asking), append(shown, shownCard(p))
			continue
		}
		if err := uploadPRCardPictures(ctx, id, &p.Card); err != nil {
			a.poller.log.Printf("PR card for %s#%d refused: its pictures did not upload: %v", p.Repo, p.Number, err)
			a.tellRefused(ctx, id, p, err)
			continue
		}
		t := cards.draft(channelID, thread)
		if len(p.replaces) > 0 {
			delete(t.Answers, p.Card.Waits)
		}
		m := &liveMessage{Card: p.Card, At: now, Final: p.Card.Waits == ""}
		text, blocks := t.replyMessage(m, now, cards.canvas())
		res, err := a.post(atStep(ctx, fmt.Sprintf("tap:%s#%d:%s", p.Repo, p.Number, p.Card.Waits)), db, id, channelID, thread, text, blocks, nil, m.Card.entities(m.Card.prEntities(t.view(m, now, cards.canvas()))...)...)
		if err != nil {
			a.poller.log.Printf("PR card for %s#%d in %s/%s not posted: %v", p.Repo, p.Number, channelID, thread, err)
			return placed, shown, replaced
		}
		m.TS = res.TS
		t.Thread = cmp.Or(thread, res.TS)
		notePostWarning(ctx, a.linesCarried(ctx, id, cardRef{channelID, t.Thread, m.TS}, nil, &p.Card))
		t.PRCards = append(t.PRCards, m)
		t.Updated = now
		cards.commit(t)
		fresh := cardRef{channelID, t.Thread, m.TS}
		placed, shown, replaced = append(placed, fresh), append(shown, shownCard(p)), append(replaced, p.replacedBy(fresh)...)
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
	answered, retired := "", false
	a.cards.markReply(r.old.channel, r.old.thread, r.old.ts, func(m *liveMessage) {
		if m.ReplacedBy == "" {
			answered, m.ReplacedBy, retired = m.Tapped, r.fresh.ts, true
		}
	})
	if !retired {
		return
	}
	line := "↪ *Replaced:* " + mrkdwnEscapes.Replace(clipRunes(r.why, cardMaxReasonLen)) + " · " +
		slackLink(id.permalink(r.fresh.channel, r.fresh.ts, r.fresh.thread), "answer the new card")
	if answered != "" && answered != cardNoLongerWaits {
		line += "\nYour answer to this version: " + answered
	}
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
	return a.linkCardInto(ctx, db, id, beside.thread, card)
}

func (a *app) linkCardInto(ctx context.Context, db *sql.DB, id identity, thread string, card cardRef) error {
	if a.cards.showsIn(card.channel, thread, card.ts) {
		return nil
	}
	return a.postCardLink(ctx, db, id, thread, card)
}

func (a *app) postCardLink(ctx context.Context, db *sql.DB, id identity, under string, card cardRef) error {
	res, err := a.post(atStep(ctx, "link:"+card.ts+":"+under), db, id, card.channel, under, slackLink(id.permalink(card.channel, card.ts, card.thread), a.cards.titleOf(card.channel, card.ts)), nil, nil)
	if err != nil {
		return err
	}
	a.cards.rememberCopy(card.channel, cmp.Or(under, res.TS), liveCardCopy{Card: card.ts, TS: res.TS, Link: true}, a.now())
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

func shownCard(p providedCard) map[string]any {
	return map[string]any{"repo": p.Repo, "number": p.Number, "waits": p.Card.Waits}
}

func (a *app) tellShown(ctx context.Context, id identity, shown []map[string]any) {
	if len(shown) == 0 {
		return
	}
	shownRecords.Lock()
	go func() {
		defer shownRecords.Unlock()
		if _, _, err := cardCommandRan(context.WithoutCancel(ctx), id.Config.PRLinks.CardCommand, map[string]any{"shown": shown}); err != nil {
			a.poller.log.Printf("PR cards stand, but the card command did not record them, so their rows may draw again: %v", err)
		}
	}()
}

func (a *app) tellRefused(ctx context.Context, id identity, p providedCard, refusal error) {
	refused := map[string]any{"repo": p.Repo, "number": p.Number, "reason": refusal.Error()}
	if _, err := runCardCommand(ctx, id.Config.PRLinks.CardCommand, map[string]any{"refused": []map[string]any{refused}}); err != nil {
		a.poller.log.Printf("PR card for %s#%d was refused, and the card command did not record it, so its row will ask again: %v", p.Repo, p.Number, err)
	}
}

func askPRCards(ctx context.Context, id identity, prs []refID, posted bool) ([]providedCard, []skippedCard, error) {
	asked := make([]map[string]any, 0, len(prs))
	for _, pr := range prs {
		asked = append(asked, map[string]any{"repo": pr.Owner + "/" + pr.Repo, "number": pr.Number})
	}
	ask := map[string]any{"prs": asked}
	if posted {
		ask["redraw"] = true
	}
	out, said, err := cardCommandOutput(ctx, id.Config.PRLinks.CardCommand, ask)
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
	return answer.Cards, append(answer.Skipped, unanswered(prs, answer.Cards, answer.Skipped, said)...), nil
}

func unanswered(prs []refID, cards []providedCard, skipped []skippedCard, said string) []skippedCard {
	var told []skippedCard
	for _, pr := range prs {
		named := func(r refID) bool { return r.key() == pr.key() }
		if slices.ContainsFunc(skipped, func(s skippedCard) bool { return named(s.ref()) }) ||
			slices.ContainsFunc(cards, func(p providedCard) bool { return slices.ContainsFunc(p.Card.refIDs(), named) }) {
			continue
		}
		key := fmt.Sprintf("%s/%s#%d", pr.Owner, pr.Repo, pr.Number)
		for line := range strings.Lines(said) {
			if strings.Contains(line, key) {
				told = append(told, skippedCard{Repo: pr.Owner + "/" + pr.Repo, Number: pr.Number, Reason: strings.TrimSpace(line)})
				break
			}
		}
	}
	return told
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
	provided, skipped, err := askPRCards(ctx, id, []refID{pr}, true)
	if err != nil {
		return err
	}
	if len(skipped) > 0 {
		return fmt.Errorf("the card command drew no card for %s: %s", prURL(pr), skipped[0].Reason)
	}
	if len(provided) == 0 {
		if a.cards.retired(card) {
			return nil
		}
		return a.poller.markOption(ctx, id, card, cardNoLongerWaits)
	}
	if a.cards.staysRetired(card, provided[0].Card) {
		return nil
	}
	if err := provided[0].Card.check(a.now(), false, id.Config.Links.Tailnet); err != nil {
		a.tellRefused(ctx, id, provided[0], err)
		return err
	}
	if changed := a.cards.changedSinceAnswer(card, provided[0].Card); changed != "" {
		return fmt.Errorf("%s changed since he answered or it withdrew its card (%s), so the next post naming it in tap_prs draws its new card", prURL(pr), changed)
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
	return a.cards.redrawCopies(ctx, id, card)
}

func uploadPRCardPictures(ctx context.Context, id identity, card *statusCard) error {
	if err := hashPRCardPictures(card); err != nil || card.ImagesHash == "" {
		return err
	}
	var err error
	card.Images, err = uploadPictures(ctx, id, card.Pictures)
	return err
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
	var err error
	drawn.Images, err = uploadPictures(ctx, id, drawn.Pictures)
	return err
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
	found, err := a.cards.client.describePRs(lookup, refs)
	if err != nil {
		a.poller.log.Printf("card %q describes only the PRs GitHub answered before: %v", c.Title, err)
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
	out, _, err := cardCommandOutput(ctx, command, ask)
	return out, err
}

func cardCommandOutput(ctx context.Context, command []string, ask any) ([]byte, string, error) {
	shownRecords.RLock()
	defer shownRecords.RUnlock()
	return cardCommandRan(ctx, command, ask)
}

func cardCommandRan(ctx context.Context, command []string, ask any) ([]byte, string, error) {
	ctx, cancel := context.WithTimeout(ctx, prCardPatience)
	defer cancel()
	cmd := exec.CommandContext(ctx, installed(command[0]), command[1:]...) //nolint:gosec // G204: the configured card command's argv, never a shell
	cmd.Stdin = bytes.NewReader(mustJSON(ask))
	cmd.WaitDelay = time.Second
	var said bytes.Buffer
	cmd.Stderr = &said
	began := time.Now()
	out, err := cmd.Output()
	traced(ctx, strings.Join(command[:min(len(command), 3)], " "), time.Since(began))
	if err != nil && ctx.Err() != nil {
		return nil, "", fmt.Errorf("%s: %w in %s: %w", command[0], errCardCommandUnfinished, prCardPatience, ctx.Err())
	}
	if _, ok := errors.AsType[*exec.ExitError](err); ok {
		return nil, "", cardCommandError{command[0], err, string(bytes.TrimSpace(said.Bytes()))}
	}
	if err != nil {
		return nil, "", fmt.Errorf("%s: %w", command[0], err)
	}
	return out, said.String(), nil
}
