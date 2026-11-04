package main

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/slack-go/slack"
)

const testPR = "https://github.com/acme/app/pull/41"

type fakeRows struct {
	argv  []string
	asked string
}

func lastPost(l *liveDaemon) fakePost {
	posts := l.slack.sent()
	return posts[len(posts)-1]
}

func fakePRRows(t *testing.T, rows ...ledgerRow) fakeRows {
	t.Helper()
	dir := t.TempDir()
	c := fakeRows{argv: []string{filepath.Join(dir, "rows")}, asked: filepath.Join(dir, "asked")}
	answer := filepath.Join(dir, "rows.json")
	if err := os.WriteFile(answer, mustJSON(append([]ledgerRow{}, rows...)), 0o600); err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf("#!/bin/sh\ncat >> '%s'\necho >> '%s'\ncat '%s'\n", c.asked, c.asked, answer)
	if err := os.WriteFile(c.argv[0], []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return c
}

func previewingDaemon(t *testing.T, rows []string, held ...string) *liveDaemon {
	t.Helper()
	l, _ := prLinksDaemon(t, map[string]any{"repos": []string{"acme/app"}, "preview_command": rows}, held...)
	return l
}

func readyRow(number int) ledgerRow {
	return ledgerRow{
		Repo: "Acme/App", Number: number, Title: fmt.Sprintf("api: fix chat list %d", number),
		ForUsers: "Chats load faster. The second sentence stays out.", WaitsOn: "Only your approval.",
		ReadyForTap: true, State: rowOpen, CI: "green", Hold: rowNoHold,
	}
}

func TestAPRPreviewSaysWhereThePRStandsInHisWords(t *testing.T) {
	waiting := readyRow(41)
	waiting.ReadyForTap, waiting.WaitsOn = false, "the change below it, #40, has not landed"
	landed := readyRow(41)
	landed.ReadyForTap, landed.State, landed.WaitsOn, landed.CI = false, rowLanded, "Nothing: it landed.", "pending"
	tapped := readyRow(41)
	tapped.State, tapped.WaitsOn = "tapped", "You approved it; it lands next."
	held := readyRow(41)
	held.ReadyForTap, held.Hold, held.WaitsOn = false, "root:hold record 12", "held, root:hold record 12"
	for _, tc := range []struct {
		name         string
		row          ledgerRow
		want, absent string
	}{
		{"ready", readyRow(41), "*Ready for your tap.* CI green.", "Waiting"},
		{"waiting", waiting, "*Waiting:* the change below it, #40, has not landed. CI green.", "Ready for your tap"},
		{"landed", landed, "*Landed.*", "CI pending"},
		{"tapped", tapped, "*You approved it; it lands next.* CI green.", "Waiting"},
		{"held", held, "*On hold.* CI green.", "record 12"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := previewingDaemon(t, fakePRRows(t, tc.row).argv, "acme/app#41")
			l.do(opDMStatus, map[string]any{"text": "See " + testPR})
			post := lastPost(l)
			for _, want := range []string{"#41 api: fix chat list 41", testPR, "Chats load faster.", tc.want} {
				if !strings.Contains(post.attachments, want) {
					t.Fatalf("attachments = %s, want %q in them", post.attachments, want)
				}
			}
			for _, absent := range []string{tc.absent, "second sentence", "Open the card"} {
				if strings.Contains(post.attachments, absent) {
					t.Fatalf("attachments = %s, want no %q in them", post.attachments, absent)
				}
			}
		})
	}
}

func TestAPRWithNoRowStaysALink(t *testing.T) {
	l := previewingDaemon(t, fakePRRows(t, readyRow(7)).argv, "acme/app#41")
	l.do(opDMStatus, map[string]any{"text": "Done.\n" + testPR})
	post := lastPost(l)
	if post.attachments != "" || !strings.Contains(post.text, "<"+testPR+"|#41>") {
		t.Fatalf("post = %+v, want the link and no preview", post)
	}
}

func TestAPRNamedTwiceDrawsOnePreviewFromOneReadAndLosesItsBareLine(t *testing.T) {
	rows := fakePRRows(t, readyRow(41))
	l := previewingDaemon(t, rows.argv, "acme/app#41")
	l.do(opDMStatus, map[string]any{"text": "Fixed in " + testPR + ", as promised.\n" + testPR})
	post := lastPost(l)
	if got := strings.Count(post.attachments, "Chats load faster"); got != 1 {
		t.Fatalf("attachments = %s, want one preview", post.attachments)
	}
	if got := post.text; got != "Fixed in <"+testPR+"|#41>, as promised." {
		t.Fatalf("text = %q, want the sentence alone", got)
	}
	if got := readAsks(t, rows.asked); len(got) != 1 {
		t.Fatalf("read the rows %d times for one post: %v", len(got), got)
	}
}

func TestABareLineForAPRWithNoRowGoesWhenTheSentenceNamesIt(t *testing.T) {
	l := previewingDaemon(t, fakePRRows(t).argv, "acme/app#41")
	l.do(opDMStatus, map[string]any{"text": "Fixed in " + testPR + ".\n<" + testPR + "|#41>"})
	if got := lastPost(l).text; got != "Fixed in <"+testPR+"|#41>." {
		t.Fatalf("text = %q, want one link", got)
	}
}

func TestALinkAloneInAPostKeepsItsText(t *testing.T) {
	l := previewingDaemon(t, fakePRRows(t, readyRow(41)).argv, "acme/app#41")
	l.do(opDMStatus, map[string]any{"text": testPR})
	post := lastPost(l)
	if !strings.Contains(post.text, testPR) || !strings.Contains(post.attachments, "Chats load faster") {
		t.Fatalf("post = %+v, want the link and its preview", post)
	}
}

func TestTheSixthPreviewIsCountedAndItsLinkStays(t *testing.T) {
	var rows []ledgerRow
	var held, lines []string
	for n := 41; n <= 46; n++ {
		rows = append(rows, readyRow(n))
		held = append(held, fmt.Sprintf("acme/app#%d", n))
		lines = append(lines, fmt.Sprintf("https://github.com/acme/app/pull/%d", n))
	}
	l := previewingDaemon(t, fakePRRows(t, rows...).argv, held...)
	l.do(opDMStatus, map[string]any{"text": "Six PRs.\n" + strings.Join(lines, "\n")})
	post := lastPost(l)
	if got := strings.Count(post.attachments, "Chats load faster"); got != linkPreviewLimit {
		t.Fatalf("drew %d previews, want %d", got, linkPreviewLimit)
	}
	if !strings.Contains(post.attachments, "+1 more not shown here") {
		t.Fatalf("attachments = %s, want the sixth counted", post.attachments)
	}
	if strings.Contains(post.text, "pull/45") || !strings.Contains(post.text, "pull/46") {
		t.Fatalf("text = %q, want only the sixth's link left", post.text)
	}
}

func TestAFailedRowReadLeavesThePostAsWritten(t *testing.T) {
	rows := fakePRRows(t)
	if err := os.WriteFile(rows.argv[0], []byte("#!/bin/sh\necho no store >&2\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	l := previewingDaemon(t, rows.argv, "acme/app#41")
	l.do(opDMStatus, map[string]any{"text": "Fixed in " + testPR + ".\n" + testPR})
	post := lastPost(l)
	if post.attachments != "" || strings.Count(post.text, testPR) != 2 {
		t.Fatalf("post = %+v, want both links and no preview", post)
	}
}

func TestPRPreviewsAreOffWithNoCommand(t *testing.T) {
	l, _ := prLinksDaemon(t, map[string]any{"repos": []string{"acme/app"}}, "acme/app#41")
	l.do(opDMStatus, map[string]any{"text": "Fixed in " + testPR + ".\n" + testPR})
	post := lastPost(l)
	if post.attachments != "" || strings.Count(post.text, testPR) != 2 {
		t.Fatalf("post = %+v, want both links and no preview", post)
	}
}

func TestOnlyAPostToTheDMReadsTheRows(t *testing.T) {
	rows := fakePRRows(t, readyRow(41))
	l := previewingDaemon(t, rows.argv, "acme/app#41")
	l.post(opSend, map[string]string{"channel_id": testChannel, "text": "See " + testPR})
	if post := lastPost(l); post.attachments != "" {
		t.Fatalf("a post outside the DM drew %s", post.attachments)
	}
	if got := readAsks(t, rows.asked); len(got) != 0 {
		t.Fatalf("a post outside the DM read the rows: %v", got)
	}
}

func TestAStatusCardCarriesItsPRPreviewInItsThreadAndLosesItsBareLine(t *testing.T) {
	l := previewingDaemon(t, fakePRRows(t, readyRow(41)).argv, "acme/app#41")
	l.do(opDMStatus, map[string]any{"card": statusCard{Status: "info", Title: "Status", Body: "Fixed.\n" + testPR}})
	post := lastPost(l)
	if post.text != linkPreviewsLead || post.threadTS == "" || !strings.Contains(post.attachments, "Chats load faster") {
		t.Fatalf("post = %+v, want the preview in the card's thread", post)
	}
	if card := l.slack.sent()[0]; strings.Contains(card.blocks, testPR) {
		t.Fatalf("card blocks = %s, want no bare link", card.blocks)
	}
}

func cardedPreviewDaemon(t *testing.T, row ledgerRow) (*liveDaemon, fakePost) {
	t.Helper()
	cards := fakeCardCommand(t, []string{"acme/app#41"}, mergeRowCard())
	l, _ := prLinksDaemon(t, map[string]any{"repos": []string{"acme/app"}, "card_command": cards.argv, "preview_command": fakePRRows(t, row).argv}, "acme/app#41")
	l.do(opDMStatus, map[string]any{"text": "First " + testPR, "tap_prs": tapPR41})
	card := awaitPRCards(t, l, testDM)[0]
	l.do(opDMStatus, map[string]any{"thread_ts": card.ts, "text": "Again " + testPR})
	return l, card
}

func TestAReadyPRWithAnOpenCardLinksIt(t *testing.T) {
	l, card := cardedPreviewDaemon(t, readyRow(41))
	id, err := loadIdentity(l.ctx, l.srv.DB())
	if err != nil {
		t.Fatal(err)
	}
	post := lastPost(l)
	var previews []slack.Attachment
	if err := json.Unmarshal([]byte(post.attachments), &previews); err != nil {
		t.Fatal(err)
	}
	if len(previews) != 1 || previews[0].Fallback != "#41 api: fix chat list 41" {
		t.Fatalf("previews = %+v, want one that names the PR where blocks cannot render", previews)
	}
	if got := strings.Join(blockTexts(previews[0].Blocks), "\n"); !strings.Contains(got, "<"+id.permalink(testDM, card.ts, card.threadTS)+"|Open the card>") {
		t.Fatalf("preview = %q, want a link to the open card", got)
	}
}

func TestAWaitingPRNeverOffersATapEvenWithAnOpenCard(t *testing.T) {
	waiting := readyRow(41)
	waiting.ReadyForTap, waiting.WaitsOn = false, "CI is running"
	l, _ := cardedPreviewDaemon(t, waiting)
	if got := lastPost(l).attachments; strings.Contains(got, "Open the card") || !strings.Contains(got, "CI is running") {
		t.Fatalf("attachments = %s, want its wait and no card to tap", got)
	}
}

func buildPreviewDaemon(t *testing.T, build, message string) *liveDaemon {
	t.Helper()
	refs, srv := serveRefs(t)
	refs.build, refs.buildMessage = build, message
	l := startDaemon(t, func(a *app) {
		a.cards.client = refClient{http: srv.Client(), githubURL: srv.URL, buildkiteURL: srv.URL, now: time.Now}
	})
	loggedInWithRefs(t, l)
	return l
}

func TestABuildLinkShowsItsStateAndWhatItCarries(t *testing.T) {
	l := buildPreviewDaemon(t, "not_run", "Release 12890\nwith a long body")
	l.do(opDMStatus, map[string]any{"text": "See " + testBuildURL + " and " + testBuildURL})
	post := lastPost(l)
	if got := strings.Count(post.attachments, "Release 12890"); got != 1 {
		t.Fatalf("attachments = %s, want one preview", post.attachments)
	}
	for _, want := range []string{"deploy build 7", testBuildURL, "*Not run.*"} {
		if !strings.Contains(post.attachments, want) {
			t.Fatalf("attachments = %s, want %q in them", post.attachments, want)
		}
	}
	if strings.Contains(post.attachments, "long body") {
		t.Fatalf("attachments = %s, want the message's first line", post.attachments)
	}
}

func TestABuildThatDoesNotReadStaysALink(t *testing.T) {
	l := buildPreviewDaemon(t, "", "")
	l.do(opDMStatus, map[string]any{"text": "See " + testBuildURL})
	if post := lastPost(l); post.attachments != "" || !strings.Contains(post.text, testBuildURL) {
		t.Fatalf("post = %+v, want the link and no preview", post)
	}
}

func TestABuildPreviewIsOffWithNoBuildkiteSource(t *testing.T) {
	l := previewingDaemon(t, fakePRRows(t).argv)
	l.do(opDMStatus, map[string]any{"text": "See " + testBuildURL})
	if post := lastPost(l); post.attachments != "" {
		t.Fatalf("attachments = %s, want no read with no [buildkite]", post.attachments)
	}
}

func TestBuildPreviewsShareTheLimitWithPRs(t *testing.T) {
	var rows []ledgerRow
	var held, lines []string
	for n := 41; n <= 45; n++ {
		rows = append(rows, readyRow(n))
		held = append(held, fmt.Sprintf("acme/app#%d", n))
		lines = append(lines, fmt.Sprintf("https://github.com/acme/app/pull/%d", n))
	}
	_, srv := serveRefs(t)
	gh := &fakeGitHub{held: map[string]bool{}, merged: map[string]bool{}}
	for _, h := range held {
		gh.held[h] = true
	}
	github := httptest.NewServer(gh)
	t.Cleanup(github.Close)
	l := startDaemon(t, func(a *app) {
		a.cards.client = refClient{http: srv.Client(), githubURL: github.URL, buildkiteURL: srv.URL, now: time.Now}
	})
	l.slack.setUser(&slack.User{ID: testUser, Name: "ym", RealName: "Yasyf Mohamedali", Profile: slack.UserProfile{
		Email: "ym@example.com", Image512: "https://avatars/ym.png", DisplayName: "yasyf",
	}})
	l.do(opLogin, map[string]any{
		"token": "xoxb-test", "email": "ym@example.com", "socket_url": "ws://127.0.0.1:1/live", "repo_root": "/repo", "capt_hook_path": testCaptHook,
		"avatar_key_id": "r2-key", "avatar_secret": "r2-secret",
		"config": map[string]any{"github": map[string]string{"token": "ghp-test"}, "buildkite": map[string]string{"token": "bk-test"}, "pr_links": map[string]any{"repos": []string{"acme/app"}, "preview_command": fakePRRows(t, rows...).argv}},
	})
	l.do(opDMStatus, map[string]any{"text": "All.\n" + strings.Join(lines, "\n") + "\n" + testBuildURL})
	post := lastPost(l)
	if got := strings.Count(post.attachments, "Chats load faster"); got != linkPreviewLimit {
		t.Fatalf("drew %d PR previews, want %d", got, linkPreviewLimit)
	}
	if strings.Contains(post.attachments, "deploy build 7") || !strings.Contains(post.attachments, "+1 more not shown here") || !strings.Contains(post.text, testBuildURL) {
		t.Fatalf("post = %+v, want the build counted and its link kept", post)
	}
}

func copyDaemon(t *testing.T, card map[string]any) (*liveDaemon, identity, string, string) {
	t.Helper()
	l := startDaemon(t)
	loggedIn(t, l)
	l.do(opDMStatus, card)
	id, err := loadIdentity(l.ctx, l.srv.DB())
	if err != nil {
		t.Fatal(err)
	}
	ts := l.slack.sent()[0].ts
	var sent map[string]string
	if err := json.Unmarshal(l.do(opDMStatus, map[string]any{"text": "Two asks."}).Body, &sent); err != nil {
		t.Fatal(err)
	}
	return l, id, ts, sent["thread_ts"]
}

func copiedButtonsOf(t *testing.T, post fakePost) []*slack.ButtonBlockElement {
	t.Helper()
	var copies []slack.Attachment
	if err := json.Unmarshal([]byte(post.attachments), &copies); err != nil {
		t.Fatalf("attachments %q: %v", post.attachments, err)
	}
	var buttons []*slack.ButtonBlockElement
	for _, copied := range copies {
		for _, block := range copied.Blocks.BlockSet {
			switch b := block.(type) {
			case *slack.ActionBlock:
				for _, element := range b.Elements.ElementSet {
					if button, isButton := element.(*slack.ButtonBlockElement); isButton {
						buttons = append(buttons, button)
					}
				}
			case *slack.SectionBlock:
				if b.Accessory != nil && b.Accessory.ButtonElement != nil {
					buttons = append(buttons, b.Accessory.ButtonElement)
				}
			}
		}
	}
	return buttons
}

func TestATapOnACopyAnswersTheCardAndEveryCopyShowsTheAnswer(t *testing.T) {
	for _, tc := range []struct {
		name   string
		option string
	}{
		{"a short option", "merge it"},
		{"a listed option", "land it with the fix and then time a real big chat first"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l, id, card, thread := copyDaemon(t, cardBody("Merge the stack?", tc.option))
			link := id.permalink(id.DMChannelID, card, "")
			var first, second postResult
			if err := json.Unmarshal(l.do(opCardCopy, map[string]any{"url": link, "thread_ts": thread}).Body, &first); err != nil {
				t.Fatal(err)
			}
			post := lastPost(l)
			if post.threadTS != thread || post.text != cardCopyLead || first.Permalink == "" || !strings.Contains(post.attachments, "Merge the stack?") {
				t.Fatalf("post = %+v, reply = %+v, want the copy in thread %s and its permalink", post, first, thread)
			}
			if err := json.Unmarshal(l.do(opCardCopy, map[string]any{"url": link, "thread_ts": thread}).Body, &second); err != nil {
				t.Fatal(err)
			}
			buttons := copiedButtonsOf(t, post)
			at := slices.IndexFunc(buttons, func(b *slack.ButtonBlockElement) bool { return strings.HasPrefix(b.ActionID, cardCopyAction) })
			if at < 0 || buttons[len(buttons)-1].URL != link {
				t.Fatalf("buttons = %+v, want the card's answer and one that opens it", buttons)
			}

			l.tap(id, cardActionPush{User: testUser, Channel: id.DMChannelID, MessageTS: post.ts, ThreadTS: thread,
				ActionID: buttons[at].ActionID, Value: buttons[at].Value, ActionTS: "1800000100.000000", Seq: "001800000100000-abcd0123"})

			if edits := editsOf(l.slack.edited(), card); len(edits) != 1 || !strings.Contains(edits[0], "*"+tc.option+"*") {
				t.Fatalf("card edits = %v, want the answer on the card", edits)
			}
			for _, copied := range []string{first.TS, second.TS} {
				var marked []fakeEdit
				for _, edit := range l.slack.edited() {
					if edit.ts == copied {
						marked = append(marked, edit)
					}
				}
				if len(marked) != 1 || !strings.Contains(marked[0].attachments, "*"+tc.option+"*") || strings.Contains(marked[0].attachments, cardCopyAction) {
					t.Fatalf("copy %s edits = %+v, want its buttons replaced by the answer", copied, marked)
				}
			}
		})
	}
}

func TestACopyIsRefusedWhenNoAnswerIsLeftToTap(t *testing.T) {
	l, id, _, thread := copyDaemon(t, map[string]any{"card": statusCard{Status: "info", Title: "Status", Body: "No question."}})
	link := id.permalink(id.DMChannelID, l.slack.sent()[0].ts, "")
	l.refused(opCardCopy, map[string]any{"url": link, "thread_ts": thread}, "no answer left")
}

func TestACopyIsRefusedOutsideTheDM(t *testing.T) {
	l, _, _, thread := copyDaemon(t, cardBody("Merge the stack?", "merge it"))
	l.refused(opCardCopy, map[string]any{"url": "https://example.slack.com/archives/C1/p1793818357021259", "thread_ts": thread}, "not a message in your DM")
}
