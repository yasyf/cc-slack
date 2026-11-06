package main

import (
	"cmp"
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
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/slack-go/slack"
	"github.com/yasyf/cc-interact/agent"
)

var prAlias = regexp.MustCompile(`p(\d+): repository\(owner: "([^"]+)", name: "([^"]+)"\) \{ pullRequest\(number: (\d+)\)`)

type fakeGitHub struct {
	mu          sync.Mutex
	held        map[string]bool
	merged      map[string]bool
	down        bool
	reads       int
	mergedAfter int
	stall       chan struct{}
	stalled     int
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	if stall := f.stall; stall != nil {
		f.stalled++
		f.mu.Unlock()
		<-stall
		f.mu.Lock()
	}
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
	return prLinksDaemon(t, map[string]any{"repos": []string{"acme/app", "acme/lib"}, "card_channels": []string{testRoom}, "card_command": cardCommand}, held...)
}

func prLinksDaemon(t *testing.T, prLinks map[string]any, held ...string) (*liveDaemon, *fakeGitHub) {
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
		"config": map[string]any{"github": map[string]string{"token": "ghp-test"}, "pr_links": prLinks},
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
	return slices.DeleteFunc(readAsks(t, c.asked), isShown)
}

func (c cardCommand) shown(t *testing.T) []string {
	t.Helper()
	return slices.DeleteFunc(readAsks(t, c.asked), func(ask string) bool { return !isShown(ask) })
}

func isShown(ask string) bool {
	return strings.Contains(ask, `"shown"`)
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

const testPick = "card-41"

var tapPR41 = []string{"https://github.com/acme/app/pull/41"}

func mergeRowCard() providedCard {
	return providedCard{Repo: "acme/app", Number: 41, Card: statusCard{
		Status: "needs_you", Title: "Merge #41 at e718a74?",
		Body:    "*Fix the chat list*\n*What changes:* chats load faster\n*Against the plan:* nothing differed\n*Risk:* one cache key\n*Review and CI:* every other gate is met\n*Blocks landing:* your approval",
		Refs:    []cardLink{{URL: "https://github.com/acme/app/pull/41", Label: "acme/app#41"}},
		Options: []string{"Merge", "Hold"}, Recommended: "Merge", Waits: testWaits,
	}}
}

func awaitPRCards(t *testing.T, l *liveDaemon, channelID string) []fakePost {
	t.Helper()
	eventually(t, "the PR cards", func() bool { return len(prCards(l, channelID)) > 0 })
	return prCards(l, channelID)
}

func rootCards(cards []fakePost) []fakePost {
	return slices.DeleteFunc(cards, func(p fakePost) bool { return p.threadTS != "" })
}

func heldRowCard() providedCard {
	held := mergeRowCard()
	held.Card.Status, held.Card.Title = "info", "#41 at e718a74"
	held.Card.Options, held.Card.Recommended, held.Card.Waits = nil, "", ""
	return held
}

func TestAHeldRowDrawsItsCardAndTheSamePostAsksWhenTheHoldLifts(t *testing.T) {
	cmd := fakeCardCommand(t, []string{"acme/app#41"}, heldRowCard())
	l, _ := cardingDaemon(t, cmd.argv, "acme/app#41")
	l.do(opDMStatus, map[string]any{"text": "#41 is held", "tap_prs": tapPR41})
	drawnHeld := func() (fakePost, bool) {
		for _, p := range l.slack.sentTo(testDM) {
			if strings.Contains(p.blocks, "#41 at e718a74") {
				return p, true
			}
		}
		return fakePost{}, false
	}
	eventually(t, "the held row's card", func() bool { _, drawn := drawnHeld(); return drawn })
	card, _ := drawnHeld()
	for _, button := range []string{"Merge", cardAckAction, cardPauseAction} {
		if strings.Contains(card.blocks, button) {
			t.Fatalf("held card blocks = %s, want no %s", card.blocks, button)
		}
	}
	answer := filepath.Join(filepath.Dir(cmd.argv[0]), "cards.json")
	redraw := func(drawn providedCard) {
		if err := os.WriteFile(answer, mustJSON(map[string]any{"cards": []providedCard{drawn}}), 0o600); err != nil {
			t.Fatal(err)
		}
		l.do(opRedrawPRCards, map[string]any{})
	}
	lastEdit := func(wants string) bool {
		edits := editsOf(l.slack.edited(), card.ts)
		return len(edits) > 0 && strings.Contains(edits[len(edits)-1], wants)
	}
	posts := len(l.slack.sentTo(testDM))
	redraw(mergeRowCard())
	eventually(t, "Merge on the same post", func() bool { return lastEdit("Merge (Recommended)") })
	redraw(heldRowCard())
	eventually(t, "the buttons gone from the same post", func() bool { return lastEdit(cardNoLongerWaits) })
	redraw(mergeRowCard())
	eventually(t, "Merge back on the same post", func() bool { return lastEdit("Merge (Recommended)") })
	if got := len(l.slack.sentTo(testDM)); got != posts {
		t.Fatalf("posts = %d, want %d: a row's change edits its post", got, posts)
	}
}

func TestACopyUnderAPostFollowsARedrawOfItsCard(t *testing.T) {
	const hisThread = "1793804574.412309"
	cmd := fakeCardCommand(t, []string{"acme/app#41"}, mergeRowCard())
	l, _ := cardingDaemon(t, cmd.argv, "acme/app#41")
	l.do(opDMStatus, map[string]any{"thread_ts": hisThread, "text": "#41 is green", "tap_prs": tapPR41})
	copied := func() (fakePost, bool) {
		for _, p := range prCards(l, testDM) {
			if p.threadTS == hisThread {
				return p, true
			}
		}
		return fakePost{}, false
	}
	eventually(t, "the copy", func() bool { _, drawn := copied(); return drawn })
	theCopy, _ := copied()
	if err := os.WriteFile(filepath.Join(filepath.Dir(cmd.argv[0]), "cards.json"), mustJSON(map[string]any{"cards": []providedCard{movedCard()}}), 0o600); err != nil {
		t.Fatal(err)
	}
	l.do(opRedrawPRCards, map[string]any{})
	eventually(t, "the copy redrawn", func() bool { return len(editsOf(l.slack.edited(), theCopy.ts)) > 0 })
}

func prCards(l *liveDaemon, channelID string) []fakePost {
	var cards []fakePost
	for _, p := range l.slack.sentTo(channelID) {
		carries := (p.threadTS == "" || p.text == carriedMoreText) && strings.Contains(p.blocks, cardCopyBlock+"-")
		if strings.Contains(p.blocks, testWaits) && !carries {
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
		{"markdown link naming the pr", "[#41](https://github.com/Acme/App/pull/41) is green", "<https://github.com/Acme/App/pull/41|#41> is green"},
		{"markdown link with words", "[the fix](https://github.com/acme/app/pull/41) is green", "<https://github.com/acme/app/pull/41|the fix> is green"},
		{"markdown link to another site", "[#41 notes](https://example.com/doc) and acme/lib#7", "[#41 notes](https://example.com/doc) and <https://github.com/acme/lib/pull/7|#7>"},
		{"code span", "run `gh pr view #41`", ""},
		{"markdown link in a code span", "`[#41](https://github.com/acme/app/pull/41)`", ""},
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

func TestACardPRCarryingItsDescriptionKeepsItOverGitHubs(t *testing.T) {
	l, _ := linkingDaemon(t, "acme/app#41")
	l.do(opDMStatus, map[string]any{"text": "Approve it?", "card": map[string]any{"title": "Approve it?", "prs": []map[string]any{{
		"url": "https://github.com/acme/app/pull/41", "title": "One credential per team", "description": "The ledger's description.",
		"change": "Launches stop waiting", "plan": "Nothing differed", "risk": "None", "blocks": "Your approval",
	}}}})
	card := lastPost(l)
	id, err := loadIdentity(l.ctx, l.srv.DB())
	if err != nil {
		t.Fatal(err)
	}
	l.tap(id, cardActionPush{User: testUser, Channel: id.DMChannelID, MessageTS: card.ts, ThreadTS: card.ts, ActionID: cardFoldAction,
		ActionTS: fmt.Sprintf("%d.000050", time.Now().Unix()), Seq: "001800000100000-abcd0310"})
	if got := lastPost(l).blocks; !strings.Contains(got, "The ledger's description.") {
		t.Fatalf("Explain more drew %s, want the card's own description", got)
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

func TestAnOpenPRGetsItsMergeRowCardAtTheDMRootOnce(t *testing.T) {
	cmd := fakeCardCommand(t, []string{"acme/app#41"}, mergeRowCard())
	l, _ := cardingDaemon(t, cmd.argv, "acme/app#41")
	var sent map[string]string
	if err := json.Unmarshal(l.do(opDMStatus, map[string]any{"text": "#41 is green", "tap_prs": tapPR41}).Body, &sent); err != nil {
		t.Fatal(err)
	}
	cards := rootCards(awaitPRCards(t, l, testDM))
	if len(cards) != 1 || cards[0].threadTS != "" {
		t.Fatalf("cards = %+v, want one at the DM root", cards)
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
	l.do(opDMStatus, map[string]any{"thread_ts": sent["thread_ts"], "text": "acme/app#41 again", "tap_prs": tapPR41})
	l.do(opDMStatus, map[string]any{"text": "acme/app#41 elsewhere", "tap_prs": tapPR41})
	l.do(opDMStatus, map[string]any{"thread_ts": cards[0].ts, "text": "acme/app#41 under its own card", "tap_prs": tapPR41})
	eventually(t, "the copy under the reply naming the PR", func() bool {
		return slices.ContainsFunc(prCards(l, testDM), func(p fakePost) bool { return p.threadTS == sent["thread_ts"] })
	})
	if cards := rootCards(prCards(l, testDM)); len(cards) != 1 || len(cmd.requests(t)) != 1 {
		t.Fatalf("later mentions drew cards %+v after %d asks, want the one card and one ask", cards, len(cmd.requests(t)))
	}
}

func TestAPostInHisThreadNamingAPRDrawsItsCardAtTheDMRoot(t *testing.T) {
	const hisThread = "1793804574.412309"
	cmd := fakeCardCommand(t, []string{"acme/app#41"}, mergeRowCard())
	l, _ := cardingDaemon(t, cmd.argv, "acme/app#41")
	l.do(opDMStatus, map[string]any{"thread_ts": hisThread, "text": "#41 is green", "tap_prs": tapPR41})
	cards := rootCards(awaitPRCards(t, l, testDM))
	if len(cards) != 1 {
		t.Fatalf("cards = %+v, want one at the DM root, not in his thread %s", cards, hisThread)
	}
	eventually(t, "a working copy under the post in his thread", func() bool {
		return slices.ContainsFunc(prCards(l, testDM), func(p fakePost) bool { return p.threadTS == hisThread })
	})
	eventually(t, "the watch on the card's own thread", func() bool { return slices.Contains(activeDMThreads(t, l), cards[0].ts) })
	tapMerge(t, l, cards[0])
	if edits := editsOf(l.slack.edited(), cards[0].ts); len(edits) == 0 || !strings.Contains(edits[len(edits)-1], "✓ *Merge* · answered") {
		t.Fatalf("card edits = %v, want the tap on the root card applied", edits)
	}
}

func TestAPRAskedAgainAtANewHeadRedrawsItsCardInPlace(t *testing.T) {
	cmd := fakeCardCommand(t, []string{"acme/app#41"}, mergeRowCard())
	l, _ := cardingDaemon(t, cmd.argv, "acme/app#41")
	l.do(opDMStatus, map[string]any{"text": "#41 is green", "tap_prs": tapPR41})
	card := awaitPRCards(t, l, testDM)[0]
	tapMerge(t, l, card)
	asks := len(cmd.requests(t))
	l.do(opDMStatus, map[string]any{"text": "#41 at the head he answered", "tap_prs": tapPR41})
	eventually(t, "the ask after his answer", func() bool { return len(cmd.requests(t)) > asks })
	if got := rootCards(prCards(l, testDM)); len(got) != 1 {
		t.Fatalf("PR cards = %+v, want no second card for the head he answered", got)
	}
	answer := filepath.Join(filepath.Dir(cmd.argv[0]), "cards.json")
	if err := os.WriteFile(answer, mustJSON(map[string]any{"cards": []providedCard{movedCard()}}), 0o600); err != nil {
		t.Fatal(err)
	}
	l.do(opDMStatus, map[string]any{"text": "#41 moved", "tap_prs": tapPR41})
	eventually(t, "the new head's question on the same post", func() bool {
		return slices.ContainsFunc(editsOf(l.slack.edited(), card.ts), func(e string) bool { return strings.Contains(e, "merge:41:09b10f1") })
	})
	if slices.ContainsFunc(rootCards(l.slack.sentTo(testDM)), func(p fakePost) bool { return strings.Contains(p.blocks, "merge:41:09b10f1") }) {
		t.Fatal("a new head drew a new card at the DM root, want the same card edited")
	}
}

func TestOnlyACallNamingAThreadPostsInIt(t *testing.T) {
	const hisThread = "1793804574.412309"
	l := startDaemon(t)
	loggedIn(t, l)
	l.do(opDMStatus, map[string]any{"thread_ts": hisThread, "text": "a reply naming its thread"})
	l.do(opDMStatus, cardBody("A card from the session that just posted there", "merge it"))
	l.do(opDMStatus, map[string]any{"text": "a status with no row"})
	sent := l.slack.sent()
	if len(sent) != 3 || sent[0].threadTS != hisThread || sent[1].threadTS != "" || sent[2].threadTS != "" {
		t.Fatalf("posts = %+v, want the reply in %s and the card and status at the DM root", sent, hisThread)
	}
}

func TestAMergedPRAndAPRTheBoardDoesNotHoldGetALinkAndNoCard(t *testing.T) {
	t.Run("merged", func(t *testing.T) {
		cmd := fakeCardCommand(t, []string{"acme/app#41"}, mergeRowCard())
		l, gh := cardingDaemon(t, cmd.argv, "acme/app#41")
		gh.mu.Lock()
		gh.merged["acme/app#41"] = true
		gh.mu.Unlock()
		l.do(opDMStatus, map[string]any{"text": "#41 landed", "tap_prs": tapPR41})
		if got := l.slack.sentTo(testDM); len(got) != 1 || got[0].text != "<https://github.com/acme/app/pull/41|#41> landed" || len(cmd.requests(t)) != 0 {
			t.Fatalf("posted %+v and asked %v, want the link alone and no ask", got, cmd.requests(t))
		}
	})
	t.Run("not held", func(t *testing.T) {
		cmd := fakeCardCommand(t, []string{"acme/app#41"})
		l, _ := cardingDaemon(t, cmd.argv, "acme/app#41")
		l.do(opDMStatus, map[string]any{"text": "#41 is green", "tap_prs": tapPR41})
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
	if err := json.Unmarshal(l.post(opSend, map[string]any{"channel_id": testChannel, "text": "#41 is green", "tap_prs": tapPR41}).Body, &sent); err != nil {
		t.Fatal(err)
	}
	l.post(opReply, map[string]any{"channel_id": testChannel, "thread_ts": sent.TS, "text": "#41 is green", "tap_prs": tapPR41})
	if got := l.slack.sentTo(testChannel); len(got) != 2 || len(cmd.requests(t)) != 0 {
		t.Fatalf("unlisted channel got %+v after %d asks, want two linked posts, no card and no ask, even in a thread we posted", got, len(cmd.requests(t)))
	}
	var room postResult
	if err := json.Unmarshal(l.post(opSend, map[string]any{"channel_id": testRoom, "text": "#41 is green", "tap_prs": tapPR41}).Body, &room); err != nil {
		t.Fatal(err)
	}
	if cards := awaitPRCards(t, l, testRoom); len(cards) != 1 || cards[0].threadTS != room.TS {
		t.Fatalf("listed channel got cards %+v, want one in the post's thread %s", cards, room.TS)
	}
}

func TestMergeOnAPRCardGoesToTheCardCommand(t *testing.T) {
	cmd := fakeCardCommand(t, []string{"acme/app#41"}, mergeRowCard())
	l, _ := cardingDaemon(t, cmd.argv, "acme/app#41")
	l.do(opDMStatus, map[string]any{"text": "#41 is green", "tap_prs": tapPR41})
	card := awaitPRCards(t, l, testDM)[0]
	id, err := loadIdentity(l.ctx, l.srv.DB())
	if err != nil {
		t.Fatal(err)
	}
	l.tap(id, cardActionPush{User: testUser, Channel: testDM, MessageTS: card.ts, ThreadTS: cmp.Or(card.threadTS, card.ts),
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

func refusingCardCommand(t *testing.T, reason string, cards ...providedCard) (cardCommand, string) {
	t.Helper()
	dir := t.TempDir()
	c := cardCommand{argv: []string{filepath.Join(dir, "card")}, asked: filepath.Join(dir, "asked"), resolved: filepath.Join(dir, "resolved")}
	answer := filepath.Join(dir, "cards.json")
	if err := os.WriteFile(answer, mustJSON(map[string]any{"cards": cards}), 0o600); err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf("#!/bin/sh\nin=$(cat)\necho \"$in\" >> '%s'\ncase \"$in\" in\n*'\"answer\"'*) echo 'card: %s' >&2; exit 1 ;;\n*'\"numbers\"'*) echo '{\"prs\":[{\"repo\":\"acme/app\",\"number\":41}]}' ;;\n*) cat '%s' ;;\nesac\n", c.asked, reason, answer)
	if err := os.WriteFile(c.argv[0], []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return c, answer
}

func tapMerge(t *testing.T, l *liveDaemon, card fakePost) identity {
	t.Helper()
	id, err := loadIdentity(l.ctx, l.srv.DB())
	if err != nil {
		t.Fatal(err)
	}
	l.tap(id, cardActionPush{User: testUser, Channel: testDM, MessageTS: card.ts, ThreadTS: cmp.Or(card.threadTS, card.ts),
		ActionID: cardWaitAction + "0:" + testWaits, Value: string(mustJSON(askTap{ID: testWaits, Label: "Merge", Question: "Merge #41 at e718a74?", Options: []string{"Merge", "Hold"}})),
		ActionTS: fmt.Sprintf("%d.000100", time.Now().Unix()), Seq: "001800000100000-abcd0301"})
	return id
}

func movedCard() providedCard {
	moved := mergeRowCard()
	moved.Card.Title, moved.Card.Waits = "Merge #41 at 09b10f1?", "merge:41:09b10f1"
	return moved
}

func TestARefusedTapSendsNoLineAndTheCardShowsWhatStandsNow(t *testing.T) {
	cmd, answer := refusingCardCommand(t, "#41 moved to 09b10f1 since the card asked merge:41:e718a74", mergeRowCard())
	l, _ := cardingDaemon(t, cmd.argv, "acme/app#41")
	l.do(opDMStatus, map[string]any{"text": "#41 is green", "tap_prs": tapPR41})
	card := awaitPRCards(t, l, testDM)[0]
	if err := os.WriteFile(answer, mustJSON(map[string]any{"cards": []providedCard{movedCard()}}), 0o600); err != nil {
		t.Fatal(err)
	}

	before := len(l.slack.sentTo(testDM))
	tapMerge(t, l, card)

	if said := l.slack.sentTo(testDM); len(said) != before {
		t.Fatalf("posts = %+v, want no line after a refused tap", said[before:])
	}
	edits := editsOf(l.slack.edited(), card.ts)
	if len(edits) == 0 || !strings.Contains(edits[len(edits)-1], "09b10f1") || !strings.Contains(edits[len(edits)-1], "merge:41:09b10f1") {
		t.Fatalf("card edits = %v, want it redrawn at the new head", edits)
	}
	if posted := rootCards(prCards(l, testDM)); len(posted) != 1 {
		t.Fatalf("PR cards = %+v, want the one card edited in place", posted)
	}
}

func TestACardForAPRThatNoLongerWaitsGivesUpItsButtons(t *testing.T) {
	cmd, answer := refusingCardCommand(t, "#41 no longer waits on his tap", mergeRowCard())
	l, _ := cardingDaemon(t, cmd.argv, "acme/app#41")
	l.do(opDMStatus, map[string]any{"text": "#41 is green", "tap_prs": tapPR41})
	card := awaitPRCards(t, l, testDM)[0]
	if err := os.WriteFile(answer, mustJSON(map[string]any{"cards": []providedCard{}}), 0o600); err != nil {
		t.Fatal(err)
	}

	tapMerge(t, l, card)

	edits := editsOf(l.slack.edited(), card.ts)
	if len(edits) == 0 || !strings.Contains(edits[len(edits)-1], cardNoLongerWaits) {
		t.Fatalf("card edits = %v, want the buttons gone", edits)
	}
}

func TestAHeadThatMovesRedrawsItsOpenCardInPlaceWithNoNewPost(t *testing.T) {
	cmd, answer := refusingCardCommand(t, "unused", mergeRowCard())
	l, _ := cardingDaemon(t, cmd.argv, "acme/app#41")
	l.do(opDMStatus, map[string]any{"text": "#41 is green", "tap_prs": tapPR41})
	card := awaitPRCards(t, l, testDM)[0]
	if err := os.WriteFile(answer, mustJSON(map[string]any{"cards": []providedCard{movedCard()}}), 0o600); err != nil {
		t.Fatal(err)
	}
	id, err := loadIdentity(l.ctx, l.srv.DB())
	if err != nil {
		t.Fatal(err)
	}
	posts := len(l.slack.sentTo(testDM))

	l.app.cards.recardMoved(l.ctx, id, map[string]bool{"pr:acme/app#40": true})
	if edits := editsOf(l.slack.edited(), card.ts); slices.ContainsFunc(edits, func(edit string) bool { return strings.Contains(edit, "09b10f1") }) {
		t.Fatalf("another PR's head redrew this card: %v", edits)
	}

	l.app.cards.recardMoved(l.ctx, id, map[string]bool{"pr:acme/app#41": true})

	edits := editsOf(l.slack.edited(), card.ts)
	if len(edits) == 0 || !strings.Contains(edits[len(edits)-1], "merge:41:09b10f1") {
		t.Fatalf("card edits = %v, want it redrawn at the new head", edits)
	}
	if got := len(l.slack.sentTo(testDM)); got != posts {
		t.Fatalf("posts went from %d to %d, want none new", posts, got)
	}
}

func TestAnAnsweredCardKeepsItsAnswerUnderTheTitleItsRowHasNow(t *testing.T) {
	cmd := fakeCardCommand(t, []string{"acme/app#41"}, mergeRowCard())
	l, _ := cardingDaemon(t, cmd.argv, "acme/app#41")
	l.do(opDMStatus, map[string]any{"text": "#41 is green", "tap_prs": tapPR41})
	card := awaitPRCards(t, l, testDM)[0]
	id := tapMerge(t, l, card)
	landed := providedCard{Repo: "acme/app", Number: 41, Card: statusCard{
		Status: "done", Title: "#41 at e718a74",
		Body: "*Fix the chat list*\n*Blocks landing:* nothing; it landed on dev",
		Refs: []cardLink{{URL: "https://github.com/acme/app/pull/41", Label: "acme/app#41"}},
	}}
	if err := os.WriteFile(filepath.Join(filepath.Dir(cmd.argv[0]), "cards.json"), mustJSON(map[string]any{"cards": []providedCard{landed}}), 0o600); err != nil {
		t.Fatal(err)
	}

	l.app.cards.recardMoved(l.ctx, id, map[string]bool{"pr:acme/app#41": true})

	edits := editsOf(l.slack.edited(), card.ts)
	if len(edits) == 0 || !strings.Contains(edits[len(edits)-1], "#41 at e718a74") || !strings.Contains(edits[len(edits)-1], "✓ *Merge* · answered") {
		t.Fatalf("card edits = %v, want the landed title with his answer kept", edits)
	}
	if last := edits[len(edits)-1]; strings.Contains(last, "your approval") || strings.Contains(last, "it landed on dev") || strings.Contains(last, cardNoLongerWaits) {
		t.Fatalf("last edit = %s, want neither the body nor the no-longer-waits line", last)
	}
}

func TestACardNobodyAnsweredGivesUpItsButtonsUnderItsRowsTitle(t *testing.T) {
	cmd, answer := refusingCardCommand(t, "unused", mergeRowCard())
	l, _ := cardingDaemon(t, cmd.argv, "acme/app#41")
	l.do(opDMStatus, map[string]any{"text": "#41 is green", "tap_prs": tapPR41})
	card := awaitPRCards(t, l, testDM)[0]
	red := providedCard{Repo: "acme/app", Number: 41, Card: statusCard{
		Status: "info", Title: "#41 at e718a74",
		Body: "*Fix the chat list*\n*Blocks landing:* CI is red at this head",
		Refs: []cardLink{{URL: "https://github.com/acme/app/pull/41", Label: "acme/app#41"}},
	}}
	if err := os.WriteFile(answer, mustJSON(map[string]any{"cards": []providedCard{red}}), 0o600); err != nil {
		t.Fatal(err)
	}
	id, err := loadIdentity(l.ctx, l.srv.DB())
	if err != nil {
		t.Fatal(err)
	}

	l.app.cards.recardMoved(l.ctx, id, map[string]bool{"pr:acme/app#41": true})

	edits := editsOf(l.slack.edited(), card.ts)
	if len(edits) == 0 || !strings.Contains(edits[len(edits)-1], "#41 at e718a74") || !strings.Contains(edits[len(edits)-1], cardNoLongerWaits) ||
		strings.Contains(edits[len(edits)-1], cardWaitAction) || strings.Contains(edits[len(edits)-1], "CI is red at this head") {
		t.Fatalf("card edits = %v, want the row's title with its body and buttons gone", edits)
	}
}

func TestOnlyAKnownHeadThatChangesCountsAsMoved(t *testing.T) {
	for _, tc := range []struct {
		prior, next string
		want        bool
	}{{"", "a", false}, {"a", "", false}, {"a", "a", false}, {"a", "b", true}} {
		if got := (refState{Head: tc.next}).movedFrom(refState{Head: tc.prior}); got != tc.want {
			t.Errorf("%q to %q moved = %v, want %v", tc.prior, tc.next, got, tc.want)
		}
	}
}

func TestRedrawPRCardsRedrawsEveryOpenCardWithNoNewPost(t *testing.T) {
	cmd, answer := refusingCardCommand(t, "unused", mergeRowCard())
	l, _ := cardingDaemon(t, cmd.argv, "acme/app#41")
	l.do(opDMStatus, map[string]any{"text": "#41 is green", "tap_prs": tapPR41})
	card := awaitPRCards(t, l, testDM)[0]
	full := mergeRowCard()
	full.Card.Body += "\n*From the ledger:* the whole row"
	if err := os.WriteFile(answer, mustJSON(map[string]any{"cards": []providedCard{full}}), 0o600); err != nil {
		t.Fatal(err)
	}
	posts := len(l.slack.sentTo(testDM))

	l.do(opRedrawPRCards, map[string]any{})

	eventually(t, "the redraw", func() bool {
		edits := editsOf(l.slack.edited(), card.ts)
		return len(edits) > 0 && strings.Contains(edits[len(edits)-1], "From the ledger")
	})
	if got := len(l.slack.sentTo(testDM)); got != posts {
		t.Fatalf("posts went from %d to %d, want none new", posts, got)
	}
}

func TestRedrawPRCardsCanTakeOnePR(t *testing.T) {
	cmd, answer := refusingCardCommand(t, "unused", mergeRowCard())
	l, _ := cardingDaemon(t, cmd.argv, "acme/app#41")
	l.do(opDMStatus, map[string]any{"text": "#41 is green", "tap_prs": tapPR41})
	card := awaitPRCards(t, l, testDM)[0]
	full := mergeRowCard()
	full.Card.Body += "\n*From the ledger:* the whole row"
	if err := os.WriteFile(answer, mustJSON(map[string]any{"cards": []providedCard{full}}), 0o600); err != nil {
		t.Fatal(err)
	}
	redrawing := func(pr string) int {
		var got map[string]int
		if err := json.Unmarshal(l.do(opRedrawPRCards, map[string]any{"pr": pr}).Body, &got); err != nil {
			t.Fatal(err)
		}
		return got["redrawing"]
	}
	if got := redrawing("https://github.com/acme/app/pull/42"); got != 0 {
		t.Fatalf("another PR redraws %d cards, want none", got)
	}
	if got := redrawing(tapPR41[0]); got != 1 {
		t.Fatalf("its own PR redraws %d cards, want one", got)
	}
	eventually(t, "the redraw", func() bool {
		edits := editsOf(l.slack.edited(), card.ts)
		return len(edits) > 0 && strings.Contains(edits[len(edits)-1], "From the ledger")
	})
	l.refused(opRedrawPRCards, map[string]any{"pr": "not a link"}, "pull request URL")
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
	gh.mergedAfter = 2
	gh.mu.Unlock()
	l.do(opDMStatus, map[string]any{"text": "#41 is green", "tap_prs": tapPR41})
	card := awaitPRCards(t, l, testDM)[0]
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
			if got := cmd.requests(t); len(got) != 0 {
				t.Fatalf("card command asked %v, want no card from a link", got)
			}
		})
	}
}

func headRowCard() providedCard {
	card := mergeRowCard()
	card.Card.Refs, card.Card.PRs = nil, []cardPR{{URL: tapPR41[0], Head: "e718a74"}}
	return card
}

func pickFor41(head string, options ...string) map[string]any {
	return map[string]any{"card": statusCard{Status: "needs_you", Title: "Tap: #41", Body: "Fixes the chat list.", Options: options, Recommended: options[0], Waits: testPick,
		PRs: []cardPR{{URL: tapPR41[0], Title: "Fix the chat list", Description: "Chats load faster.", Head: head}}}}
}

func postPick(t *testing.T, l *liveDaemon, body map[string]any) fakePost {
	t.Helper()
	var res map[string]string
	if err := json.Unmarshal(l.do(opDMStatus, body).Body, &res); err != nil {
		t.Fatal(err)
	}
	return fakePost{ts: res["ts"], threadTS: res["thread_ts"]}
}

func tapMergeOnPick(t *testing.T, l *liveDaemon, card fakePost) {
	t.Helper()
	id, err := loadIdentity(l.ctx, l.srv.DB())
	if err != nil {
		t.Fatal(err)
	}
	l.tap(id, cardActionPush{User: testUser, Channel: testDM, MessageTS: card.ts, ThreadTS: cmp.Or(card.threadTS, card.ts),
		ActionID: cardWaitAction + "0:" + testPick, Value: string(mustJSON(askTap{ID: testPick, Label: "Merge", Question: "Tap: #41", Options: []string{"Merge", "Hold"}})),
		ActionTS: fmt.Sprintf("%d.000100", time.Now().Unix()), Seq: "001800000100000-abcd0" + card.ts[len(card.ts)-3:]})
}

func seedThreadCard(l *liveDaemon, thread, ts string, card statusCard) {
	l.app.cards.mu.Lock()
	defer l.app.cards.mu.Unlock()
	l.app.cards.threads[threadKey(testDM, thread)] = &liveThread{Channel: testDM, Thread: thread, Updated: time.Now(),
		PRCards: []*liveMessage{{TS: ts, Card: card, At: time.Now()}}}
}

func answersOf(requests []string) []string {
	return slices.DeleteFunc(requests, func(r string) bool { return !strings.Contains(r, `"answer"`) })
}

func TestALinkInAPostDrawsNoTapCardAndKeepsItsLink(t *testing.T) {
	cmd := fakeCardCommand(t, []string{"acme/app#41"}, mergeRowCard())
	l, _ := cardingDaemon(t, cmd.argv, "acme/app#41")
	l.do(opDMStatus, map[string]any{"text": "#41 and " + tapPR41[0] + " are green"})
	l.do(opDMStatus, map[string]any{"card": statusCard{Status: "info", Title: "#41 is green", Body: tapPR41[0]}})
	l.post(opSend, map[string]any{"channel_id": testRoom, "text": "#41 again"})
	if asked := cmd.requests(t); len(asked) != 0 {
		t.Fatalf("card command asked %v, want no ask from a link", asked)
	}
	first := l.slack.sentTo(testDM)[0]
	if !strings.Contains(first.text, "<https://github.com/acme/app/pull/41|#41>") {
		t.Fatalf("post = %q, want the PR still linked", first.text)
	}
	l.do(opDMStatus, map[string]any{"text": "#41 waits on your tap", "tap_prs": tapPR41})
	if cards := rootCards(awaitPRCards(t, l, testDM)); len(cards) != 1 {
		t.Fatalf("cards = %+v, want the one card tap_prs asked for", cards)
	}
}

func TestTapPRsTakesOnlyPullRequestURLs(t *testing.T) {
	cmd := fakeCardCommand(t, []string{"acme/app#41"}, mergeRowCard())
	l, _ := cardingDaemon(t, cmd.argv, "acme/app#41")
	raw := mustJSON(map[string]any{"text": "not a PR", "tap_prs": []string{"#41"}})
	if _, err := invoke(l.ctx, l.deps, testSession, "/any", opDMStatus, raw); err == nil || !strings.Contains(err.Error(), "tap_prs takes pull request URLs") {
		t.Fatalf("a bare number in tap_prs: %v", err)
	}
}

func TestATapClosesEveryOtherCardOfThatPRAndHeadAndAppliesOnce(t *testing.T) {
	const oldThread, oldCard, otherHead = "1793000000.000100", "1793000000.000200", "1793000000.000300"
	cmd := fakeCardCommand(t, []string{"acme/app#41"}, mergeRowCard())
	l, _ := cardingDaemon(t, cmd.argv, "acme/app#41")
	l.do(opDMStatus, map[string]any{"text": "#41 is green", "tap_prs": tapPR41})
	card := awaitPRCards(t, l, testDM)[0]
	seedThreadCard(l, oldThread, oldCard, mergeRowCard().Card)
	seedThreadCard(l, oldThread+"9", otherHead, movedCard().Card)
	tapMerge(t, l, card)
	if answers := answersOf(cmd.requests(t)); len(answers) != 1 {
		t.Fatalf("card command answers = %v, want the one tap applied once", answers)
	}
	if edits := editsOf(l.slack.edited(), oldCard); len(edits) == 0 || !strings.Contains(edits[len(edits)-1], "✓ *Merge* · answered") || !strings.Contains(edits[len(edits)-1], "the other card") {
		t.Fatalf("the open copy's edits = %v, want the answer and a link to the card he tapped", edits)
	}
	if edits := editsOf(l.slack.edited(), otherHead); len(edits) != 0 {
		t.Fatalf("the card at another head was edited: %v", edits)
	}
	if _, done := l.app.cards.answer(testDM, oldThread, testWaits); !done {
		t.Fatal("the copy's own thread does not hold the answer, so a late tap on it would apply again")
	}
}

func TestATapOnAPickForThatPRAndHeadClosesItsCardAndCountsInTheLedger(t *testing.T) {
	cmd := fakeCardCommand(t, []string{"acme/app#41"}, headRowCard())
	l, _ := cardingDaemon(t, cmd.argv, "acme/app#41")
	l.do(opDMStatus, map[string]any{"text": "#41 is green", "tap_prs": tapPR41})
	card := awaitPRCards(t, l, testDM)[0]
	pick := postPick(t, l, pickFor41("e718a74", "Merge", "Hold"))
	tapMergeOnPick(t, l, pick)
	want := `{"answer":{"label":"Merge","number":41,"repo":"acme/app","waits":"merge:41:e718a74"}}`
	if answers := answersOf(cmd.requests(t)); len(answers) != 1 || answers[0] != want {
		t.Fatalf("card command answers = %v, want %s", answers, want)
	}
	if edits := editsOf(l.slack.edited(), card.ts); len(edits) == 0 || !strings.Contains(edits[len(edits)-1], "✓ *Merge* · answered") {
		t.Fatalf("the PR card's edits = %v, want the answer from the pick", edits)
	}
}

func TestATapOnThePRCardAnswersItsPickAndTellsTheLaneWaitingOnIt(t *testing.T) {
	cmd := fakeCardCommand(t, []string{"acme/app#41"}, headRowCard())
	l, _ := cardingDaemon(t, cmd.argv, "acme/app#41")
	l.do(opDMStatus, map[string]any{"text": "#41 is green", "tap_prs": tapPR41})
	card := awaitPRCards(t, l, testDM)[0]
	pick := postPick(t, l, pickFor41("e718a74", "Merge", "Hold"))
	tapMerge(t, l, card)
	if edits := editsOf(l.slack.edited(), pick.ts); len(edits) == 0 || !strings.Contains(edits[len(edits)-1], "✓ *Merge* · answered") {
		t.Fatalf("the pick's edits = %v, want the answer from the PR card", edits)
	}
	if got := l.inject(agent.TopLevel); !strings.Contains(got, "Merge") {
		t.Fatalf("the waiting lane heard %q, want the pick's answer", got)
	}
}

func TestATapRedrawsItsCardWithinASecondWhileGitHubAndTheCardCommandStall(t *testing.T) {
	cmd := fakeCardCommand(t, []string{"acme/app#41"}, mergeRowCard())
	l, gh := cardingDaemon(t, cmd.argv, "acme/app#41")
	l.do(opDMStatus, map[string]any{"text": "#41 is green", "tap_prs": tapPR41})
	card := awaitPRCards(t, l, testDM)[0]
	docs := statusCard{Status: "needs_you", Title: "Ship the docs?", Body: "The docs are ready.", Options: []string{"Ship", "Hold"}, Recommended: "Ship", Waits: "card-docs"}
	l.do(opDMStatus, map[string]any{"card": docs})
	other := sentWith(t, l, testDM, docs.Waits)

	released := filepath.Join(t.TempDir(), "released")
	stalled := fmt.Sprintf("#!/bin/sh\ncat >/dev/null\nwhile [ ! -e '%s' ]; do sleep 0.05; done\n", released)
	if err := os.WriteFile(cmd.argv[0], []byte(stalled), 0o700); err != nil {
		t.Fatal(err)
	}
	stall := make(chan struct{})
	gh.mu.Lock()
	gh.stall = stall
	gh.mu.Unlock()
	posting := make(chan struct{})
	go func() {
		defer close(posting)
		_, _ = invoke(l.ctx, l.deps, testSession, "/any", opDMStatus, mustJSON(map[string]any{"text": "acme/app#41 is still green"}))
	}()
	t.Cleanup(func() {
		close(stall)
		if err := os.WriteFile(released, nil, 0o600); err != nil {
			t.Error(err)
		}
		<-posting
		l.app.poller.taps.busy.Wait()
	})
	eventually(t, "GitHub holding the post's PR lookup", func() bool {
		gh.mu.Lock()
		defer gh.mu.Unlock()
		return gh.stalled > 0
	})

	id, err := loadIdentity(l.ctx, l.srv.DB())
	if err != nil {
		t.Fatal(err)
	}
	l.push(id, cardActionPush{User: testUser, Channel: testDM, MessageTS: card.ts, ThreadTS: cmp.Or(card.threadTS, card.ts),
		ActionID: cardWaitAction + "0:" + testWaits, Value: string(mustJSON(askTap{ID: testWaits, Label: "Merge", Question: "Merge #41 at e718a74?", Options: []string{"Merge", "Hold"}})),
		ActionTS: fmt.Sprintf("%d.000100", time.Now().Unix()), Seq: "001800000100000-abcd0401"})
	within(t, time.Second, "the PR card drawing his Merge", func() bool { return answered(l, card.ts, "Merge") })
	l.push(id, cardActionPush{User: testUser, Channel: testDM, MessageTS: other.ts, ThreadTS: cmp.Or(other.threadTS, other.ts),
		ActionID: cardWaitAction + "0:" + docs.Waits, Value: string(mustJSON(askTap{ID: docs.Waits, Label: "Ship", Question: docs.Title, Options: docs.Options})),
		ActionTS: fmt.Sprintf("%d.000200", time.Now().Unix()), Seq: "001800000100000-abcd0402"})
	within(t, time.Second, "the other card drawing his Ship", func() bool { return answered(l, other.ts, "Ship") })
}

func sentWith(t *testing.T, l *liveDaemon, channelID, text string) fakePost {
	t.Helper()
	for _, p := range l.slack.sentTo(channelID) {
		if strings.Contains(p.blocks, text) {
			return p
		}
	}
	t.Fatalf("no post in %s carries %q", channelID, text)
	return fakePost{}
}

func answered(l *liveDaemon, ts, label string) bool {
	edits := editsOf(l.slack.edited(), ts)
	return len(edits) > 0 && strings.Contains(edits[len(edits)-1], "✓ *"+label+"* · answered")
}

func within(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s took longer than %s", what, d)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestAPickSettledByWhatHappenedWritesNoLedgerAnswer(t *testing.T) {
	cmd := fakeCardCommand(t, []string{"acme/app#41"}, headRowCard())
	l, _ := cardingDaemon(t, cmd.argv, "acme/app#41")
	l.do(opDMStatus, map[string]any{"text": "#41 is green", "tap_prs": tapPR41})
	card := awaitPRCards(t, l, testDM)[0]
	pick := postPick(t, l, pickFor41("e718a74", "Merge", "Hold"))
	id, err := loadIdentity(l.ctx, l.srv.DB())
	if err != nil {
		t.Fatal(err)
	}
	l.do(opCardAnswer, map[string]any{"waits": testPick, "url": id.permalink(testDM, pick.ts, pick.threadTS), "label": "It landed", "other": true, "via": "http://127.0.0.1:7000/p/asks--ab12", "settled": true})
	if answers := answersOf(cmd.requests(t)); len(answers) != 0 {
		t.Fatalf("card command answers = %v, want none from a settled pick", answers)
	}
	if edits := editsOf(l.slack.edited(), card.ts); len(edits) != 0 {
		t.Fatalf("the PR card was edited: %v", edits)
	}
}

func TestAPickThatIsNotTheSameOfferLeavesTheCardOpen(t *testing.T) {
	for name, pick := range map[string]map[string]any{
		"another head":  pickFor41("09b10f1", "Merge", "Hold"),
		"other answers": pickFor41("e718a74", "Accept the numbers", "Keep the bar"),
		"no head":       pickFor41("", "Merge", "Hold"),
		"only names it": {"card": statusCard{Status: "needs_you", Title: "Tap all ready fixes?", Body: "#41 is one of them.", Options: []string{"Merge", "Hold"}, Recommended: "Merge", Waits: testPick}},
	} {
		t.Run(name, func(t *testing.T) {
			cmd := fakeCardCommand(t, []string{"acme/app#41"}, headRowCard())
			l, _ := cardingDaemon(t, cmd.argv, "acme/app#41")
			l.do(opDMStatus, map[string]any{"text": "#41 is green", "tap_prs": tapPR41})
			card := awaitPRCards(t, l, testDM)[0]
			posted := postPick(t, l, pick)
			tapMergeOnPick(t, l, posted)
			if answers := answersOf(cmd.requests(t)); len(answers) != 0 {
				t.Fatalf("card command answers = %v, want none", answers)
			}
			if edits := editsOf(l.slack.edited(), card.ts); len(edits) != 0 {
				t.Fatalf("the PR card was edited: %v", edits)
			}
		})
	}
}

func TestAnOpenPickForThatPRAndHeadDrawsNoSecondCard(t *testing.T) {
	cmd := fakeCardCommand(t, []string{"acme/app#41"}, headRowCard())
	l, _ := cardingDaemon(t, cmd.argv, "acme/app#41")
	pick := postPick(t, l, pickFor41("e718a74", "Merge", "Hold"))
	l.do(opDMStatus, map[string]any{"text": "#41 is green", "tap_prs": tapPR41})
	eventually(t, "the ask", func() bool { return len(cmd.requests(t)) == 1 })
	if cards := prCards(l, testDM); len(cards) != 0 || len(cardCopies(l, pick)) != 1 || len(l.slack.sentTo(testDM)) != 3 {
		t.Fatalf("cards = %+v in %d posts, want the pick as the one offer and its working copy under the post", cards, len(l.slack.sentTo(testDM)))
	}
}

func TestAPickAskingAboutAPRDrawsNoButtonlessCardBesideIt(t *testing.T) {
	held := heldRowCard()
	held.Card.Refs, held.Card.PRs = nil, []cardPR{{URL: tapPR41[0], Head: "e718a74"}}
	cmd := fakeCardCommand(t, []string{"acme/app#41"}, held)
	l, _ := cardingDaemon(t, cmd.argv, "acme/app#41")
	pick := postPick(t, l, pickFor41("e718a74", "Merge", "Hold"))
	l.do(opDMStatus, map[string]any{"text": "#41 is held", "tap_prs": tapPR41})
	if drawn := slices.DeleteFunc(l.slack.sentTo(testDM), func(p fakePost) bool { return !strings.Contains(p.blocks, "#41 at e718a74") }); len(drawn) != 0 {
		t.Fatalf("posts = %+v, want no card for #41 without buttons while the pick asks about it", drawn)
	}
	if copies := cardCopies(l, pick); len(copies) != 1 {
		t.Fatalf("copies of the pick = %+v, want its working copy under the post", copies)
	}
}

func TestACardAtTheDMRootCarriesEachPRCardInItselfOnce(t *testing.T) {
	cmd := fakeCardCommand(t, []string{"acme/app#41", "acme/app#42"}, mergeRowCard())
	l, _ := cardingDaemon(t, cmd.argv, "acme/app#41", "acme/app#42")
	list := map[string]any{"card": statusCard{Status: "needs_you", Title: "PR approvals waiting on you", Body: "Both wait on your tap.", Key: "approvals"}, "tap_prs": tapPR41}
	var sent map[string]string
	if err := json.Unmarshal(l.do(opDMStatus, list).Body, &sent); err != nil {
		t.Fatal(err)
	}
	card41 := rootCards(prCards(l, testDM))[0]
	notice := func() fakePost {
		edits := editsOf(l.slack.edited(), sent["ts"])
		return fakePost{ts: sent["ts"], threadTS: sent["ts"], blocks: edits[len(edits)-1]}
	}
	carriedOf := func(card fakePost) []*slack.ButtonBlockElement {
		return slices.DeleteFunc(copiedButtonsOf(t, notice()), func(b *slack.ButtonBlockElement) bool {
			return !strings.HasPrefix(b.ActionID, cardCopyAction+card.ts+":")
		})
	}
	if buttons := carriedOf(card41); len(buttons) != 1 || buttons[0].Text.Text != "Merge (Recommended)" || strings.Contains(sent["warning"], "NO APPROVE") {
		t.Fatalf("notice blocks = %s, warning %q, want #41's Merge in the notice itself and no warning", notice().blocks, sent["warning"])
	}
	if beside := slices.DeleteFunc(l.slack.sentTo(testDM), func(p fakePost) bool { return p.threadTS != sent["ts"] }); len(beside) != 0 {
		t.Fatalf("posts in the notice's thread = %+v, want none: the notice is the card to tap", beside)
	}

	card42 := mergeRowCard()
	card42.Number, card42.Card.Title, card42.Card.Waits = 42, "Merge #42 at 5c1d2e3?", "merge:42:5c1d2e3"
	card42.Card.Refs = []cardLink{{URL: "https://github.com/acme/app/pull/42", Label: "acme/app#42"}}
	if err := os.WriteFile(filepath.Join(filepath.Dir(cmd.argv[0]), "cards.json"), mustJSON(map[string]any{"cards": []providedCard{card42}}), 0o600); err != nil {
		t.Fatal(err)
	}
	list["tap_prs"] = []string{tapPR41[0], "https://github.com/acme/app/pull/42"}
	l.do(opDMStatus, list)
	at := slices.IndexFunc(l.slack.sentTo(testDM), func(p fakePost) bool { return strings.Contains(p.blocks, "merge:42:5c1d2e3") })
	if at < 0 {
		t.Fatal("no card for the PR the edit adds")
	}
	if got41, got42 := carriedOf(card41), carriedOf(l.slack.sentTo(testDM)[at]); len(got41) != 1 || len(got42) != 1 {
		t.Fatalf("notice blocks = %s, want each PR's answers in it once", notice().blocks)
	}

	id, err := loadIdentity(l.ctx, l.srv.DB())
	if err != nil {
		t.Fatal(err)
	}
	merge := carriedOf(card41)[0]
	l.tap(id, cardActionPush{User: testUser, Channel: testDM, MessageTS: sent["ts"], ThreadTS: sent["ts"],
		ActionID: merge.ActionID, Value: merge.Value, ActionTS: fmt.Sprintf("%d.000100", time.Now().Unix()), Seq: "001800000100000-abcd0302"})

	if edits := editsOf(l.slack.edited(), card41.ts); len(edits) == 0 || !strings.Contains(edits[len(edits)-1], "*Merge*") {
		t.Fatalf("card edits = %v, want the tap in the notice answering #41's card", edits)
	}
	if left := carriedOf(card41); len(left) != 0 || !strings.Contains(notice().blocks, "*Merge*") {
		t.Fatalf("notice blocks = %s, want #41's answer in place of its buttons", notice().blocks)
	}
	if got := cmd.requests(t); !slices.ContainsFunc(got, func(r string) bool { return strings.Contains(r, `"answer"`) }) {
		t.Fatalf("card command requests = %v, want the answer recorded through it", got)
	}
}

func TestANoticeCarryingManyPRCardsKeepsEachMessageWithinSlacksBlocksAndEveryApprove(t *testing.T) {
	for _, count := range []int{8, 30, 60} {
		t.Run(fmt.Sprintf("%d PRs", count), func(t *testing.T) {
			var cards []providedCard
			var drive, taps []string
			for n := 41; n < 41+count; n++ {
				card, url := mergeRowCard(), fmt.Sprintf("https://github.com/acme/app/pull/%d", n)
				card.Number, card.Card.Title, card.Card.Waits = n, fmt.Sprintf("Merge #%d at e718a74?", n), fmt.Sprintf("merge:%d:e718a74", n)
				card.Card.Refs = []cardLink{{URL: url, Label: fmt.Sprintf("acme/app#%d", n)}}
				cards, drive, taps = append(cards, card), append(drive, fmt.Sprintf("acme/app#%d", n)), append(taps, url)
			}
			cmd := fakeCardCommand(t, drive, cards...)
			l, _ := cardingDaemon(t, cmd.argv, drive...)
			var sent map[string]string
			list := map[string]any{"card": statusCard{Status: "needs_you", Title: "PR approvals waiting on you", Body: "Each waits on your tap.", Key: "approvals"}, "tap_prs": taps}
			if err := json.Unmarshal(l.do(opDMStatus, list).Body, &sent); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(sent["warning"], "NO APPROVE") {
				t.Fatalf("warning = %q, want every PR's Approve carried", sent["warning"])
			}
			carriers := []string{sent["ts"]}
			for _, p := range l.slack.sentTo(testDM) {
				if p.threadTS == sent["ts"] && p.text == carriedMoreText {
					carriers = append(carriers, p.ts)
				}
			}
			if continued := len(carriers) > 1; continued != (count > cardMaxBlocks) {
				t.Fatalf("%d PRs drew %d messages, want a continuation in the thread only past %d rows", count, len(carriers), cardMaxBlocks)
			}
			var carried []slack.Block
			for _, ts := range carriers {
				message, err := messageAt(l.ctx, l.slack, testDM, sent["ts"], ts)
				if err != nil {
					t.Fatal(err)
				}
				if held := len(message.Blocks.BlockSet); held > cardMaxBlocks {
					t.Fatalf("message %s holds %d blocks, past Slack's %d", ts, held, cardMaxBlocks)
				}
				carried = append(carried, message.Blocks.BlockSet...)
			}
			posts := l.slack.sentTo(testDM)
			for n := 41; n < 41+count; n++ {
				waits := fmt.Sprintf("merge:%d:e718a74", n)
				at := slices.IndexFunc(posts, func(p fakePost) bool {
					return p.threadTS == "" && p.ts != sent["ts"] && strings.Contains(p.blocks, waits)
				})
				if at < 0 {
					t.Fatalf("no card for #%d", n)
				}
				if !approvesOn(carried, posts[at].ts) {
					t.Fatalf("#%d has no button in the notice or its thread", n)
				}
			}
		})
	}
}

func TestATapPostWhoseCardNeverDrawsWarnsNoApprove(t *testing.T) {
	cmd := fakeCardCommand(t, []string{"acme/app#41"})
	l, _ := cardingDaemon(t, cmd.argv, "acme/app#41")
	var sent map[string]string
	if err := json.Unmarshal(l.do(opDMStatus, map[string]any{"card": statusCard{Status: "needs_you", Title: "#41 waits on you", Body: "Tap it.", Key: "approvals"}, "tap_prs": tapPR41}).Body, &sent); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(sent["warning"], "NO APPROVE") || !strings.Contains(sent["warning"], tapPR41[0]) {
		t.Fatalf("warning = %q, want NO APPROVE naming %s", sent["warning"], tapPR41[0])
	}
}

func cardCopies(l *liveDaemon, card fakePost) []fakePost {
	posts := l.slack.sentTo(testDM)
	header := headerOf(posts[slices.IndexFunc(posts, func(p fakePost) bool { return p.ts == card.ts })].blocks)
	return slices.DeleteFunc(posts, func(p fakePost) bool {
		return p.threadTS == "" || p.text != cardCopyLead || headerOf(p.blocks) != header
	})
}

func headerOf(raw string) string {
	var blocks slack.Blocks
	if json.Unmarshal([]byte(raw), &blocks) != nil {
		return ""
	}
	for _, block := range blocks.BlockSet {
		if h, ok := block.(*slack.HeaderBlock); ok {
			return h.Text.Text
		}
	}
	return ""
}

func TestAPostedPRCardTellsTheCommandItStandsAndAFailedOneDoesNot(t *testing.T) {
	cmd := fakeCardCommand(t, []string{"acme/app#41"}, mergeRowCard())
	l, _ := cardingDaemon(t, cmd.argv, "acme/app#41")
	l.slack.fail("chat.postMessage", nil)
	l.slack.fail("chat.postMessage", slack.SlackErrorResponse{Err: "fatal_error"})
	l.do(opDMStatus, map[string]any{"text": "#41 is green", "tap_prs": tapPR41})
	eventually(t, "the card command asked for the card", func() bool { return len(cmd.requests(t)) > 0 })
	eventually(t, "the failed post", func() bool { return l.slack.callCount("chat.postMessage") >= 2 })
	if cards := prCards(l, testDM); len(cards) != 0 {
		t.Fatalf("cards = %+v, want the failed post to leave none", cards)
	}
	l.do(opDMStatus, map[string]any{"text": "#41 again", "tap_prs": tapPR41})
	if cards := rootCards(awaitPRCards(t, l, testDM)); len(cards) != 1 {
		t.Fatalf("cards = %+v, want one once Slack takes it", cards)
	}
	eventually(t, "the card command told the card stands", func() bool { return len(cmd.shown(t)) > 0 })
	shown := cmd.shown(t)
	if len(shown) != 1 || !strings.Contains(shown[0], `"waits":"`+testWaits+`"`) || !strings.Contains(shown[0], `"number":41`) || !strings.Contains(shown[0], `"repo":"acme/app"`) {
		t.Fatalf("shown = %v, want the one posted card, none for the failed post", shown)
	}
}
