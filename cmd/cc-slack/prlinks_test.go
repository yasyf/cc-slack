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
	mu          sync.Mutex
	held        map[string]bool
	down        bool
	reads       int
	mergedAfter int
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
			state := "OPEN"
			if f.mergedAfter > 0 && f.reads > f.mergedAfter {
				state = "MERGED"
			}
			data["p"+m[1]] = map[string]any{"pullRequest": map[string]any{"title": "Fix PR " + m[4], "state": state, "isDraft": false}}
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
		"config": map[string]any{"github": map[string]string{"token": "ghp-test"}, "pr_links": map[string]any{"repos": []string{"acme/app", "acme/lib"}, "card_channels": []string{testRoom}}},
	})
	return l, gh
}

const (
	testDM   = "D" + testUser
	testRoom = "C0TEAMROOM"
)

func lastText(l *liveDaemon) string {
	posts := l.slack.sent()
	return posts[len(posts)-1].text
}

func shown(t *testing.T, p fakePost) string {
	t.Helper()
	var blocks slack.Blocks
	if err := json.Unmarshal([]byte(p.blocks), &blocks); err != nil {
		t.Fatal(err)
	}
	return strings.Join(blockTexts(blocks), "\n")
}

func prCards(l *liveDaemon, channelID string) []fakePost {
	var cards []fakePost
	for _, p := range l.slack.sentTo(channelID) {
		if strings.Contains(p.blocks, `"card-prs"`) {
			cards = append(cards, p)
		}
	}
	return cards
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
	if got := l.slack.sentTo(testDM)[0].text; got != linked {
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

func TestPRCardLandsInThePostsThreadOnce(t *testing.T) {
	l, _ := linkingDaemon(t, "acme/app#41", "acme/lib#7")
	var sent map[string]string
	if err := json.Unmarshal(l.do(opDMStatus, map[string]any{"text": "#41 is green"}).Body, &sent); err != nil {
		t.Fatal(err)
	}
	cards := prCards(l, testDM)
	if len(cards) != 1 || cards[0].threadTS != sent["thread_ts"] || !strings.Contains(shown(t, cards[0]), "<https://github.com/acme/app/pull/41|app#41 Fix PR 41>") {
		t.Fatalf("cards = %+v, want one for acme/app#41 in thread %s", cards, sent["thread_ts"])
	}
	l.do(opDMStatus, map[string]any{"thread_ts": sent["thread_ts"], "text": "acme/app#41 and acme/lib#7 next"})
	cards = prCards(l, testDM)
	if len(cards) != 2 || strings.Contains(shown(t, cards[1]), "pull/41") || !strings.Contains(shown(t, cards[1]), "pull/7") {
		t.Fatalf("cards after a second mention = %+v, want one more, for acme/lib#7 only", cards)
	}
}

func TestPRCardsOnlyInTheDMAndListedChannels(t *testing.T) {
	l, _ := linkingDaemon(t, "acme/app#41")
	const linked = "<https://github.com/acme/app/pull/41|#41> is green"
	var sent postResult
	if err := json.Unmarshal(l.post(opSend, map[string]any{"channel_id": testChannel, "text": "#41 is green"}).Body, &sent); err != nil {
		t.Fatal(err)
	}
	l.post(opReply, map[string]any{"channel_id": testChannel, "thread_ts": sent.TS, "text": "#41 is green"})
	if got := l.slack.sentTo(testChannel); len(got) != 2 || got[0].text != linked || got[1].text != linked || len(prCards(l, testChannel)) != 0 {
		t.Fatalf("unlisted channel got %+v, want two linked posts and no card, even in a thread we posted", got)
	}
	l.post(opSend, map[string]any{"channel_id": testRoom, "text": "#41 is green"})
	if cards := prCards(l, testRoom); len(cards) != 1 {
		t.Fatalf("listed channel got cards %+v, want one", cards)
	}
}

func TestCardNamingAPRCitesIt(t *testing.T) {
	l, _ := linkingDaemon(t, "acme/app#41")
	l.do(opDMStatus, map[string]any{"card": statusCard{Status: "info", Title: "Landing #41", Body: "#41 is green"}})
	posted := l.slack.sentTo(testDM)
	if len(posted) != 1 || !strings.Contains(shown(t, posted[0]), "<https://github.com/acme/app/pull/41|#41> is green") ||
		!strings.Contains(shown(t, posted[0]), "<https://github.com/acme/app/pull/41|app#41 Fix PR 41>") {
		t.Fatalf("posted %+v, want one card with a linked body and the PR in its refs", posted)
	}
}

func TestPRCardRedrawsWhenItsPRLands(t *testing.T) {
	l, gh := linkingDaemon(t, "acme/app#41")
	gh.mu.Lock()
	gh.mergedAfter = 1
	gh.mu.Unlock()
	l.do(opDMStatus, map[string]any{"text": "#41 is green"})
	card := prCards(l, testDM)[0]
	if !strings.Contains(shown(t, card), "🟢 open") {
		t.Fatalf("card = %q, want the state the post's read saw", shown(t, card))
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, b := range editsOf(l.slack.edited(), card.ts) {
			if strings.Contains(b, "🟣 landed") {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("card edits %v never showed the PR landed", editsOf(l.slack.edited(), card.ts))
}
