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

func workObjectsIn(post fakePost) string {
	if !strings.Contains(post.metadata, `"entities"`) {
		return ""
	}
	return post.metadata
}

func entitiesOf(t *testing.T, raw string) []slack.WorkObjectEntity {
	t.Helper()
	var metadata slack.WorkObjectMetadata
	if err := json.Unmarshal([]byte(raw), &metadata); err != nil {
		t.Fatal(err)
	}
	return metadata.Entities
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
		{"ready", readyRow(41), "Ready for your tap", "Waiting on"},
		{"waiting", waiting, "the change below it, #40, has not landed", "Ready for your tap"},
		{"landed", landed, `"value":"Merged"`, "Waiting on"},
		{"tapped", tapped, "You approved it; it lands next.", "Ready for your tap"},
		{"held", held, `"value":"On hold"`, "record 12"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := previewingDaemon(t, fakePRRows(t, tc.row).argv, "acme/app#41")
			l.do(opDMStatus, map[string]any{"text": "See " + testPR})
			post := lastPost(l)
			for _, want := range []string{"#41 api: fix chat list 41", testPR, "Chats load faster.", tc.want} {
				if !strings.Contains(workObjectsIn(post), want) {
					t.Fatalf("attachments = %s, want %q in them", workObjectsIn(post), want)
				}
			}
			for _, absent := range []string{tc.absent, "Open the card"} {
				if strings.Contains(workObjectsIn(post), absent) {
					t.Fatalf("attachments = %s, want no %q in them", workObjectsIn(post), absent)
				}
			}
		})
	}
}

func TestAPRWithNoRowStaysALink(t *testing.T) {
	l := previewingDaemon(t, fakePRRows(t, readyRow(7)).argv, "acme/app#41")
	l.do(opDMStatus, map[string]any{"text": "Done.\n" + testPR})
	post := lastPost(l)
	if workObjectsIn(post) != "" || !strings.Contains(post.text, "<"+testPR+"|#41>") {
		t.Fatalf("post = %+v, want the link and no preview", post)
	}
}

func TestAPRNamedTwiceDrawsOnePreviewFromOneReadAndLosesItsBareLine(t *testing.T) {
	rows := fakePRRows(t, readyRow(41))
	l := previewingDaemon(t, rows.argv, "acme/app#41")
	l.do(opDMStatus, map[string]any{"text": "Fixed in " + testPR + ", as promised.\n" + testPR})
	post := lastPost(l)
	if got := strings.Count(workObjectsIn(post), "Chats load faster"); got != 1 {
		t.Fatalf("attachments = %s, want one preview", workObjectsIn(post))
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
	if !strings.Contains(post.text, testPR) || !strings.Contains(workObjectsIn(post), "Chats load faster") {
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
	if got := strings.Count(workObjectsIn(post), "Chats load faster"); got != linkPreviewLimit {
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
	if workObjectsIn(post) != "" || strings.Count(post.text, testPR) != 2 {
		t.Fatalf("post = %+v, want both links and no preview", post)
	}
}

func TestPRPreviewsAreOffWithNoCommand(t *testing.T) {
	l, _ := prLinksDaemon(t, map[string]any{"repos": []string{"acme/app"}}, "acme/app#41")
	l.do(opDMStatus, map[string]any{"text": "Fixed in " + testPR + ".\n" + testPR})
	post := lastPost(l)
	if workObjectsIn(post) != "" || strings.Count(post.text, testPR) != 2 {
		t.Fatalf("post = %+v, want both links and no preview", post)
	}
}

func TestOnlyAPostToTheDMReadsTheRows(t *testing.T) {
	rows := fakePRRows(t, readyRow(41))
	l := previewingDaemon(t, rows.argv, "acme/app#41")
	l.post(opSend, map[string]string{"channel_id": testChannel, "text": "See " + testPR})
	if post := lastPost(l); workObjectsIn(post) != "" {
		t.Fatalf("a post outside the DM drew %s", workObjectsIn(post))
	}
	if got := readAsks(t, rows.asked); len(got) != 0 {
		t.Fatalf("a post outside the DM read the rows: %v", got)
	}
}

func TestAStatusCardCarriesItsPRPreviewInItsThreadAndLosesItsBareLine(t *testing.T) {
	l := previewingDaemon(t, fakePRRows(t, readyRow(41)).argv, "acme/app#41")
	l.do(opDMStatus, map[string]any{"card": statusCard{Status: "info", Title: "Status", Body: "Fixed.\n" + testPR}})
	post := lastPost(l)
	if !strings.HasPrefix(post.text, linkPreviewsLead+"\n") || !strings.Contains(post.text, testPR) || post.threadTS == "" || !strings.Contains(workObjectsIn(post), "Chats load faster") {
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
	entities := entitiesOf(t, workObjectsIn(lastPost(l)))
	if len(entities) != 1 {
		t.Fatalf("entities = %+v, want one", entities)
	}
	fields := mustJSON(entities[0].EntityPayload["custom_fields"])
	if want := string(mustJSON(id.permalink(testDM, card.ts, card.threadTS))); !strings.Contains(string(fields), `"link":`+want) {
		t.Fatalf("fields = %s, want a link to the open card", fields)
	}
}

func TestAWaitingPRNeverOffersATapEvenWithAnOpenCard(t *testing.T) {
	waiting := readyRow(41)
	waiting.ReadyForTap, waiting.WaitsOn = false, "CI is running"
	l, _ := cardedPreviewDaemon(t, waiting)
	if got := workObjectsIn(lastPost(l)); strings.Contains(got, "Open the card") || !strings.Contains(got, "CI is running") {
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
	if got := strings.Count(workObjectsIn(post), "Chats load faster"); got != linkPreviewLimit {
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

func TestAPRCardOpensThePROnGitHubInOneTap(t *testing.T) {
	l := previewingDaemon(t, fakePRRows(t, readyRow(41)).argv, "acme/app#41")
	l.do(opDMStatus, map[string]any{"text": "See " + testPR})
	entity := entitiesOf(t, workObjectsIn(lastPost(l)))[0]
	if entity.URL != testPR || entity.EntityType != prEntityType {
		t.Fatalf("entity = %+v, want the item entity at the PR", entity)
	}
	var actions struct {
		Primary []struct {
			Text     string `json:"text"`
			ActionID string `json:"action_id"`
			URL      string `json:"url"`
		} `json:"primary_actions"`
	}
	if err := json.Unmarshal(mustJSON(entity.EntityPayload["actions"]), &actions); err != nil {
		t.Fatal(err)
	}
	if len(actions.Primary) != 1 || actions.Primary[0].URL != testPR || actions.Primary[0].Text != prOpenLabel || !strings.HasPrefix(actions.Primary[0].ActionID, cardLinkAction) {
		t.Fatalf("actions = %+v, want one link to GitHub the backend acks", actions)
	}
}

func TestARowDrawsTheMergeTagItsStateEarns(t *testing.T) {
	row := func(change func(*ledgerRow)) ledgerRow {
		r := ledgerRow{State: rowOpen, CI: ciGreen, Approvers: "forge-pr-reviewer", Mergeable: "clean", Queue: rowNone, Hold: rowNone}
		change(&r)
		return r
	}
	for _, tc := range []struct {
		name, value, color string
		row                ledgerRow
	}{
		{"open", "Open", tagBlue, row(func(*ledgerRow) {})},
		{"landed beats a red CI", "Merged", tagGreen, row(func(r *ledgerRow) { r.State, r.CI = rowLanded, ciRed })},
		{"closed", "Closed", tagGray, row(func(r *ledgerRow) { r.State = rowClosed })},
		{"ready", "Ready for your tap", tagBlue, row(func(r *ledgerRow) { r.ReadyForTap = true })},
		{"hold beats red CI", "On hold", tagYellow, row(func(r *ledgerRow) { r.Hold, r.CI = "root:12", ciRed })},
		{"red CI", "Blocked: CI red", tagRed, row(func(r *ledgerRow) { r.CI = ciRed })},
		{"conflict", "Blocked: conflict", tagRed, row(func(r *ledgerRow) { r.Mergeable = rowConflict })},
		{"no approval", "Blocked: review", tagYellow, row(func(r *ledgerRow) { r.Approvers = rowNone })},
		{"queued", "Queued", tagYellow, row(func(r *ledgerRow) { r.Queue = rowQueued })},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.row.mergeTag(); got.value != tc.value || got.color != tc.color {
				t.Fatalf("merge tag = %+v, want %s %s", got, tc.value, tc.color)
			}
		})
	}
}

func TestACardWithARowDrawsItsWorkObjectInPlaceOfItsFieldBlocks(t *testing.T) {
	row := readyRow(41)
	l := previewingDaemon(t, fakePRRows(t).argv, "acme/app#41")
	card := statusCard{Status: "info", Title: "Status", PRs: []cardPR{{URL: testPR, Change: "Chats load faster", Row: &row}}}
	l.do(opDMStatus, map[string]any{"card": card})
	post := l.slack.sent()[0]
	if strings.Contains(post.blocks, "What changes") {
		t.Fatalf("blocks = %s, want the fields only on the Work Object", post.blocks)
	}
	if entities := entitiesOf(t, workObjectsIn(post)); len(entities) != 1 || entities[0].URL != testPR {
		t.Fatalf("metadata = %s, want the PR's Work Object", post.metadata)
	}
}

func TestAPostNamingAPRAndADocCarriesBothCards(t *testing.T) {
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
	entities := entitiesOf(t, workObjectsIn(lastPost(l)))
	urls := make([]string, 0, len(entities))
	for _, entity := range entities {
		urls = append(urls, entity.URL)
	}
	if strings.Join(urls, " ") != doc.URL+" "+testPR {
		t.Fatalf("cards at %v, want the doc and the PR", urls)
	}
}

func TestTheLinkLineUnderACardAlwaysNamesItsPRs(t *testing.T) {
	entity := readyRow(41).entity(testPR, "")
	if got, want := linkPreviewsText([]slack.WorkObjectEntity{entity}), linkPreviewsLead+"\n<"+testPR+"|#41 api: fix chat list 41>"; got != want {
		t.Fatalf("text = %q, want %q", got, want)
	}
}

func TestAThreadReadNamesEachMessagesButtonsAndWorkObjects(t *testing.T) {
	l := previewingDaemon(t, fakePRRows(t, readyRow(41)).argv, "acme/app#41")
	l.do(opDMStatus, map[string]any{"card": statusCard{Status: "needs_you", Title: "Land it?", Body: "Fixed.\n" + testPR, Options: []string{"Approve", "Hold"}, Recommended: "Approve", Waits: "w1"}})
	card, reply := l.slack.sent()[0], lastPost(l)
	var read threadResult
	if err := json.Unmarshal(l.do(opThread, map[string]any{"channel_id": testDM, "thread_ts": card.ts, "work_objects": true}).Body, &read); err != nil {
		t.Fatal(err)
	}
	var buttons []string
	var drawn []slack.WorkObjectEntity
	for _, m := range read.Messages {
		if m.TS == card.ts {
			buttons = m.Buttons
		}
		if m.TS == reply.ts {
			drawn = m.WorkObjects
		}
	}
	if !slices.Contains(buttons, "Hold") || len(drawn) != 1 || drawn[0].URL != testPR {
		t.Fatalf("buttons = %v, work objects = %+v, want the card's buttons and the reply's PR card", buttons, drawn)
	}
}
