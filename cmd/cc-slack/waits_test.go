package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestALineThatWaitsOnYouLinksItsCardOrIsRefused(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	c := postWaiting(t, l, mergeCard())
	posts := len(l.slack.sent())

	_, err := invoke(l.ctx, l.deps, testSession, "/any", opDMStatus, mustJSON(map[string]any{"text": "The Hima fix waits on your pick."}))
	if err == nil || !strings.Contains(err.Error(), `"The Hima fix waits on your pick"`) || !strings.Contains(err.Error(), "cc-slack drive-cards") {
		t.Fatalf("a line that waits on him with no card: %v, want it refused naming the sentence and the card to add", err)
	}
	if len(l.slack.sent()) != posts {
		t.Fatalf("a refused line posted %d messages", len(l.slack.sent())-posts)
	}

	l.refused(opDMStatus, map[string]any{"text": "The Hima fix waits on your pick: " + c.url()}, cardLinkNamed)
	l.refused(opDMStatus, map[string]any{"text": "The Hima fix waits on your pick: <" + c.url() + "|" + c.url() + ">"}, cardLinkNamed)
	l.do(opDMStatus, map[string]any{"text": "The Hima fix waits on your pick: <" + c.url() + "|the card>"})
	sent := l.slack.sent()[posts:]
	if len(sent) != 1 || sent[0].unfurlLinks != "true" || sent[0].blocks != "" {
		t.Fatalf("posts = %+v, want the line alone, unfurling the card it links", sent)
	}

	l.do(opDMStatus, map[string]any{"text": "The release is live, and every chat opens again."})
	if len(l.slack.sent()) != posts+2 {
		t.Fatalf("an ordinary line posted %d messages, want one", len(l.slack.sent())-posts-1)
	}
}

func TestWaitsOnYouReadsPlainVariantsAndLeavesOtherLines(t *testing.T) {
	cases := map[string]bool{
		"The Hima fix waits on your pick.":           true,
		"Still waiting on you to pick a route.":      true,
		"This one needs your tap before it lands.":   true,
		"Blocked on your approval.":                  true,
		"I need your answer on the cache key.":       true,
		"Your pick is needed on the import fix.":     true,
		"It's your call: fingerprint or path.":       true,
		"The rollout is up to you.":                  true,
		"Ready for your review whenever you are.":    true,
		"It needs a tap from you.":                   true,
		"You need to approve the release first.":     true,
		"This waits on your answer.":                 true,
		"What I need is your pick on the cache key.": true,
		"The cutover plan's step 7b, a design-lane recommendation you have not picked yet, deletes the session target.": true,
		"You haven't answered either card.":                                                                         true,
		"You still have not chosen a route.":                                                                        true,
		"The SandSQL view fix is waiting on your card.":                                                             true,
		"It waits only on your card about handing SandSQL's primaries over, which has not drawn yet.":               true,
		"The import fix waits on your question about the cache key.":                                                true,
		"Checking now: a list of what's still waiting on you, biggest first, with links, coming in a minute or two": false,
		"Looking at what still needs your tap, back shortly.":                                                       false,
		"Pulling up what you need to approve.":                                                                      false,
		"Nothing waits on you tonight.":                                                                             false,
		"The fix no longer needs your tap.":                                                                         false,
		"Still waiting on CI, and you hear next at 7:10pm.":                                                         false,
		"Your PR is waiting on CI.":                                                                                 false,
		"Thank you for waiting.":                                                                                    false,
		"It is blocked on the migration you asked for.":                                                             false,
		"The release is live, and every chat opens again.":                                                          false,
		"<@U0123ABCD>, this waits on your review.":                                                                  false,
		"Your approval landed it at 6:40pm.":                                                                        false,
		"`waits on your pick` is the line that was refused.":                                                        false,
		"Everything waiting on you is in the list below.":                                                           false,
		"Here is all that still needs your tap.":                                                                    false,
		"A list of whichever needs your approval.":                                                                  false,
		"Reading what that still waits on you now.":                                                                 false,
		"You have not missed anything since the release.":                                                           false,
		"You never ruled on the cache key, so the old default holds.":                                               false,
		"Your card on the cache key is answered.":                                                                   false,
	}
	for text, want := range cases {
		if got := len(waitsOnReader(text)) > 0; got != want {
			t.Errorf("waitsOnReader(%q) = %v, want %v", text, got, want)
		}
	}
}

func TestAPRsTapCardCarriesOnlyALineWaitingOnHisTapAndAPickLineNeedsItsQuestionCard(t *testing.T) {
	cmd := fakeCardCommand(t, []string{"acme/app#41"}, mergeRowCard())
	l, _ := cardingDaemon(t, cmd.argv, "acme/app#41")
	question := postWaiting(t, l, mergeCard())
	linked := "<" + tapPR41[0] + "|#41>"
	for _, body := range []map[string]any{
		{"text": "#41 waits on your pick of the cache key.", "tap_prs": tapPR41},
		{"text": linked + " waits on your pick of the cache key."},
		{"text": linked + " is a design-lane recommendation you have not picked yet."},
		{"text": linked + " waits on your tap.\nThe cache key is your call."},
	} {
		l.refused(opDMStatus, body, "no PR's tap card stands in for")
	}
	l.do(opDMStatus, map[string]any{"text": "#41 waits on your tap.", "tap_prs": tapPR41})
	l.do(opDMStatus, map[string]any{"text": "#41 still waits on you.", "tap_prs": tapPR41})
	l.do(opDMStatus, map[string]any{"text": linked + " waits on your approval."})
	l.do(opDMStatus, map[string]any{"text": linked + " waits on your pick: <" + question.url() + "|the merge card>", "tap_prs": tapPR41})
}

func TestAListOfStatesEndingInWaitingOnYouWaitsOnNothing(t *testing.T) {
	cases := map[string]bool{
		"I'm gathering where each order stands, merged, open or waiting on you, and the list goes in this thread.": false,
		"Every PR is merged, in review or waiting on you.":                                                         false,
		"Merged, open or waiting on you to pick a route.":                                                          true,
		"The fix is open or waiting on you.":                                                                       true,
		"Tests passed, release is open or waiting on you.":                                                         true,
	}
	for text, want := range cases {
		if got := len(waitsOnReader(text)) > 0; got != want {
			t.Errorf("waitsOnReader(%q) = %v, want %v", text, got, want)
		}
	}
}

func TestAPostThatCopiesACardIsRefusedAndItsLinkIsNot(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	c := postWaiting(t, l, mergeCard())
	posts := len(l.slack.sent())
	copied := map[string]any{"type": "section", "block_id": cardCopyBlock + "-" + c.ts, "text": map[string]string{"type": "mrkdwn", "text": "Merge #41 now?"}}
	l.refused(opSend, map[string]any{"channel_id": testChannel, "text": "Merge #41 now?", "blocks": []any{copied}}, "these blocks copy a card")
	l.refused(opDMStatus, map[string]any{"text": "Merge #41 now? still waits, the release goes out after it."}, "repeats the open card \"Merge #41 now?\" without its link: link it as "+slackLink(c.url(), "Merge #41 now?"))
	if len(l.slack.sent()) != posts {
		t.Fatalf("refused posts sent %+v", l.slack.sent()[posts:])
	}
	l.do(opDMStatus, map[string]any{"text": "Merge #41 now? still waits: <" + c.url() + "|the merge card>"})
	l.do(opDMStatus, map[string]any{"thread_ts": c.thread, "text": "Merge #41 now? is the card above."})
	if len(l.slack.sent()) != posts+2 {
		t.Fatalf("posts = %+v, want the line that links the card and the reply in its own thread", l.slack.sent()[posts:])
	}
}

func TestAnEditDrawsAnOpenCardItsThreadHasNotShown(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	card := postWaiting(t, l, mergeCard())
	link := "<" + card.url() + "|the merge card>"
	var first postResult
	if err := json.Unmarshal(l.post(opSend, map[string]string{"channel_id": testChannel, "text": "first: " + link}).Body, &first); err != nil {
		t.Fatal(err)
	}
	l.post(opEdit, map[string]string{"channel_id": testChannel, "ts": "1800000000.000100", "text": "now see " + link})
	l.post(opEdit, map[string]string{"channel_id": testChannel, "ts": "1800000000.000100", "text": "now see " + link + ", still"})
	l.post(opEdit, map[string]string{"channel_id": testChannel, "ts": "1800000000.000200", "text": "see https://example.slack.com/archives/C1/p1793818357021259"})
	l.post(opEdit, map[string]string{"channel_id": testChannel, "ts": "1800000000.000300", "thread_ts": first.TS, "text": "edited: " + link})
	edits := l.slack.edited()
	if len(edits) != 4 || edits[0].unfurlLinks != "true" || edits[1].unfurlLinks != "true" || edits[2].unfurlLinks == "true" || edits[3].unfurlLinks == "true" {
		t.Fatalf("edits = %+v, want the card drawn by the message that links it each time it is edited, and plain in a thread that shows it and under a link to another message", edits)
	}
}
