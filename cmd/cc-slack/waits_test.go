package main

import (
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

	l.refused(opDMStatus, map[string]any{"text": "The Hima fix waits on your pick: <" + c.url() + "|the card>"}, cardLinkBare)
	l.do(opDMStatus, map[string]any{"text": "The Hima fix waits on your pick: " + c.url()})
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
	}
	for text, want := range cases {
		if _, got := waitsOnReader(text); got != want {
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
	l.refused(opDMStatus, map[string]any{"text": "Merge #41 now? still waits, the release goes out after it."}, "repeats the open card \"Merge #41 now?\" without its link: write its bare permalink "+c.url())
	if len(l.slack.sent()) != posts {
		t.Fatalf("refused posts sent %+v", l.slack.sent()[posts:])
	}
	l.do(opDMStatus, map[string]any{"text": "Merge #41 now? still waits: " + c.url()})
	l.do(opDMStatus, map[string]any{"thread_ts": c.thread, "text": "Merge #41 now? is the card above."})
	if len(l.slack.sent()) != posts+2 {
		t.Fatalf("posts = %+v, want the line that links the card and the reply in its own thread", l.slack.sent()[posts:])
	}
}

func TestAnEditThatLinksAMessageUnfurlsIt(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	l.post(opEdit, map[string]string{"channel_id": testChannel, "ts": "1800000000.000100", "text": "now see https://example.slack.com/archives/C1/p1793818357021259"})
	l.post(opEdit, map[string]string{"channel_id": testChannel, "ts": "1800000000.000100", "text": "see https://example.com/a"})
	if edits := l.slack.edited(); len(edits) != 2 || edits[0].unfurlLinks != "true" || edits[1].unfurlLinks == "true" {
		t.Fatalf("edits = %+v, want the one linking a message to unfurl it and the other left alone", edits)
	}
}
