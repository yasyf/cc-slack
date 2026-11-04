package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
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
	merged      map[string]bool
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
		if pr := m[2] + "/" + m[3] + "#" + m[4]; f.held[pr] {
			state := "OPEN"
			if f.merged[pr] || f.mergedAfter > 0 && f.reads > f.mergedAfter {
				state = "MERGED"
			}
			data["p"+m[1]] = map[string]any{"pullRequest": map[string]any{"state": state, "isDraft": false}}
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
	return cardingDaemon(t, nil, held...)
}

func cardingDaemon(t *testing.T, cardCommand []string, held ...string) (*liveDaemon, *fakeGitHub) {
	t.Helper()
	gh := &fakeGitHub{held: map[string]bool{}, merged: map[string]bool{}}
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
		"config": map[string]any{"github": map[string]string{"token": "ghp-test"}, "pr_links": map[string]any{"repos": []string{"acme/app", "acme/lib"}, "card_channels": []string{testRoom}, "card_command": cardCommand}},
	})
	return l, gh
}

const (
	testDM    = "D" + testUser
	testRoom  = "C0TEAMROOM"
	testWaits = "merge:41:e718a74"
)

type cardCommand struct {
	argv     []string
	asked    string
	resolved string
}

func fakeCardCommand(t *testing.T, drivePRs []string, cards ...providedCard) cardCommand {
	t.Helper()
	dir := t.TempDir()
	c := cardCommand{argv: []string{filepath.Join(dir, "card")}, asked: filepath.Join(dir, "asked"), resolved: filepath.Join(dir, "resolved")}
	answer, holders := filepath.Join(dir, "cards.json"), filepath.Join(dir, "holders.json")
	if err := os.WriteFile(answer, mustJSON(map[string]any{"cards": append([]providedCard{}, cards...)}), 0o600); err != nil {
		t.Fatal(err)
	}
	held := make([]map[string]any, 0, len(drivePRs))
	for _, pr := range drivePRs {
		repo, number, _ := strings.Cut(pr, "#")
		n, err := strconv.Atoi(number)
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, map[string]any{"repo": repo, "number": n})
	}
	if err := os.WriteFile(holders, mustJSON(map[string]any{"prs": held}), 0o600); err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf("#!/bin/sh\nin=$(cat)\ncase \"$in\" in\n*'\"numbers\"'*) echo \"$in\" >> '%s'; cat '%s' ;;\n*) echo \"$in\" >> '%s'; cat '%s' ;;\nesac\n", c.resolved, holders, c.asked, answer)
	if err := os.WriteFile(c.argv[0], []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return c
}

func (c cardCommand) requests(t *testing.T) []string {
	t.Helper()
	return readAsks(t, c.asked)
}

func (c cardCommand) resolves(t *testing.T) []string {
	t.Helper()
	return readAsks(t, c.resolved)
}

func readAsks(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(string(raw))
}

func mergeRowCard() providedCard {
	return providedCard{Repo: "acme/app", Number: 41, Card: statusCard{
		Status: "needs_you", Title: "Merge #41 at e718a74?",
		Body:    "*Fix the chat list*\n*What changes:* chats load faster\n*Against the plan:* nothing differed\n*Risk:* one cache key\n*Review and CI:* every other gate is met\n*Blocks landing:* your approval",
		Refs:    []cardLink{{URL: "https://github.com/acme/app/pull/41", Label: "acme/app#41"}},
		Options: []string{"Merge", "Hold"}, Recommended: "Merge", Waits: testWaits,
	}}
}

func awaitPRCards(t *testing.T, l *liveDaemon, channelID string, n int) []fakePost {
	t.Helper()
	eventually(t, "the PR cards", func() bool { return len(prCards(l, channelID)) >= n })
	return prCards(l, channelID)
}

func prCards(l *liveDaemon, channelID string) []fakePost {
	var cards []fakePost
	for _, p := range l.slack.sentTo(channelID) {
		if strings.Contains(p.blocks, testWaits) {
			cards = append(cards, p)
		}
	}
	return cards
}

func shown(t *testing.T, p fakePost) string {
	t.Helper()
	var blocks slack.Blocks
	if err := json.Unmarshal([]byte(p.blocks), &blocks); err != nil {
		t.Fatal(err)
	}
	return strings.Join(blockTexts(blocks), "\n")
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
		{"bare, two listed repos hold it", "(#41) is green", ""},
		{"bare, one listed repo holds it", "*#7* is green", "*<https://github.com/acme/lib/pull/7|#7>* is green"},
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

func TestPRReposAddTheSessionRepoToTheListedOnes(t *testing.T) {
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

func TestAnOpenPRGetsItsMergeRowCardInThePostsThreadOnce(t *testing.T) {
	cmd := fakeCardCommand(t, []string{"acme/app#41"}, mergeRowCard())
	l, _ := cardingDaemon(t, cmd.argv, "acme/app#41")
	var sent map[string]string
	if err := json.Unmarshal(l.do(opDMStatus, map[string]any{"text": "#41 is green"}).Body, &sent); err != nil {
		t.Fatal(err)
	}
	cards := awaitPRCards(t, l, testDM, 1)
	if len(cards) != 1 || cards[0].threadTS != sent["thread_ts"] {
		t.Fatalf("cards = %+v, want one in thread %s", cards, sent["thread_ts"])
	}
	for _, want := range []string{"*What changes:* chats load faster", "*Blocks landing:* your approval", "<https://github.com/acme/app/pull/41|acme/app#41>"} {
		if !strings.Contains(shown(t, cards[0]), want) {
			t.Fatalf("card shows %q, want %q in it", shown(t, cards[0]), want)
		}
	}
	if !strings.Contains(cards[0].blocks, "Merge (Recommended)") || !strings.Contains(cards[0].blocks, `"Hold"`) || strings.Contains(cards[0].blocks, cardAckAction) {
		t.Fatalf("card blocks = %s, want Merge and Hold and no Ack", cards[0].blocks)
	}
	if got := cmd.requests(t); !reflect.DeepEqual(got, []string{`{"prs":[{"number":41,"repo":"acme/app"}]}`}) {
		t.Fatalf("card command asked %v", got)
	}
	l.do(opDMStatus, map[string]any{"thread_ts": sent["thread_ts"], "text": "acme/app#41 again"})
	if cards := prCards(l, testDM); len(cards) != 1 || len(cmd.requests(t)) != 1 {
		t.Fatalf("a second mention drew cards %+v after %d asks, want the one card and one ask", cards, len(cmd.requests(t)))
	}
}

func TestAPostReturnsBeforeItsPRCardCommandAnswers(t *testing.T) {
	cmd := fakeCardCommand(t, []string{"acme/app#41"}, mergeRowCard())
	release := filepath.Join(filepath.Dir(cmd.argv[0]), "release")
	script, err := os.ReadFile(cmd.argv[0])
	if err != nil {
		t.Fatal(err)
	}
	slow := strings.Replace(string(script), "*) echo", fmt.Sprintf("*) until [ -e '%s' ]; do sleep 0.02; done; echo", release), 1)
	if err := os.WriteFile(cmd.argv[0], []byte(slow), 0o700); err != nil {
		t.Fatal(err)
	}
	l, _ := cardingDaemon(t, cmd.argv, "acme/app#41")
	l.do(opDMStatus, map[string]any{"text": "#41 is green"})
	if got := l.slack.sentTo(testDM); len(got) != 1 {
		t.Fatalf("posted %+v while the card command ran, want the post alone", got)
	}
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if cards := awaitPRCards(t, l, testDM, 1); len(cards) != 1 {
		t.Fatalf("cards = %+v, want one once the command answers", cards)
	}
}

func TestAMergedPRAndAPRTheBoardDoesNotHoldGetALinkAndNoCard(t *testing.T) {
	t.Run("merged", func(t *testing.T) {
		cmd := fakeCardCommand(t, []string{"acme/app#41"}, mergeRowCard())
		l, gh := cardingDaemon(t, cmd.argv, "acme/app#41")
		gh.mu.Lock()
		gh.merged["acme/app#41"] = true
		gh.mu.Unlock()
		l.do(opDMStatus, map[string]any{"text": "#41 landed"})
		if got := l.slack.sentTo(testDM); len(got) != 1 || got[0].text != "<https://github.com/acme/app/pull/41|#41> landed" || len(cmd.requests(t)) != 0 {
			t.Fatalf("posted %+v and asked %v, want the link alone and no ask", got, cmd.requests(t))
		}
	})
	t.Run("not held", func(t *testing.T) {
		cmd := fakeCardCommand(t, []string{"acme/app#41"})
		l, _ := cardingDaemon(t, cmd.argv, "acme/app#41")
		l.do(opDMStatus, map[string]any{"text": "#41 is green"})
		eventually(t, "the ask", func() bool { return len(cmd.requests(t)) == 1 })
		if got := l.slack.sentTo(testDM); len(got) != 1 {
			t.Fatalf("posted %+v after %d asks, want the linked post alone after one ask", got, len(cmd.requests(t)))
		}
	})
}

func TestPRCardsOnlyInTheDMAndListedChannels(t *testing.T) {
	cmd := fakeCardCommand(t, []string{"acme/app#41"}, mergeRowCard())
	l, _ := cardingDaemon(t, cmd.argv, "acme/app#41")
	var sent postResult
	if err := json.Unmarshal(l.post(opSend, map[string]any{"channel_id": testChannel, "text": "#41 is green"}).Body, &sent); err != nil {
		t.Fatal(err)
	}
	l.post(opReply, map[string]any{"channel_id": testChannel, "thread_ts": sent.TS, "text": "#41 is green"})
	if got := l.slack.sentTo(testChannel); len(got) != 2 || len(cmd.requests(t)) != 0 {
		t.Fatalf("unlisted channel got %+v after %d asks, want two linked posts, no card and no ask, even in a thread we posted", got, len(cmd.requests(t)))
	}
	l.post(opSend, map[string]any{"channel_id": testRoom, "text": "#41 is green"})
	if cards := awaitPRCards(t, l, testRoom, 1); len(cards) != 1 {
		t.Fatalf("listed channel got cards %+v, want one", cards)
	}
}

func TestMergeOnAPRCardGoesToTheCardCommand(t *testing.T) {
	cmd := fakeCardCommand(t, []string{"acme/app#41"}, mergeRowCard())
	l, _ := cardingDaemon(t, cmd.argv, "acme/app#41")
	l.do(opDMStatus, map[string]any{"text": "#41 is green"})
	card := awaitPRCards(t, l, testDM, 1)[0]
	id, err := loadIdentity(l.ctx, l.srv.DB())
	if err != nil {
		t.Fatal(err)
	}
	l.tap(id, cardActionPush{User: testUser, Channel: testDM, MessageTS: card.ts, ThreadTS: card.threadTS,
		ActionID: cardWaitAction + "0:" + testWaits, Value: string(mustJSON(askTap{ID: testWaits, Label: "Merge", Question: "Merge #41 at e718a74?", Options: []string{"Merge", "Hold"}})),
		ActionTS: fmt.Sprintf("%d.000100", time.Now().Unix()), Seq: "001800000100000-abcd0300"})
	want := `{"answer":{"label":"Merge","number":41,"repo":"acme/app","waits":"merge:41:e718a74"}}`
	if got := cmd.requests(t); len(got) != 2 || got[1] != want {
		t.Fatalf("card command asked %v, want the Merge answer last", got)
	}
	if edits := editsOf(l.slack.edited(), card.ts); len(edits) == 0 || !strings.Contains(edits[len(edits)-1], "✓ *Merge* · answered") {
		t.Fatalf("card edits = %v, want the answer in place of the buttons", edits)
	}
}

func TestARefsOnlyCardOffersNoAckOrPause(t *testing.T) {
	l, _ := linkingDaemon(t)
	l.do(opDMStatus, map[string]any{"card": statusCard{Status: "info", Title: "The relay fix", Refs: []cardLink{{URL: "https://github.com/acme/app/pull/41"}}}})
	posted := l.slack.sentTo(testDM)
	if len(posted) != 1 || strings.Contains(posted[0].blocks, cardAckAction) || strings.Contains(posted[0].blocks, cardPauseAction) {
		t.Fatalf("posted %+v, want one card with no Ack or Pause", posted)
	}
}

func TestPRCardRedrawsWhenItsPRLands(t *testing.T) {
	cmd := fakeCardCommand(t, []string{"acme/app#41"}, mergeRowCard())
	l, gh := cardingDaemon(t, cmd.argv, "acme/app#41")
	gh.mu.Lock()
	gh.mergedAfter = 1
	gh.mu.Unlock()
	l.do(opDMStatus, map[string]any{"text": "#41 is green"})
	card := awaitPRCards(t, l, testDM, 1)[0]
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

func TestABareNumberLinksToTheDrivePRItNamesOrStaysPlain(t *testing.T) {
	const github = "https://github.com/yasyf/cc-skills/pull/2475"
	for _, tc := range []struct {
		name, text string
		drivePRs   []string
		want       string
		resolves   int
	}{
		{"one drive PR holds it", "Merges #2475", []string{"yasyf/cc-skills#2475"}, "Merges <" + github + "|#2475>", 1},
		{"no drive PR holds it", "Merges #2475", nil, "Merges #2475", 1},
		{"two drive PRs hold it", "Merges #2475", []string{"yasyf/cc-skills#2475", "acme/app#2475"}, "Merges #2475", 1},
		{"qualified", "Merges yasyf/cc-skills#2475", nil, "Merges <" + github + "|#2475>", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := fakeCardCommand(t, tc.drivePRs)
			l, _ := cardingDaemon(t, cmd.argv, "acme/app#2475", "yasyf/cc-skills#2475")
			l.do(opDMStatus, map[string]any{"text": tc.text})
			if got := l.slack.sentTo(testDM)[0].text; got != tc.want {
				t.Fatalf("posted %q, want %q", got, tc.want)
			}
			if got := cmd.resolves(t); len(got) != tc.resolves || tc.resolves == 1 && got[0] != `{"numbers":[2475]}` {
				t.Fatalf("card command resolved %v, want %d asks for [2475]", got, tc.resolves)
			}
			wantCards := []string{}
			if tc.want != tc.text {
				wantCards = []string{`{"prs":[{"number":2475,"repo":"yasyf/cc-skills"}]}`}
			}
			eventually(t, "the asks", func() bool { return len(cmd.requests(t)) == len(wantCards) })
			if got := cmd.requests(t); len(got) == 1 && got[0] != wantCards[0] {
				t.Fatalf("card command asked %v, want %v", got, wantCards)
			}
		})
	}
}
