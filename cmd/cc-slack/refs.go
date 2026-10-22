package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	refPR              = "pr"
	refBuild           = "build"
	githubAPI          = "https://api.github.com"
	buildkiteAPI       = "https://api.buildkite.com"
	refTokenTTL        = 30 * time.Minute
	refCommandTimeout  = 15 * time.Second
	refGraphQLBatch    = 25
	refMaxResponseSize = 4 << 20
)

var refNamePattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)

type refID struct {
	Kind   string
	Owner  string
	Repo   string
	Number int
}

func (r refID) key() string { return fmt.Sprintf("%s:%s/%s#%d", r.Kind, r.Owner, r.Repo, r.Number) }

func (r refID) defaultLabel() string {
	if r.Kind == refPR {
		return fmt.Sprintf("%s#%d", r.Repo, r.Number)
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
	out, err := exec.CommandContext(ctx, t.source.Command[0], t.source.Command[1:]...).Output() //nolint:gosec // G204: the configured token command's argv, never a shell
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
	githubURL    string
	buildkiteURL string
	now          func() time.Time
}

func (c refClient) do(ctx context.Context, tok *refToken, req *http.Request) ([]byte, error) {
	token, err := tok.get(ctx, c.now())
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, refMaxResponseSize))
	if err != nil {
		return nil, err
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		tok.forget()
		return nil, fmt.Errorf("%s answered 401", req.URL.Host)
	case resp.StatusCode == http.StatusTooManyRequests || (resp.StatusCode == http.StatusForbidden && rateLimitHeaders(resp.Header)):
		return nil, errRateLimited{until: c.retryAt(resp.Header)}
	case resp.StatusCode/100 != 2:
		return nil, fmt.Errorf("%s answered %d: %s", req.URL.Host, resp.StatusCode, bytes.TrimSpace(body[:min(len(body), 200)]))
	}
	return body, nil
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
	State           string `json:"state"`
	IsDraft         bool   `json:"isDraft"`
	MergeQueueEntry *struct {
		State string `json:"state"`
	} `json:"mergeQueueEntry"`
	Commits struct {
		Nodes []struct {
			Commit struct {
				StatusCheckRollup *struct {
					State string `json:"state"`
				} `json:"statusCheckRollup"`
			} `json:"commit"`
		} `json:"nodes"`
	} `json:"commits"`
}

func (p githubPR) state() refState {
	var s refState
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

func (c refClient) pullRequests(ctx context.Context, tok *refToken, refs []refID) (map[string]refState, error) {
	out := map[string]refState{}
	for start := 0; start < len(refs); start += refGraphQLBatch {
		batch := refs[start:min(start+refGraphQLBatch, len(refs))]
		var q strings.Builder
		q.WriteString("query {")
		for i, r := range batch {
			fmt.Fprintf(&q, " p%d: repository(owner: %s, name: %s) { pullRequest(number: %d) { state isDraft mergeQueueEntry { state } "+
				"commits(last: 1) { nodes { commit { statusCheckRollup { state } } } } } }", i, strconv.Quote(r.Owner), strconv.Quote(r.Repo), r.Number)
		}
		q.WriteString(" }")
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.githubURL+"/graphql", bytes.NewReader(mustJSON(map[string]string{"query": q.String()})))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		body, err := c.do(ctx, tok, req)
		if err != nil {
			return nil, err
		}
		var res struct {
			Data   map[string]*struct{ PullRequest *githubPR } `json:"data"`
			Errors []struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"errors"`
		}
		if err := json.Unmarshal(body, &res); err != nil {
			return nil, fmt.Errorf("decode GitHub pull requests: %w", err)
		}
		for _, e := range res.Errors {
			if e.Type == "RATE_LIMITED" {
				return nil, errRateLimited{until: c.now().Add(refMaxBackoff)}
			}
		}
		if res.Data == nil && len(res.Errors) > 0 {
			return nil, fmt.Errorf("GitHub pull requests: %s", res.Errors[0].Message)
		}
		for i, r := range batch {
			if repo := res.Data[fmt.Sprintf("p%d", i)]; repo != nil && repo.PullRequest != nil {
				out[r.key()] = repo.PullRequest.state()
			}
		}
	}
	return out, nil
}

func (c refClient) build(ctx context.Context, tok *refToken, r refID) (refState, error) {
	u := fmt.Sprintf("%s/v2/organizations/%s/pipelines/%s/builds/%d", c.buildkiteURL, url.PathEscape(r.Owner), url.PathEscape(r.Repo), r.Number)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, http.NoBody)
	if err != nil {
		return refState{}, err
	}
	body, err := c.do(ctx, tok, req)
	if err != nil {
		return refState{}, err
	}
	var b struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(body, &b); err != nil {
		return refState{}, fmt.Errorf("decode Buildkite build: %w", err)
	}
	if b.State == "" {
		return refState{}, errors.New("buildkite build carries no state")
	}
	return refState{State: b.State}, nil
}
