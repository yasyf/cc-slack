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
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

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

func (a *app) attachPRCards(ctx context.Context, db *sql.DB, sub subject.Subject, id identity, channelID, postThread string, prs []linkedPR) {
	command := id.Config.PRLinks.CardCommand
	if len(command) == 0 || !cardsAt(id, channelID) {
		return
	}
	cards := a.cards
	cards.mu.Lock()
	defer cards.mu.Unlock()
	post := cards.draft(channelID, postThread)
	var asked []map[string]any
	for _, pr := range prs {
		keys := map[string]bool{pr.ref.key(): true}
		open := func(t *liveThread, m *liveMessage) bool { return m.cites(keys) && t.open(m) }
		if pr.state.terminal() || post.carded(keys) || cards.rootPRCard(channelID, open) {
			continue
		}
		asked = append(asked, map[string]any{"repo": pr.ref.Owner + "/" + pr.ref.Repo, "number": pr.ref.Number})
		cards.refs[pr.ref.key()] = pr.state
	}
	if len(asked) == 0 {
		return
	}
	go a.postPRCards(context.WithoutCancel(ctx), db, sub, id, channelID, prCardThread(id, channelID, postThread), asked)
}

func (a *app) postPRCards(ctx context.Context, db *sql.DB, sub subject.Subject, id identity, channelID, thread string, asked []map[string]any) {
	provided, err := askPRCards(ctx, id, asked)
	if err != nil {
		a.poller.log.Printf("PR cards for %s/%s not asked for: %v", channelID, thread, err)
		return
	}
	cards := a.cards
	cards.mu.Lock()
	defer cards.mu.Unlock()
	now := a.now()
	for _, p := range provided {
		if err := p.Card.check(now, false, id.Config.Links.Tailnet); err != nil || p.Card.Waits == "" {
			a.poller.log.Printf("PR card for %s#%d refused: a card command answers with a waiting card that passes the card checks: %v", p.Repo, p.Number, err)
			continue
		}
		sameAsk := func(m *liveMessage) bool { return m.Card.Waits == p.Card.Waits }
		t := cards.draft(channelID, thread)
		if slices.ContainsFunc(t.PRCards, sameAsk) || cards.rootPRCard(channelID, func(_ *liveThread, m *liveMessage) bool { return sameAsk(m) }) || cards.pickOffers(channelID, p.Card.offer()) {
			continue
		}
		m := &liveMessage{Card: p.Card, At: now}
		text, blocks := t.replyMessage(m, now, cards.refs)
		res, err := a.post(ctx, db, id, channelID, thread, text, blocks, nil)
		if err != nil {
			a.poller.log.Printf("PR card for %s#%d in %s/%s not posted: %v", p.Repo, p.Number, channelID, thread, err)
			return
		}
		m.TS = res.TS
		t.Thread = cmp.Or(thread, res.TS)
		t.PRCards = append(t.PRCards, m)
		t.Updated = now
		cards.commit(t)
		if thread != "" {
			continue
		}
		if err := a.bindSessionThread(ctx, db, sub, id, res.TS); err != nil {
			a.poller.log.Printf("PR card for %s#%d at %s: its thread goes unwatched: %v", p.Repo, p.Number, res.TS, err)
		}
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
// row's current words. A refused tap also answers in the thread, since nothing
// else tells him.
func (a *app) recardPR(ctx context.Context, id identity, card cardRef, pr refID, refused string) {
	if err := a.redrawPRCard(ctx, id, card, pr); err != nil {
		a.poller.log.Printf("PR card %s stays as drawn: %v", card.ts, err)
	}
	if refused == "" {
		return
	}
	line := refused + " " + slackLink(id.permalink(card.channel, card.ts, card.thread), "The card") + " shows where it stands now."
	if _, err := a.post(ctx, a.poller.plane.DB(), id, card.channel, card.thread, line, nil, nil); err != nil {
		a.poller.log.Printf("PR card %s: the refused tap went untold: %v", card.ts, err)
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
		return err
	}
	return a.cards.replacePRCard(ctx, a.newAPI(id.Token), card, provided[0].Card, a.now())
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

func refusalOf(err error) string {
	failed, ok := errors.AsType[cardCommandError](err)
	if !ok {
		return err.Error()
	}
	return strings.TrimPrefix(failed.said, path.Base(failed.command)+": ")
}

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
