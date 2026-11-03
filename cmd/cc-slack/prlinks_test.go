package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/slack-go/slack"
)

var prAlias = regexp.MustCompile(`p(\d+): repository\(owner: "([^"]+)", name: "([^"]+)"\) \{ pullRequest\(number: (\d+)\)`)

type fakeGitHub struct {
	mu    sync.Mutex
	held  map[string]bool
	down  bool
	reads int
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	if f.down {
		http.Error(w, "unicorn", http.StatusBadGateway)
		return
	}
	var body struct {
		Query string `json:"query"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	data := map[string]any{}
	var errs []map[string]string
	for _, m := range prAlias.FindAllStringSubmatch(body.Query, -1) {
		if f.held[m[2]+"/"+m[3]+"#"+m[4]] {
			data["p"+m[1]] = map[string]any{"pullRequest": map[string]any{"state": "OPEN", "isDraft": false}}
			continue
		}
		data["p"+m[1]] = map[string]any{"pullRequest": nil}
		errs = append(errs, map[string]string{"type": "NOT_FOUND", "message": "Could not resolve to a PullRequest with the number of " + m[4] + "."})
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data, "errors": errs})
}

func (f *fakeGitHub) readCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads
}

func linkingDaemon(t *testing.T, held ...string) (*liveDaemon, *fakeGitHub) {
	t.Helper()
	gh := &fakeGitHub{held: map[string]bool{}}
	for _, h := range held {
		gh.held[h] = true
	}
	srv := httptest.NewServer(gh)
	t.Cleanup(srv.Close)
	l := startDaemon(t, func(a *app) {
		a.cards.client = refClient{http: srv.Client(), githubURL: srv.URL, buildkiteURL: srv.URL, now: time.Now}
	})
	l.slack.setUser(&slack.User{ID: testUser, Name: "ym", RealName: "Yasyf Mohamedali", Profile: slack.UserProfile{
		Email: "ym@example.com", Image512: "https://avatars/ym.png", DisplayName: "yasyf",
	}})
	l.do(opLogin, map[string]any{
		"token": "xoxb-test", "email": "ym@example.com", "socket_url": "ws://127.0.0.1:1/live", "repo_root": "/repo", "capt_hook_path": testCaptHook,
		"avatar_key_id": "r2-key", "avatar_secret": "r2-secret",
		"config": map[string]any{"github": map[string]string{"token": "ghp-test"}, "pr_links": map[string]any{"repos": []string{"acme/app", "acme/lib"}}},
	})
	return l, gh
}

func lastText(l *liveDaemon) string {
	posts := l.slack.sent()
	return posts[len(posts)-1].text
}

func TestPostLinksEachPRReferenceForm(t *testing.T) {
	l, _ := linkingDaemon(t, "acme/app#41", "acme/lib#7", "acme/lib#41")
	for _, tc := range []struct{ name, text, want string }{
		{"pr url", "see https://github.com/acme/app/pull/41.", "see <https://github.com/acme/app/pull/41|#41>."},
		{"pr url with a path", "diff at https://github.com/Acme/App/pull/41/files", "diff at <https://github.com/Acme/App/pull/41/files|#41>"},
		{"owner/repo", "acme/lib#7 landed", "<https://github.com/acme/lib/pull/7|#7> landed"},
		{"bare, first listed repo wins", "(#41) is green", "(<https://github.com/acme/app/pull/41|#41>) is green"},
		{"bare, later listed repo", "*#7* is green", "*<https://github.com/acme/lib/pull/7|#7>* is green"},
		{"already linked", "<https://github.com/acme/app/pull/41|the fix> in <#C1|general>", ""},
		{"code span", "run `gh pr view #41`", ""},
		{"code block", "```\nacme/app#41 and #7\n```", ""},
		{"no listed repo holds it", "step #1 of 3", ""},
		{"another site's fragment", "https://example.com/doc#41", ""},
		{"inside a word or entity", "issue#41 &#41;", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l.post(opSend, map[string]any{"channel_id": testChannel, "text": tc.text})
			want := tc.want
			if want == "" {
				want = tc.text
			}
			if got := lastText(l); got != want {
				t.Fatalf("posted %q, want %q", got, want)
			}
		})
	}
}

func TestReplyEditAndDMStatusLinkPRs(t *testing.T) {
	l, _ := linkingDaemon(t, "acme/app#41")
	const linked = "<https://github.com/acme/app/pull/41|#41> is green"
	var sent postResult
	if err := json.Unmarshal(l.post(opSend, map[string]any{"channel_id": testChannel, "text": "CI"}).Body, &sent); err != nil {
		t.Fatal(err)
	}
	l.post(opReply, map[string]any{"channel_id": testChannel, "thread_ts": sent.TS, "text": "#41 is green"})
	if got := lastText(l); got != linked {
		t.Fatalf("reply posted %q, want %q", got, linked)
	}
	l.post(opEdit, map[string]any{"channel_id": testChannel, "ts": sent.TS, "text": "#41 is green"})
	if edits := l.slack.edited(); len(edits) != 1 || edits[0].text != linked {
		t.Fatalf("edits = %+v, want one with %q", edits, linked)
	}
	l.do(opDMStatus, map[string]any{"text": "#41 is green"})
	if got := lastText(l); got != linked {
		t.Fatalf("dm-status posted %q, want %q", got, linked)
	}
}

func TestPostGoesOutUnlinkedWhenGitHubFails(t *testing.T) {
	l, gh := linkingDaemon(t, "acme/app#41")
	gh.mu.Lock()
	gh.down = true
	gh.mu.Unlock()
	var res postResult
	if err := json.Unmarshal(l.post(opSend, map[string]any{"channel_id": testChannel, "text": "#41 is green"}).Body, &res); err != nil {
		t.Fatal(err)
	}
	if !res.Posted || lastText(l) != "#41 is green" {
		t.Fatalf("posted=%v text=%q, want the text posted as written", res.Posted, lastText(l))
	}
}

func TestPostReadsGitHubOnceHoweverManyPRs(t *testing.T) {
	for _, n := range []int{1, 20} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			var held, refs []string
			for i := range n {
				held = append(held, fmt.Sprintf("acme/lib#%d", 100+i))
				refs = append(refs, fmt.Sprintf("#%d", 100+i))
			}
			l, gh := linkingDaemon(t, held...)
			l.post(opSend, map[string]any{"channel_id": testChannel, "text": strings.Join(refs, " ")})
			if reads := gh.readCount(); reads != 1 {
				t.Fatalf("%d PRs took %d GitHub reads, want 1", n, reads)
			}
			if links := strings.Count(lastText(l), "<https://github.com/acme/lib/pull/"); links != n {
				t.Fatalf("posted %q with %d links, want %d", lastText(l), links, n)
			}
		})
	}
}

func TestPRReposTryTheSessionRepoFirst(t *testing.T) {
	dir := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"remote", "add", "origin", "git@github.com:Acme/Lib.git"}} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	got := prRepos(context.Background(), dir, []string{"acme/app", "acme/lib"})
	want := []refID{repoRef("acme", "lib"), repoRef("acme", "app")}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("repos = %v, want %v", got, want)
	}
}
