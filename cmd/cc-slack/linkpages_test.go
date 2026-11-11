package main

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
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
			got, err := linkPages(tc.text)
			if err != nil || !slices.Equal(got, tc.want) {
				t.Fatalf("linkPages = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
	crowded := "see " + strings.Join(roomLinks(1, 6), " ")
	if _, err := linkPages(crowded); err == nil || err.Error() != (crowdedLine{1, 6}).refusal() {
		t.Fatalf("linkPages of one line with six links = %v, want it refused naming line 1", err)
	}
}

func TestADMPostPastFiveLinksGoesOutAsPagesThatEachDrawTheirCards(t *testing.T) {
	const hisThread = "1793804574.412309"
	l := startDaemon(t)
	loggedIn(t, l)
	text := strings.Join(slices.Concat([]string{"Tonight's cards:"}, roomLinks(1, 3), []string{""}, roomLinks(4, 7)), "\n")
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
		for i, page := range sent {
			if page.unfurlLinks != "true" || thread != "" && page.threadTS != thread || thread == "" && page.threadTS != "" {
				t.Fatalf("thread %q: page %d = %+v, want it unfurled where the post goes", thread, i+1, page)
			}
			if links := len(drawnLinks(page.text)); links > slackMaxPreviews {
				t.Fatalf("thread %q: page %d links %d messages, want at most five", thread, i+1, links)
			}
		}
		if !strings.Contains(sent[0].text, "card 3") || strings.Contains(sent[0].text, "card 4") || !strings.Contains(sent[1].text, "card 7") {
			t.Fatalf("thread %q: pages %q and %q, want the cut at the blank line after card 3", thread, sent[0].text, sent[1].text)
		}
		if !strings.Contains(res["page_permalink"], strings.ReplaceAll(sent[1].ts, ".", "")) {
			t.Fatalf("thread %q: dm-status = %v, want it to name the second page", thread, res)
		}
	}
}

func TestALineOrAnEditPastFiveLinksIsRefusedAndSendsNothing(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	posts := len(l.slack.sent())
	l.refused(opDMStatus, map[string]any{"text": "Tonight: " + strings.Join(roomLinks(1, 6), ", ")}, "line 1 links 6 Slack messages")
	l.refused(opEdit, map[string]any{"channel_id": testDM, "ts": "1800000000.000100", "text": strings.Join(roomLinks(1, 6), "\n")}, errEditPastFive.Error())
	if sent, edits := l.slack.sent()[posts:], l.slack.edited(); len(sent) != 0 || len(edits) != 0 {
		t.Fatalf("refused posts sent %+v and edited %+v, want nothing", sent, edits)
	}
}

var waitsOn41 = "<" + tapPR41[0] + "|#41> waits on your tap."

func TestAPostThatWaitsOnHimDrawsTheTapCardOfEachPRItLinks(t *testing.T) {
	cmd := fakeCardCommand(t, []string{"acme/app#41"}, mergeRowCard())
	l, _ := cardingDaemon(t, cmd.argv, "acme/app#41")
	l.do(opDMStatus, map[string]any{"text": waitsOn41})
	if cards := rootCards(awaitPRCards(t, l, testDM)); len(cards) != 1 {
		t.Fatalf("cards = %+v, want #41's tap card", cards)
	}
}

func TestAPostThatWaitsOnHimGoesOutLinkingAPRWhoseCardDoesNotDraw(t *testing.T) {
	cmd := fakeCardCommand(t, []string{"acme/app#41"})
	l, _ := cardingDaemon(t, cmd.argv, "acme/app#41")
	l.do(opDMStatus, map[string]any{"text": waitsOn41})
	if sent := l.slack.sentTo(testDM); len(sent) != 1 || !strings.Contains(sent[0].text, tapPR41[0]) {
		t.Fatalf("a waiting post whose PR drew no card sent %+v, want it once with the PR's link", sent)
	}
	l.do(opDMStatus, map[string]any{"text": "<" + tapPR41[0] + "|#41> merged at 6:40pm."})
	if sent := l.slack.sentTo(testDM); len(sent) != 2 {
		t.Fatalf("a post that waits on nothing sent %+v, want it after the first", sent)
	}
}
