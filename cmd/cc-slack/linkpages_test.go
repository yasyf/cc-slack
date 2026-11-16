package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/slack-go/slack"
)

func roomLink(n int) string {
	return fmt.Sprintf("<https://example.slack.com/archives/%s/p17938183570212%02d|card %d>", testRoom, n, n)
}

func roomLinks(from, to int) []string {
	var lines []string
	for n := from; n <= to; n++ {
		lines = append(lines, roomLink(n))
	}
	return lines
}

func TestLinkPagesCutsBeforeTheSixthLink(t *testing.T) {
	t.Parallel()
	head := "Tonight's cards:"
	cases := map[string]struct {
		text string
		want []string
	}{
		"five links stay one post": {
			strings.Join(append([]string{head}, roomLinks(1, 5)...), "\n"),
			[]string{strings.Join(append([]string{head}, roomLinks(1, 5)...), "\n")},
		},
		"a blank line before the sixth link": {
			strings.Join(slices.Concat([]string{head}, roomLinks(1, 3), []string{"", ""}, roomLinks(4, 7)), "\n"),
			[]string{strings.Join(append([]string{head}, roomLinks(1, 3)...), "\n"), strings.Join(roomLinks(4, 7), "\n")},
		},
		"no blank line cuts at the sixth link's line": {
			strings.Join(append([]string{head}, roomLinks(1, 7)...), "\n"),
			[]string{strings.Join(append([]string{head}, roomLinks(1, 5)...), "\n"), strings.Join(roomLinks(6, 7), "\n")},
		},
		"a link named twice counts once": {
			strings.Join(slices.Concat([]string{head}, roomLinks(1, 5), []string{"again " + roomLink(1)}), "\n"),
			[]string{strings.Join(slices.Concat([]string{head}, roomLinks(1, 5), []string{"again " + roomLink(1)}), "\n")},
		},
		"eleven links make three posts": {
			strings.Join(roomLinks(1, 11), "\n"),
			[]string{strings.Join(roomLinks(1, 5), "\n"), strings.Join(roomLinks(6, 10), "\n"), roomLink(11)},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := linkPages(tc.text, everyLinkDraws)
			if err != nil || !slices.Equal(got, tc.want) {
				t.Fatalf("linkPages = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
	crowded := "see " + strings.Join(roomLinks(1, 6), " ")
	if _, err := linkPages(crowded, everyLinkDraws); err == nil || err.Error() != (crowdedLine{1, 6}).refusal() {
		t.Fatalf("linkPages of one line with six links = %v, want it refused naming line 1", err)
	}
}

func TestTableRowsNeverCountTowardTheFivePreviews(t *testing.T) {
	t.Parallel()
	text := strings.Join([]string{"Tonight's cards:", "", "| When | Cards |", "|---|---|", "| tonight | " + strings.Join(roomLinks(1, 6), " ") + " |", "| tomorrow | " + strings.Join(roomLinks(7, 9), " ") + " |"}, "\n")
	if pages, err := linkPages(text, everyLinkDraws); err != nil || !slices.Equal(pages, []string{text}) {
		t.Fatalf("linkPages = %q, %v; want the post whole, since Slack previews no link in a table cell", pages, err)
	}
	if err := refuseEditPastFive(identity{DMChannelID: testDM}, opBody{ChannelID: testDM, Text: text}, everyLinkDraws); err != nil {
		t.Fatalf("an edit whose table links nine cards: %v, want it let through", err)
	}
}

func TestALongStatusTableOfCardLinksPostsAsOneMessageThatKeepsItsHeader(t *testing.T) {
	const hisThread = "1793804574.412309"
	text, err := os.ReadFile("testdata/long-status-table.md")
	if err != nil {
		t.Fatal(err)
	}
	l := startDaemon(t)
	loggedIn(t, l)
	var res map[string]string
	if err := json.Unmarshal(l.do(opDMStatus, map[string]any{"text": string(text), "thread_ts": hisThread}).Body, &res); err != nil {
		t.Fatal(err)
	}
	sent := l.slack.sentTo(testDM)
	if len(sent) != 1 || res["page_permalink"] != "" {
		t.Fatalf("posts = %+v, dm-status = %v, want the 24-row table of 36 card links as one message", sent, res)
	}
	blocks := postedBlocks(t, sent[0].blocks)
	drawn := tables(blocks)
	if err := checkBlocks(blocks); err != nil || len(drawn) != 1 || len(drawn[0].Rows) != 25 {
		t.Fatalf("blocks %s, %v; want one table of its header and 24 rows that Slack takes", sent[0].blocks, err)
	}
	header := make([]string, 0, len(drawn[0].Rows[0]))
	for _, cell := range drawn[0].Rows[0] {
		header = append(header, cellText(cell))
	}
	if !slices.Equal(header, []string{"*Order*", "*State*", "*Your open question*"}) {
		t.Fatalf("header = %q, want the table's own header", header)
	}
	if rows := slices.DeleteFunc(sectionTexts(blocks), func(s string) bool { return !strings.Contains(s, " · ") }); len(rows) > 0 {
		t.Fatalf("sections %q, want no row drawn as text, where Slack previews its links", rows)
	}
	l.do(opEdit, map[string]any{"channel_id": testDM, "thread_ts": hisThread, "ts": sent[0].ts, "text": string(text)})
	if edits := l.slack.edited(); len(edits) != 1 || len(tables(postedBlocks(t, edits[0].blocks))) != 1 {
		t.Fatalf("edits = %+v, want one chat.update drawing the table", edits)
	}
}

func everyLinkDraws(permalinkRef) bool { return true }

func openCardLinks(t *testing.T, l *liveDaemon, n int) []string {
	t.Helper()
	links := make([]string, n)
	for i := range links {
		card := postWaiting(t, l, statusCard{Status: "needs_you", Title: fmt.Sprintf("Take route %d?", i+1), Body: "CI is green.", Options: []string{"Take it", "Hold"}, Recommended: "Hold", Waits: fmt.Sprintf("route-%d", i+1)})
		links[i] = "<" + card.url() + fmt.Sprintf("|card %d>", i+1)
	}
	return links
}

func TestADMPostPastFiveLinksGoesOutAsPagesThatEachDrawTheirCards(t *testing.T) {
	const hisThread = "1793804574.412309"
	l := startDaemon(t)
	loggedIn(t, l)
	cards := openCardLinks(t, l, 7)
	text := strings.Join(slices.Concat([]string{"Tonight's cards:"}, cards[:3], []string{""}, cards[3:]), "\n")
	for _, thread := range []string{"", hisThread} {
		posts := len(l.slack.sentTo(testDM))
		var res map[string]string
		if err := json.Unmarshal(l.do(opDMStatus, map[string]any{"text": text, "thread_ts": thread}).Body, &res); err != nil {
			t.Fatal(err)
		}
		sent := l.slack.sentTo(testDM)[posts:]
		if len(sent) != 2 {
			t.Fatalf("thread %q: posts = %+v, want two pages", thread, sent)
		}
		pageThread := thread
		if thread == "" {
			pageThread = sent[0].ts
		}
		for i, page := range sent {
			want := pageThread
			if i == 0 {
				want = thread
			}
			if page.unfurlLinks != "true" || page.threadTS != want {
				t.Fatalf("thread %q: page %d = %+v, want it unfurled in thread %q: the first page where the post goes, each later page in the first page's thread", thread, i+1, page, want)
			}
			if links := len(drawnLinks(page.text, everyLinkDraws)); links > slackMaxPreviews {
				t.Fatalf("thread %q: page %d links %d messages, want at most five", thread, i+1, links)
			}
		}
		if !strings.Contains(sent[0].text, "card 3") || strings.Contains(sent[0].text, "card 4") || !strings.Contains(sent[1].text, "card 7") {
			t.Fatalf("thread %q: pages %q and %q, want the cut at the blank line after card 3", thread, sent[0].text, sent[1].text)
		}
		if !strings.Contains(res["page_permalink"], strings.ReplaceAll(sent[1].ts, ".", "")+"?thread_ts="+pageThread) {
			t.Fatalf("thread %q: dm-status = %v, want it to name the second page in thread %q", thread, res, pageThread)
		}
	}
}

func TestALineOrAnEditPastFiveLinksIsRefusedAndSendsNothing(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	cards := openCardLinks(t, l, 6)
	posts, edited := len(l.slack.sent()), len(l.slack.edited())
	l.refused(opDMStatus, map[string]any{"text": "Tonight: " + strings.Join(cards, ", ")}, "line 1 links 6 Slack messages")
	l.refused(opEdit, map[string]any{"channel_id": testDM, "ts": "1800000000.000100", "text": strings.Join(cards, "\n")}, errEditPastFive.Error())
	if sent, edits := l.slack.sent()[posts:], l.slack.edited()[edited:]; len(sent) != 0 || len(edits) != 0 {
		t.Fatalf("refused posts sent %+v and edited %+v, want nothing", sent, edits)
	}
}

var waitsOn41 = "<" + tapPR41[0] + "|#41> waits on your tap."

func TestAPostDrawsTheTapCardOfEachPRItLinksWhateverItsWords(t *testing.T) {
	cmd := fakeCardCommand(t, []string{"acme/app#41"}, mergeRowCard())
	l, _ := cardingDaemon(t, cmd.argv, "acme/app#41")
	l.do(opDMStatus, map[string]any{"text": "<" + tapPR41[0] + "|#41> merged into the release branch."})
	if cards := rootCards(prCards(l, testDM)); len(cards) != 1 {
		t.Fatalf("cards = %+v, want #41's tap card drawn before dm-status answers", cards)
	}
}

func TestALineLinkingAPRByURLOrByItsCardGetsItsTapCardRightUnderIt(t *testing.T) {
	const hisThread = "1793804574.412309"
	cmd := fakeCardCommand(t, []string{"acme/app#41"}, mergeRowCard())
	l, _ := cardingDaemon(t, cmd.argv, "acme/app#41")
	said := func(text string, tapPRs []string) map[string]string {
		t.Helper()
		var sent map[string]string
		if err := json.Unmarshal(l.do(opDMStatus, map[string]any{"thread_ts": hisThread, "text": text, "tap_prs": tapPRs}).Body, &sent); err != nil {
			t.Fatal(err)
		}
		card := postUnder(t, l, hisThread, sent["ts"])
		if sent["card_ts"] != card.ts || !strings.Contains(card.blocks, "Merge (Recommended)") {
			t.Fatalf("dm-status = %v, post under %q = %+v; want #41's card with Merge right under the line, named in the reply", sent, text, card)
		}
		return sent
	}
	said("#41 is green", tapPR41)
	byURL := said("Waiting on you: <"+tapPR41[0]+"|your card's fix>'s Approve", nil)
	said("Its Merge is on <"+byURL["card_permalink"]+"|its card>", nil)
}

func TestAPostLinkingAPRTheCardCommandSkipsGoesOutAndRefusesNothing(t *testing.T) {
	dir := t.TempDir()
	script, answer := filepath.Join(dir, "card"), filepath.Join(dir, "answer.json")
	skipped := map[string]any{"cards": []any{}, "skipped": []map[string]any{{"repo": "acme/app", "number": 41, "reason": "he approved it already"}}}
	if err := os.WriteFile(answer, mustJSON(skipped), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(script, []byte("#!/bin/sh\ncat >/dev/null\ncat '"+answer+"'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	l, _ := cardingDaemon(t, []string{script}, "acme/app#41")
	var sent map[string]string
	if err := json.Unmarshal(l.do(opDMStatus, map[string]any{"text": waitsOn41}).Body, &sent); err != nil {
		t.Fatal(err)
	}
	if posts := l.slack.sentTo(testDM); len(posts) != 1 || !strings.Contains(posts[0].text, tapPR41[0]) || sent["warning"] != "" {
		t.Fatalf("a post linking an approved PR sent %+v and answered %v, want it once with the PR's link, no card and no warning", posts, sent)
	}
}

func TestALineWhoseLinkedPRsDrawnCardFailsToPostComesDown(t *testing.T) {
	cmd := fakeCardCommand(t, []string{"acme/app#41"}, mergeRowCard())
	l, _ := cardingDaemon(t, cmd.argv, "acme/app#41")
	l.slack.fail("chat.postMessage", nil)
	l.slack.fail("chat.postMessage", slack.SlackErrorResponse{Err: "invalid_blocks"})
	l.refused(opDMStatus, map[string]any{"text": "<" + tapPR41[0] + "|#41> is green."}, "for "+tapPR41[0]+": its card was not posted; the daemon log says why, so took that post down; nothing was sent")
	line := l.slack.sentTo(testDM)[0]
	if deleted := l.slack.deleted(); len(deleted) != 1 || deleted[0].ts != line.ts {
		t.Fatalf("deleted %+v, want the line %s taken down once its card failed", deleted, line.ts)
	}
}
