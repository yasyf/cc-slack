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

	var line map[string]string
	if err := json.Unmarshal(l.do(opDMStatus, map[string]any{"text": "The Hima fix waits on your pick: <" + c.url() + "|the card>"}).Body, &line); err != nil {
		t.Fatal(err)
	}
	sent := l.slack.sent()[posts:]
	if len(sent) != 1 || !strings.Contains(sent[0].text, c.url()) {
		t.Fatalf("posts = %+v, want the line alone, linking the card for Slack to unfurl", sent)
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
	}
	for text, want := range cases {
		if _, got := waitsOnReader(text); got != want {
			t.Errorf("waitsOnReader(%q) = %v, want %v", text, got, want)
		}
	}
}
