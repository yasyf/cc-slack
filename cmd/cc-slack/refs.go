package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	refPR              = "pr"
	refBuild           = "build"
	buildkiteAPI       = "https://api.buildkite.com"
	refTokenTTL        = 30 * time.Minute
	refCommandTimeout  = 15 * time.Second
	refGraphQLBatch    = 25
	refMaxResponseSize = 4 << 20
	refDescriptionAge  = 10 * time.Minute
)

var refNamePattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)

type refID struct {
	Kind   string
	Owner  string
	Repo   string
	Number int
}

func (r refID) key() string {
	return strings.ToLower(fmt.Sprintf("%s:%s/%s#%d", r.Kind, r.Owner, r.Repo, r.Number))
}

func (r refID) defaultLabel() string {
	if r.Kind == refPR {
		return fmt.Sprintf("%s PR %d", r.Repo, r.Number)
	}
	return fmt.Sprintf("%s #%d", r.Repo, r.Number)
}

func parseRef(raw string) (refID, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" {
		return refID{}, false
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 4 || !refNamePattern.MatchString(parts[0]) || !refNamePattern.MatchString(parts[1]) {
		return refID{}, false
	}
	n, err := strconv.Atoi(parts[3])
	if err != nil || n <= 0 {
		return refID{}, false
	}
	switch {
	case u.Host == "github.com" && parts[2] == "pull":
		return refID{Kind: refPR, Owner: parts[0], Repo: parts[1], Number: n}, true
	case u.Host == "buildkite.com" && parts[2] == "builds":
		return refID{Kind: refBuild, Owner: parts[0], Repo: parts[1], Number: n}, true
	}
	return refID{}, false
}

type refState struct {
	State  string `json:"state"`
	Checks string `json:"checks,omitempty"`
	Head   string `json:"head,omitempty"`
}

func (s refState) movedFrom(prior refState) bool {
	return prior.Head != "" && s.Head != "" && s.Head != prior.Head
}

func (s refState) terminal() bool {
	switch s.State {
	case "merged", "closed", "passed", "failed", "canceled", "skipped", "not_run":
		return true
	}
	return false
}

func (s refState) text() string {
	switch s.State {
	case "":
		return ""
	case "merged":
		return "🟣 landed"
	case "closed":
		return "⚪ closed"
	case "draft":
		return "📝 draft"
	case "queued":
		return "⏳ queued" + s.checksSuffix()
	case "open":
		switch s.Checks {
		case "passed":
			return "✅ CI passed"
		case "failed":
			return "❌ CI failed"
		case "pending":
			return "🔄 CI running"
		}
		return "🟢 open"
	case "passed":
		return "✅ build passed"
	case "failed", "failing":
		return "❌ build " + s.State
	case "running":
		return "🔄 build running"
	case "scheduled", "creating":
		return "⏳ build scheduled"
	case "blocked":
		return "⏸️ build blocked"
	case "canceled", "canceling":
		return "⚪ build canceled"
	}
	return "⚪ " + strings.ReplaceAll(s.State, "_", " ")
}

func (s refState) checksSuffix() string {
	switch s.Checks {
	case "passed":
		return ", CI passed"
	case "failed":
		return ", CI failed"
	case "pending":
		return ", CI running"
	}
	return ""
}

type errRateLimited struct{ until time.Time }

func (e errRateLimited) Error() string {
	return "rate limited until " + e.until.UTC().Format(time.RFC3339)
}

type refToken struct {
	mu      sync.Mutex
	source  *tokenSource
	value   string
	fetched time.Time
}

func (t *refToken) get(ctx context.Context, now time.Time) (string, error) {
	if t.source.Token != "" {
		return t.source.Token, nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.value != "" && now.Sub(t.fetched) < refTokenTTL {
		return t.value, nil
	}
	ctx, cancel := context.WithTimeout(ctx, refCommandTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, installed(t.source.Command[0]), t.source.Command[1:]...).Output() //nolint:gosec // G204: the configured token command's argv, never a shell
	if err != nil {
		return "", fmt.Errorf("token command %s: %w", t.source.Command[0], err)
	}
	t.value, t.fetched = strings.TrimSpace(string(out)), now
	if t.value == "" {
		return "", fmt.Errorf("token command %s printed nothing", t.source.Command[0])
	}
	return t.value, nil
}

func (t *refToken) forget() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.value = ""
}

type refClient struct {
	http         *http.Client
	gh           []string
	buildkiteURL string
	now          func() time.Time
	read         *prReads
}

func newRefClient(gh []string, hc *http.Client, buildkiteURL string, now func() time.Time) refClient {
	return refClient{http: hc, gh: gh, buildkiteURL: buildkiteURL, now: now, read: &prReads{last: map[prRead]githubPR{}}}
}

type prRead struct{ fields, ref string }

type prReads struct {
	mu   sync.Mutex
	last map[prRead]githubPR
}

func (p *prReads) keep(fields string, found map[string]githubPR) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for k, pr := range found {
		p.last[prRead{fields, k}] = pr
	}
}

func (p *prReads) recall(fields string, refs []refID) map[string]githubPR {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := map[string]githubPR{}
	for _, r := range refs {
		if pr, ok := p.last[prRead{fields, r.key()}]; ok {
			out[r.key()] = pr
		}
	}
	return out
}

func (c refClient) do(ctx context.Context, tok *refToken, req *http.Request) ([]byte, http.Header, error) {
	token, err := tok.get(ctx, c.now())
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, refMaxResponseSize))
	if err != nil {
		return nil, nil, err
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		tok.forget()
		return nil, nil, fmt.Errorf("%s answered 401", req.URL.Host)
	case resp.StatusCode == http.StatusTooManyRequests || (resp.StatusCode == http.StatusForbidden && rateLimitHeaders(resp.Header)):
		return nil, nil, errRateLimited{until: c.retryAt(resp.Header)}
	case resp.StatusCode/100 != 2:
		return nil, nil, fmt.Errorf("%s answered %d: %s", req.URL.Host, resp.StatusCode, bytes.TrimSpace(body[:min(len(body), 200)]))
	}
	return body, resp.Header, nil
}

func rateLimitHeaders(h http.Header) bool {
	return h.Get("Retry-After") != "" || h.Get("X-RateLimit-Remaining") == "0"
}

func (c refClient) retryAt(h http.Header) time.Time {
	now := c.now()
	if s, err := strconv.Atoi(h.Get("Retry-After")); err == nil {
		return now.Add(time.Duration(s) * time.Second)
	}
	if s, err := strconv.ParseInt(h.Get("X-RateLimit-Reset"), 10, 64); err == nil {
		return time.Unix(s, 0)
	}
	if s, err := strconv.Atoi(h.Get("RateLimit-Reset")); err == nil {
		return now.Add(time.Duration(s) * time.Second)
	}
	return now.Add(refMaxBackoff)
}

type githubPR struct {
	Title           string `json:"title"`
	Body            string `json:"body"`
	State           string `json:"state"`
	IsDraft         bool   `json:"isDraft"`
	MergeQueueEntry *struct {
		State string `json:"state"`
	} `json:"mergeQueueEntry"`
	Commits struct {
		Nodes []struct {
			Commit struct {
				OID               string `json:"oid"`
				StatusCheckRollup *struct {
					State string `json:"state"`
				} `json:"statusCheckRollup"`
			} `json:"commit"`
		} `json:"nodes"`
	} `json:"commits"`
}

func (p githubPR) state() refState {
	var s refState
	if n := p.Commits.Nodes; len(n) > 0 {
		s.Head = n[0].Commit.OID
	}
	if n := p.Commits.Nodes; len(n) > 0 && n[0].Commit.StatusCheckRollup != nil {
		switch n[0].Commit.StatusCheckRollup.State {
		case "SUCCESS":
			s.Checks = "passed"
		case "FAILURE", "ERROR":
			s.Checks = "failed"
		default:
			s.Checks = "pending"
		}
	}
	switch {
	case p.State == "MERGED":
		s.State = "merged"
	case p.State == "CLOSED":
		s.State = "closed"
	case p.MergeQueueEntry != nil:
		s.State = "queued"
	case p.IsDraft:
		s.State = "draft"
	default:
		s.State = "open"
	}
	return s
}

func (c refClient) pullRequests(ctx context.Context, refs []refID) (map[string]refState, error) {
	found, err := c.queryPRs(ctx, refs)
	out := make(map[string]refState, len(found))
	for k, pr := range found {
		out[k] = pr.state()
	}
	return out, err
}

func (c refClient) queryPRs(ctx context.Context, refs []refID) (map[string]githubPR, error) {
	return c.selectPRs(ctx, refs, "state isDraft mergeQueueEntry { state } commits(last: 1) { nodes { commit { oid statusCheckRollup { state } } } }", 0)
}

func (c refClient) describePRs(ctx context.Context, refs []refID) (map[string]githubPR, error) {
	return c.selectPRs(ctx, refs, "title body", refDescriptionAge)
}

func (c refClient) selectPRs(ctx context.Context, refs []refID, fields string, maxAge time.Duration) (map[string]githubPR, error) {
	byRepo := map[string][]refID{}
	for _, r := range refs {
		byRepo[r.Owner+"/"+r.Repo] = append(byRepo[r.Owner+"/"+r.Repo], r)
	}
	out := map[string]githubPR{}
	var errs []error
	for _, repo := range slices.Sorted(maps.Keys(byRepo)) {
		for batch := range slices.Chunk(byRepo[repo], refGraphQLBatch) {
			found, err := c.readPRs(ctx, repo, batch, fields, maxAge)
			if err != nil {
				errs = append(errs, err)
				found = c.read.recall(fields, batch)
			} else {
				c.read.keep(fields, found)
			}
			maps.Copy(out, found)
		}
	}
	return out, errors.Join(errs...)
}

func (c refClient) readPRs(ctx context.Context, repo string, refs []refID, fields string, maxAge time.Duration) (map[string]githubPR, error) {
	var q strings.Builder
	q.WriteString("query {")
	for i, r := range refs {
		fmt.Fprintf(&q, " p%d: repository(owner: %s, name: %s) { pullRequest(number: %d) { %s } }", i, strconv.Quote(r.Owner), strconv.Quote(r.Repo), r.Number, fields)
	}
	q.WriteString(" }")
	args := slices.Clone(c.gh[1:])
	if maxAge > 0 {
		args = append(args, "--max-age", maxAge.String())
	}
	ctx, cancel := context.WithTimeout(ctx, refCommandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.gh[0], append(args, "--", "api", "graphql", "--input", "-")...) //nolint:gosec // G204: argv to ccx's GitHub gateway, never a shell
	cmd.Env = append(os.Environ(), "GH_REPO="+repo, "CCX_CALLER="+binaryName)
	cmd.Stdin = bytes.NewReader(mustJSON(map[string]string{"query": q.String()}))
	cmd.WaitDelay = time.Second
	var said bytes.Buffer
	cmd.Stderr = &said
	began := time.Now()
	answer, err := cmd.Output()
	traced(ctx, "ccx vcs gh graphql", time.Since(began))
	var res struct {
		Data map[string]*struct{ PullRequest *githubPR } `json:"data"`
	}
	if decodeErr := json.Unmarshal(answer, &res); res.Data == nil {
		return nil, fmt.Errorf("read %s pull requests through ccx vcs gh: %w: %s", repo, errors.Join(err, decodeErr), bytes.TrimSpace(said.Bytes()))
	}
	out := map[string]githubPR{}
	for i, r := range refs {
		if repo := res.Data[fmt.Sprintf("p%d", i)]; repo != nil && repo.PullRequest != nil {
			out[r.key()] = *repo.PullRequest
		}
	}
	return out, nil
}

type buildkiteBuild struct {
	State   string `json:"state"`
	Message string `json:"message"`
}

func (c refClient) build(ctx context.Context, tok *refToken, r refID) (refState, error) {
	b, err := c.readBuild(ctx, tok, r)
	return refState{State: b.State}, err
}

func (c refClient) readBuild(ctx context.Context, tok *refToken, r refID) (buildkiteBuild, error) {
	u := fmt.Sprintf("%s/v2/organizations/%s/pipelines/%s/builds/%d", c.buildkiteURL, url.PathEscape(r.Owner), url.PathEscape(r.Repo), r.Number)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, http.NoBody)
	if err != nil {
		return buildkiteBuild{}, err
	}
	body, _, err := c.do(ctx, tok, req)
	if err != nil {
		return buildkiteBuild{}, err
	}
	var b buildkiteBuild
	if err := json.Unmarshal(body, &b); err != nil {
		return buildkiteBuild{}, fmt.Errorf("decode Buildkite build: %w", err)
	}
	if b.State == "" {
		return buildkiteBuild{}, errors.New("buildkite build carries no state")
	}
	return b, nil
}
