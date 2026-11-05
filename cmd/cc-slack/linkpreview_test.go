package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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

func entitiesOf(t *testing.T, raw string) []slack.WorkObjectEntity {
	t.Helper()
	var metadata slack.WorkObjectMetadata
	if err := json.Unmarshal([]byte(raw), &metadata); err != nil {
		t.Fatal(err)
	}
	return metadata.Entities
}

func testEntity() slack.WorkObjectEntity {
	return slack.WorkObjectEntity{
		URL: testPR, EntityType: slack.EntityTypeContentItem,
		EntityPayload: map[string]any{"attributes": map[string]any{"title": map[string]any{"text": "The doc"}, "metadata_last_modified": time.Now().Unix()}},
	}
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
		ForUsers: "Chats load faster. A second sentence stays in.", WaitsOn: "Only your approval.",
		ReadyForTap: true, State: rowOpen, CI: "green", Hold: rowNone,
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
		{"ready", readyRow(41), "Ready for your tap", "Waiting"},
		{"waiting", waiting, "the change below it, #40, has not landed", "Ready for your tap"},
		{"landed", landed, "*Landed.*", "Waiting"},
		{"tapped", tapped, "You approved it; it lands next", "Ready for your tap"},
		{"held", held, "*On hold.*", "record 12"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := previewingDaemon(t, fakePRRows(t, tc.row).argv, "acme/app#41")
			l.do(opDMStatus, map[string]any{"text": "See " + testPR})
			post := lastPost(l)
			for _, want := range []string{"#41 api: fix chat list 41", testPR, "Chats load faster. A second sentence stays in.", tc.want} {
				if !strings.Contains(post.attachments, want) {
					t.Fatalf("attachments = %s, want %q in them", post.attachments, want)
				}
			}
			for _, absent := range []string{tc.absent, "Open the card"} {
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

func TestAPreviewPastTheLimitIsCountedAndItsLinkStays(t *testing.T) {
	var rows []ledgerRow
	var held, lines []string
	for n := 41; n <= 44; n++ {
		rows = append(rows, readyRow(n))
		held = append(held, fmt.Sprintf("acme/app#%d", n))
		lines = append(lines, fmt.Sprintf("https://github.com/acme/app/pull/%d", n))
	}
	l := previewingDaemon(t, fakePRRows(t, rows...).argv, held...)
	l.do(opDMStatus, map[string]any{"text": "Four PRs.\n" + strings.Join(lines, "\n")})
	post := lastPost(l)
	if got := strings.Count(post.attachments, "Chats load faster"); got != linkPreviewLimit {
		t.Fatalf("drew %d previews, want %d", got, linkPreviewLimit)
	}
	if !strings.Contains(post.attachments, "+1 more not shown here") {
		t.Fatalf("attachments = %s, want the fourth counted", post.attachments)
	}
	if strings.Contains(post.text, "pull/43") || !strings.Contains(post.text, "pull/44") {
		t.Fatalf("text = %q, want only the fourth's link left", post.text)
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
	if post.text != linkPreviewsLead || post.threadTS == "" || !strings.Contains(post.attachments, testPR) || !strings.Contains(post.attachments, "Chats load faster") {
		t.Fatalf("post = %+v, want the preview in the card's thread", post)
	}
	if card := l.slack.sent()[0]; strings.Contains(card.blocks, testPR) {
		t.Fatalf("card blocks = %s, want no bare link", card.blocks)
	}
}

func TestACardPreviewsNoPRItDraws(t *testing.T) {
	const mixedCase = "https://github.com/Acme/App/pull/41"
	for name, card := range map[string]statusCard{
		"in its prs":  {PRs: []cardPR{{URL: mixedCase, Change: "Chats load faster"}}},
		"in its refs": {Refs: []cardLink{{URL: mixedCase}}},
	} {
		t.Run(name, func(t *testing.T) {
			rows := fakePRRows(t, readyRow(41))
			l := previewingDaemon(t, rows.argv, "acme/app#41")
			card.Status, card.Title, card.Body = "info", "Status", "Fixed.\n"+testPR
			l.do(opDMStatus, map[string]any{"card": card})
			posts := l.slack.sent()
			if len(posts) != 1 || posts[0].attachments != "" {
				t.Fatalf("posts = %+v, want the card alone", posts)
			}
			if strings.Contains(posts[0].blocks, testPR) {
				t.Fatalf("card blocks = %s, want no bare link", posts[0].blocks)
			}
			if got := readAsks(t, rows.asked); len(got) != 0 {
				t.Fatalf("read rows %v for a PR the card draws", got)
			}
		})
	}
}

func TestACardStillPreviewsALinkItDoesNotDraw(t *testing.T) {
	const other = "https://github.com/acme/app/pull/42"
	rows := fakePRRows(t, readyRow(41), readyRow(42))
	l := previewingDaemon(t, rows.argv, "acme/app#41", "acme/app#42")
	l.do(opDMStatus, map[string]any{"card": statusCard{Status: "info", Title: "Status", Body: "Fixed.\n" + testPR + "\n" + other, PRs: []cardPR{{URL: testPR, Change: "Chats load faster"}}}})
	post := lastPost(l)
	if post.text != linkPreviewsLead || !strings.Contains(post.attachments, other) || strings.Contains(post.attachments, testPR) {
		t.Fatalf("post = %+v, want a preview of the PR the card does not draw and no other", post)
	}
}

func cardedPreviewDaemon(t *testing.T, row ledgerRow) (*liveDaemon, fakePost) {
	t.Helper()
	cards := fakeCardCommand(t, []string{"acme/app#41"}, mergeRowCard())
	l, _ := prLinksDaemon(t, map[string]any{"repos": []string{"acme/app"}, "card_command": cards.argv, "preview_command": fakePRRows(t, row).argv}, "acme/app#41")
	l.do(opDMStatus, map[string]any{"text": "First " + testPR, "tap_prs": tapPR41})
	card := awaitPRCards(t, l, testDM)[0]
	awaitCardLink(t, l, card)
	l.do(opDMStatus, map[string]any{"thread_ts": card.ts, "text": "Again " + testPR})
	return l, card
}

func TestAReadyPRWithAnOpenCardLinksIt(t *testing.T) {
	l, card := cardedPreviewDaemon(t, readyRow(41))
	id, err := loadIdentity(l.ctx, l.srv.DB())
	if err != nil {
		t.Fatal(err)
	}
	got := lastPost(l).attachments
	if !strings.Contains(got, id.permalink(testDM, card.ts, card.threadTS)) || !strings.Contains(got, "Open the card") {
		t.Fatalf("attachments = %s, want a link to the open card", got)
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
	for n := 41; n <= 43; n++ {
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
	var copied slack.Blocks
	if err := json.Unmarshal([]byte(post.blocks), &copied); err != nil {
		t.Fatalf("blocks %q: %v", post.blocks, err)
	}
	var buttons []*slack.ButtonBlockElement
	for _, block := range copied.BlockSet {
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
			if post.threadTS != thread || post.text != cardCopyLead || first.Permalink == "" || !strings.Contains(post.blocks, "Merge the stack?") || post.attachments != "" {
				t.Fatalf("post = %+v, reply = %+v, want the copy in thread %s and its permalink", post, first, thread)
			}
			if err := json.Unmarshal(l.do(opCardCopy, map[string]any{"url": link, "thread_ts": thread}).Body, &second); err != nil {
				t.Fatal(err)
			}
			buttons := copiedButtonsOf(t, post)
			at := slices.IndexFunc(buttons, func(b *slack.ButtonBlockElement) bool { return strings.HasPrefix(b.ActionID, cardCopyAction) })
			if at < 0 || strings.Contains(post.blocks, "Open the card") {
				t.Fatalf("buttons = %+v, want the card's answer and no button that opens it", buttons)
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
				if len(marked) != 1 || !strings.Contains(marked[0].blocks, "*"+tc.option+"*") || strings.Contains(marked[0].blocks, cardCopyAction) {
					t.Fatalf("copy %s edits = %+v, want its buttons replaced by the answer", copied, marked)
				}
			}
		})
	}
}

func TestACopyIsTheWholeCardWithEveryParagraph(t *testing.T) {
	long := strings.Repeat("A paragraph long enough that the old copy would clip it. ", 12)
	l, id, card, thread := copyDaemon(t, map[string]any{"card": statusCard{Status: "needs_you", Title: "Merge the stack?", Body: long + "\n\nThe last paragraph.", Options: []string{"merge it", "hold it"}}})
	l.do(opCardCopy, map[string]any{"url": id.permalink(id.DMChannelID, card, ""), "thread_ts": thread})
	post := lastPost(l)
	var blocks slack.Blocks
	if err := json.Unmarshal([]byte(post.blocks), &blocks); err != nil {
		t.Fatalf("blocks %q: %v", post.blocks, err)
	}
	if _, isHeader := blocks.BlockSet[0].(*slack.HeaderBlock); !isHeader {
		t.Fatalf("first block = %T, want the card's header", blocks.BlockSet[0])
	}
	if !strings.Contains(post.blocks, "The last paragraph.") || !strings.Contains(post.blocks, strings.TrimSpace(long)) {
		t.Fatalf("blocks = %s, want every paragraph whole", post.blocks)
	}
	if len(copiedButtonsOf(t, post)) != 2 {
		t.Fatalf("blocks = %s, want both answers and nothing else", post.blocks)
	}
}

func TestACopyOfAMessageThatDrewAsTextSaysSo(t *testing.T) {
	l, id, _, thread := copyDaemon(t, map[string]any{"text": "Just words."})
	link := id.permalink(id.DMChannelID, l.slack.sent()[0].ts, "")
	l.refused(opCardCopy, map[string]any{"url": link, "thread_ts": thread}, "no card to copy")
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

func TestAPreviewKeepsAllOfWhatThePRChangesAndLinksGitHub(t *testing.T) {
	row := readyRow(41)
	row.ForUsers = strings.Repeat("Words that run on. ", 200)
	l := previewingDaemon(t, fakePRRows(t, row).argv, "acme/app#41")
	l.do(opDMStatus, map[string]any{"text": "See " + testPR})
	post := lastPost(l)
	if got := strings.Count(post.attachments, "Words that run on."); got != 200 {
		t.Fatalf("drew %d of 200 sentences: %s", got, post.attachments)
	}
	if !strings.Contains(post.attachments, testPR) || strings.Contains(post.attachments, "oncall") || strings.Contains(post.metadata, `"entities"`) {
		t.Fatalf("post = %+v, want our own blocks linking GitHub and no Work Object", post)
	}
}

func TestAPRCardDrawsInBlocksAboveItsButtonsAndNoWorkObject(t *testing.T) {
	l := previewingDaemon(t, fakePRRows(t).argv, "acme/app#41")
	long := strings.Repeat("A sentence of the description. ", 200)
	card := statusCard{Status: "needs_you", Title: "Approve?", Options: []string{"Approve", "Hold"}, Recommended: "Approve", Waits: "m41", PRs: []cardPR{{URL: testPR, Change: "Chats load faster", Description: long}}}
	l.do(opDMStatus, map[string]any{"card": card})
	post := l.slack.sent()[0]
	if strings.Contains(post.metadata, `"entities"`) || post.attachments != "" {
		t.Fatalf("metadata = %s, attachments = %s, want the PR in the blocks alone", post.metadata, post.attachments)
	}
	at, buttons := strings.Index(post.blocks, "What changes"), strings.Index(post.blocks, cardExplainAction+"m41")
	if at < 0 || buttons < 0 || at > buttons {
		t.Fatalf("blocks = %s, want the PR above its buttons", post.blocks)
	}
	if strings.Contains(post.blocks, "A sentence of the description.") {
		t.Fatalf("blocks = %s, want the description behind Explain more", post.blocks)
	}
	if !strings.Contains(post.blocks, testPR) {
		t.Fatalf("blocks = %s, want the PR's link on GitHub", post.blocks)
	}
}

func TestAPostNamingAPRAndADocCarriesTheDocCardAndThePRPreview(t *testing.T) {
	rows := fakePRRows(t, readyRow(41))
	l := previewingDaemon(t, rows.argv, "acme/app#41")
	doc := slack.WorkObjectEntity{URL: "https://docs.example.com/x", EntityType: slack.EntityTypeContentItem}
	l.slack.cards = []slack.WorkObjectEntity{doc}
	id, err := loadIdentity(l.ctx, l.srv.DB())
	if err != nil {
		t.Fatal(err)
	}
	id.Config.DocCards = docCardsConfig{Link: "https://docs.example.com/", URL: testDocCardsURL}
	if err := saveIdentity(l.ctx, l.srv.DB(), id); err != nil {
		t.Fatal(err)
	}
	l.do(opDMStatus, map[string]any{"text": "Read https://docs.example.com/x and " + testPR})
	post := lastPost(l)
	entities := entitiesOf(t, post.metadata)
	if len(entities) != 1 || entities[0].URL != doc.URL {
		t.Fatalf("metadata = %s, want the doc's card alone", post.metadata)
	}
	if !strings.Contains(post.attachments, "Chats load faster") {
		t.Fatalf("attachments = %s, want the PR's preview", post.attachments)
	}
}

func TestAThreadReadNamesEachMessagesButtonsAndPrintsItRaw(t *testing.T) {
	l := previewingDaemon(t, fakePRRows(t, readyRow(41)).argv, "acme/app#41")
	l.do(opDMStatus, map[string]any{"card": statusCard{Status: "needs_you", Title: "Land it?", Body: "Fixed.\n" + testPR, Options: []string{"Approve", "Hold"}, Recommended: "Approve", Waits: "w1"}})
	card := l.slack.sent()[0]
	var read threadResult
	if err := json.Unmarshal(l.do(opThread, map[string]any{"channel_id": testDM, "thread_ts": card.ts, "raw": true}).Body, &read); err != nil {
		t.Fatal(err)
	}
	var buttons []string
	var raw string
	for _, m := range read.Messages {
		if m.TS == card.ts {
			buttons, raw = m.Buttons, string(m.Raw)
		}
	}
	if !slices.Contains(buttons, "Hold") || !strings.Contains(raw, "Land it?") {
		t.Fatalf("buttons = %v, raw = %s, want the card's buttons and its message as sent", buttons, raw)
	}
}

func TestAWorkObjectPostsAsJSONWithOnlyItsEntitiesAsMetadata(t *testing.T) {
	var posted []string
	var contentType, path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		posted, contentType, path = append(posted, string(body)), r.Header.Get("Content-Type"), r.URL.Path
		_, _ = w.Write([]byte(`{"ok":true,"channel":"D1","ts":"1.2"}`))
	}))
	defer srv.Close()
	api := newSlackClient("xoxb-test", srv.URL+"/", srv.Client())
	entity := testEntity()
	channel, ts, err := api.PostMessageContext(context.Background(), "D1",
		slack.MsgOptionText("See it", false), slack.MsgOptionTS("1.1"), slack.MsgOptionDisableLinkUnfurl(), slack.MsgOptionWorkObjectMetadata(slack.WorkObjectMetadata{Entities: []slack.WorkObjectEntity{entity}}))
	if err != nil || channel != "D1" || ts != "1.2" {
		t.Fatalf("post = %q %q %v", channel, ts, err)
	}
	if !strings.HasPrefix(contentType, "application/json") || path != "/chat.postMessage" {
		t.Fatalf("posted %s to %s, want JSON to chat.postMessage", contentType, path)
	}
	var body struct {
		Channel     string `json:"channel"`
		Text        string `json:"text"`
		ThreadTS    string `json:"thread_ts"`
		UnfurlLinks bool   `json:"unfurl_links"`
		Token       string `json:"token"`
		Metadata    struct {
			EventType    string         `json:"event_type"`
			EventPayload map[string]any `json:"event_payload"`
			Entities     []struct {
				URL           string `json:"url"`
				EntityPayload struct {
					Attributes struct {
						LastModified int64 `json:"metadata_last_modified"`
					} `json:"attributes"`
				} `json:"entity_payload"`
			} `json:"entities"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal([]byte(posted[0]), &body); err != nil {
		t.Fatalf("%s: %v", posted[0], err)
	}
	if body.Channel != "D1" || body.Text != "See it" || body.ThreadTS != "1.1" || body.UnfurlLinks || body.Token != "" {
		t.Fatalf("body = %+v, want the message's fields and no token", body)
	}
	if body.Metadata.EventType != "" || len(body.Metadata.EventPayload) != 0 || len(body.Metadata.Entities) != 1 ||
		body.Metadata.Entities[0].URL != testPR || body.Metadata.Entities[0].EntityPayload.Attributes.LastModified == 0 {
		t.Fatalf("metadata = %+v, want only a stamped entity, since Slack refuses event fields beside entities", body.Metadata)
	}
}

func TestAWorkObjectMessageKeepsItsBlocksAndAttachments(t *testing.T) {
	entities := slack.MsgOptionWorkObjectMetadata(slack.WorkObjectMetadata{Entities: []slack.WorkObjectEntity{testEntity()}})
	message := []slack.MsgOption{
		slack.MsgOptionText("See it", false),
		slack.MsgOptionBlocks(header("The card"), mrkdwn("card-body", "Its paragraph")),
		slack.MsgOptionAttachments(slack.Attachment{Fallback: "The attachment"}),
		entities,
	}
	for _, tc := range []struct {
		method string
		send   func(api slackAPI) error
	}{
		{"/chat.postMessage", func(api slackAPI) error {
			_, _, err := api.PostMessageContext(context.Background(), "D1", message...)
			return err
		}},
		{"/chat.update", func(api slackAPI) error {
			_, _, _, err := api.UpdateMessageContext(context.Background(), "D1", "1.2", message...)
			return err
		}},
	} {
		t.Run(tc.method, func(t *testing.T) {
			var got struct {
				Blocks      []map[string]any `json:"blocks"`
				Attachments []map[string]any `json:"attachments"`
			}
			var path, auth string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				path, auth = r.URL.Path, r.Header.Get("Authorization")
				if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
					t.Errorf("decode: %v", err)
				}
				_, _ = w.Write([]byte(`{"ok":true,"channel":"D1","ts":"1.2"}`))
			}))
			defer srv.Close()
			if err := tc.send(newSlackClient("xoxb-test", srv.URL+"/", srv.Client())); err != nil {
				t.Fatal(err)
			}
			if path != tc.method || auth != "Bearer xoxb-test" {
				t.Fatalf("posted to %s with auth %q", path, auth)
			}
			if len(got.Blocks) != 2 || got.Blocks[0]["type"] != "header" || len(got.Attachments) != 1 {
				t.Fatalf("blocks = %v, attachments = %v, want the card's two blocks and its attachment", got.Blocks, got.Attachments)
			}
		})
	}
}

func TestAPostWithNoWorkObjectStaysAForm(t *testing.T) {
	var contentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contentType = r.Header.Get("Content-Type")
		_, _ = w.Write([]byte(`{"ok":true,"channel":"D1","ts":"1.2"}`))
	}))
	defer srv.Close()
	api := newSlackClient("xoxb-test", srv.URL+"/", srv.Client())
	if _, _, err := api.PostMessageContext(context.Background(), "D1", slack.MsgOptionText("Hi", false), slack.MsgOptionMetadata(slack.SlackMetadata{EventType: postEventType, EventPayload: map[string]any{"user_id": "U1"}})); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(contentType, "application/x-www-form-urlencoded") {
		t.Fatalf("posted %s, want slack-go's form", contentType)
	}
}

func TestAWorkObjectAnswerNamesWhatSlackWarnedAndKept(t *testing.T) {
	got, _ := workObjectEcho([]byte(`{"ok":true,"response_metadata":{"warnings":["invalid_metadata"]},"message":{"attachments":[{"from_url":"https://x.example/1"}],"metadata":{"event_type":"e"}}}`))
	for _, want := range []string{"ok=true", "invalid_metadata", "https://x.example/1", `"event_type":"e"`} {
		if !strings.Contains(got, want) {
			t.Fatalf("echo = %q, missing %q", got, want)
		}
	}
}

func TestSlackDroppingACardReachesTheCallersContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"channel":"D1","ts":"1.2","warning":"metadata_too_large","response_metadata":{"warnings":["metadata_too_large"],"messages":["[WARN] too big"]}}`))
	}))
	defer srv.Close()
	api := newSlackClient("xoxb-test", srv.URL+"/", srv.Client())
	ctx := withPostWarnings(context.Background())
	_, _, err := api.PostMessageContext(ctx, "D1", slack.MsgOptionText("See it", false),
		slack.MsgOptionWorkObjectMetadata(slack.WorkObjectMetadata{Entities: []slack.WorkObjectEntity{testEntity()}}))
	if err != nil {
		t.Fatal(err)
	}
	if got := postWarningsIn(ctx); !strings.Contains(got, "too big") {
		t.Fatalf("refusals = %q, want Slack's words", got)
	}
}
