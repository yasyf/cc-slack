package main

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

const prLookupPatience = 5 * time.Second

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
	title string
	state refState
}

func (p linkedPR) label() string {
	return clipRunes(fmt.Sprintf("%s#%d %s", p.ref.Repo, p.ref.Number, p.title), cardMaxLabelLen)
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

func (m prMention) candidates(repos []refID) []refID {
	if m.ref.Owner != "" {
		return []refID{m.ref}
	}
	out := make([]refID, len(repos))
	for i, r := range repos {
		out[i] = refID{Kind: refPR, Owner: r.Owner, Repo: r.Repo, Number: m.ref.Number}
	}
	return out
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
	bare := false
	for i, text := range texts {
		mentions[i] = prMentions(text)
		bare = bare || slices.ContainsFunc(mentions[i], func(m prMention) bool { return m.ref.Owner == "" })
	}
	var repos []refID
	if bare {
		repos = prRepos(ctx, scope, id.Config.PRLinks.Repos)
	}
	var asked []refID
	for _, m := range slices.Concat(mentions...) {
		for _, r := range m.candidates(repos) {
			if !slices.Contains(asked, r) {
				asked = append(asked, r)
			}
		}
	}
	if len(asked) == 0 {
		return texts, nil
	}
	lookup, cancel := context.WithTimeout(ctx, prLookupPatience)
	defer cancel()
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
			held := m.candidates(repos)
			k := slices.IndexFunc(held, func(r refID) bool { _, ok := found[r.key()]; return ok })
			if k < 0 {
				continue
			}
			out.WriteString(text[at:m.start])
			out.WriteString(slackLink(cmp.Or(m.url, prURL(held[k])), fmt.Sprintf("#%d", held[k].Number)))
			at = m.end
			if gh := found[held[k].key()]; !slices.ContainsFunc(prs, func(p linkedPR) bool { return p.ref == held[k] }) {
				prs = append(prs, linkedPR{ref: held[k], title: gh.Title, state: gh.state()})
			}
		}
		out.WriteString(text[at:])
		linked[i] = out.String()
	}
	return linked, prs
}

func cardsAt(id identity, channelID string) bool {
	return strings.HasPrefix(channelID, "D") || slices.Contains(id.Config.PRLinks.CardChannels, channelID)
}

func (a *app) attachPRCards(ctx context.Context, db *sql.DB, id identity, channelID, threadTS string, prs []linkedPR) {
	if len(prs) == 0 || !cardsAt(id, channelID) {
		return
	}
	cards := a.cards
	cards.mu.Lock()
	defer cards.mu.Unlock()
	t := cards.draft(channelID, threadTS)
	prs = slices.DeleteFunc(slices.Clone(prs), func(pr linkedPR) bool { return t.carded(map[string]bool{pr.ref.key(): true}) })
	if len(prs) == 0 {
		return
	}
	now := a.now()
	for chunk := range slices.Chunk(prs, cardMaxRefs) {
		m := &liveMessage{At: now}
		labels := make([]string, len(chunk))
		for i, pr := range chunk {
			labels[i] = pr.label()
			m.Card.Refs = append(m.Card.Refs, cardLink{URL: prURL(pr.ref), Label: labels[i]})
			cards.refs[pr.ref.key()] = pr.state
		}
		m.Card.Title = strings.Join(labels, "\n")
		res, err := a.post(ctx, db, id, channelID, threadTS, m.Card.Title, m.Card.prBlocks(t.view(m, now, cards.refs)), nil)
		if err != nil {
			a.poller.log.Printf("PR card in %s/%s not posted: %v", channelID, threadTS, err)
			break
		}
		m.TS = res.TS
		t.PRCards = append(t.PRCards, m)
	}
	t.Updated = now
	cards.commit(t)
}

func (c *statusCard) cite(prs []linkedPR) {
	for _, pr := range prs {
		if len(c.Refs) == cardMaxRefs {
			return
		}
		if !slices.ContainsFunc(c.refIDs(), func(id refID) bool { return strings.EqualFold(id.key(), pr.ref.key()) }) {
			c.Refs = append(c.Refs, cardLink{URL: prURL(pr.ref), Label: pr.label()})
		}
	}
}

func (l *liveCards) seed(prs []linkedPR) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, pr := range prs {
		l.refs[pr.ref.key()] = pr.state
	}
}
