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
)

const (
	prLookupPatience = 5 * time.Second
	prCardPatience   = 30 * time.Second
)

var (
	prUnscanned   = regexp.MustCompile("(?s)```.*?```|`[^`\n]+`|<[^<>\n]*>|https?://[^\\s<>]+")
	prMentioned   = regexp.MustCompile(`(^|[^A-Za-z0-9_.&#/-])(?:([A-Za-z0-9-]{1,39})/([A-Za-z0-9._-]{1,100}))?#([0-9]{1,9})\b`)
	prURLTrailing = regexp.MustCompile(`[.,;:!?)\]'"]+$`)
	githubRemote  = regexp.MustCompile(`github\.com[:/]([A-Za-z0-9-]+)/([A-Za-z0-9._-]+?)(?:\.git)?/?$`)
)

type prMention struct {
	start, end int
	ref        refID
	url        string
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
			out.WriteString(slackLink(cmp.Or(m.url, prURL(pr)), fmt.Sprintf("#%d", pr.Number)))
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

func (a *app) attachPRCards(ctx context.Context, db *sql.DB, id identity, channelID, threadTS string, prs []linkedPR) {
	command := id.Config.PRLinks.CardCommand
	if len(command) == 0 || !cardsAt(id, channelID) {
		return
	}
	cards := a.cards
	cards.mu.Lock()
	defer cards.mu.Unlock()
	t := cards.draft(channelID, threadTS)
	var asked []map[string]any
	for _, pr := range prs {
		if pr.state.terminal() || t.carded(map[string]bool{pr.ref.key(): true}) {
			continue
		}
		asked = append(asked, map[string]any{"repo": pr.ref.Owner + "/" + pr.ref.Repo, "number": pr.ref.Number})
		cards.refs[pr.ref.key()] = pr.state
	}
	if len(asked) == 0 {
		return
	}
	out, err := runCardCommand(ctx, command, map[string]any{"prs": asked})
	if err != nil {
		a.poller.log.Printf("PR cards for %s/%s not asked for: %v", channelID, threadTS, err)
		return
	}
	var answer struct {
		Cards []providedCard `json:"cards"`
	}
	if err := json.Unmarshal(out, &answer); err != nil {
		a.poller.log.Printf("PR cards for %s/%s: decode %s: %v", channelID, threadTS, command[0], err)
		return
	}
	now := a.now()
	for _, p := range answer.Cards {
		if err := p.Card.check(now, false, id.Config.Links.Tailnet); err != nil || p.Card.Waits == "" {
			a.poller.log.Printf("PR card for %s#%d refused: a card command answers with a waiting card that passes the card checks: %v", p.Repo, p.Number, err)
			continue
		}
		m := &liveMessage{Card: p.Card, At: now}
		res, err := a.post(ctx, db, id, channelID, threadTS, m.Card.text(), m.Card.replyBlocks(t.view(m, now, cards.refs)), nil)
		if err != nil {
			a.poller.log.Printf("PR card for %s#%d in %s/%s not posted: %v", p.Repo, p.Number, channelID, threadTS, err)
			break
		}
		m.TS = res.TS
		t.PRCards = append(t.PRCards, m)
	}
	t.Updated = now
	cards.commit(t)
}

func runCardCommand(ctx context.Context, command []string, ask any) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, prCardPatience)
	defer cancel()
	cmd := exec.CommandContext(ctx, command[0], command[1:]...) //nolint:gosec // G204: the configured card command's argv, never a shell
	cmd.Stdin = bytes.NewReader(mustJSON(ask))
	out, err := cmd.Output()
	if exit, ok := errors.AsType[*exec.ExitError](err); ok {
		return nil, fmt.Errorf("%s: %w: %s", command[0], err, bytes.TrimSpace(exit.Stderr))
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", command[0], err)
	}
	return out, nil
}
