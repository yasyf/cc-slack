package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
		{"tapped", tapped, "*Waiting:* You approved it; it lands next. CI green.", "Ready for your tap"},
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
	if got := strings.Count(post.attachments, "Chats load faster"); got != prPreviewLimit {
		t.Fatalf("drew %d previews, want %d", got, prPreviewLimit)
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
	if post.text != prPreviewsLead || post.threadTS == "" || !strings.Contains(post.attachments, "Chats load faster") {
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
	l.do(opDMStatus, map[string]any{"text": "First " + testPR})
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
