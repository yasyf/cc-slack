package main

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/slack-go/slack"
)

func TestSearchFindsMessagesAsTheUserWithItsFilters(t *testing.T) {
	l := startDaemon(t)
	loggedInWithOAuth(t, l)
	user := l.user.as(testUser)
	ruling := "Release links in drive posts must be the Slack post, never Buildkite.\nKeep it in the long-running config."
	user.matches = []slack.SearchMessage{{
		Channel: slack.CtxChannel{ID: testChannel, Name: "releases"}, User: "U0OWNER01", Username: "yasyf",
		Timestamp: "1794700000.000100", Text: ruling,
		Permalink: "https://acme.slack.com/archives/C1/p1794700000000100?thread_ts=1794690000.000100&cid=C1",
	}}

	var res historyResult
	if err := json.Unmarshal(l.do(opHistory, map[string]any{
		"query": "buildkite", "channel_id": testChannel, "from": "U0OWNER01", "after": "2026-11-13", "before": "2026-11-16",
	}).Body, &res); err != nil {
		t.Fatal(err)
	}

	want := "buildkite in:<#" + testChannel + "> from:<@U0OWNER01> after:2026-11-13 before:2026-11-16"
	if !slices.Equal(user.searches(), []string{want}) || len(l.slack.searches()) != 0 {
		t.Fatalf("searched as the user %q and as the bot %q, want one search as the user for %q", user.searches(), l.slack.searches(), want)
	}
	if res.Query != want || res.Total != 1 || len(res.Messages) != 1 {
		t.Fatalf("result = %+v, want the one match for %q", res, want)
	}
	if m := res.Messages[0]; m.Text != ruling || m.ChannelID != testChannel || m.ThreadTS != "1794690000.000100" || m.TS != "1794700000.000100" {
		t.Fatalf("match = %+v, want its whole text, channel and thread", m)
	}
}

func TestSearchRefusesADayItCannotRead(t *testing.T) {
	l := startDaemon(t)
	loggedInWithOAuth(t, l)
	user := l.user.as(testUser)

	l.refused(opHistory, map[string]any{"from": "yasyf", "after": "Nov 14"}, `search after takes a day as YYYY-MM-DD, not "Nov 14"`)
	if len(user.searches()) != 0 {
		t.Fatalf("searched %q, want nothing asked of Slack", user.searches())
	}
}

func TestSearchWithATokenLackingSearchAsksForConsentAgain(t *testing.T) {
	l := startDaemon(t)
	loggedInWithOAuth(t, l)
	user := l.user.as(testUser)
	user.fail("search.messages", slack.SlackErrorResponse{Err: "missing_scope"})

	raw, _ := json.Marshal(map[string]any{"query": "buildkite"})
	_, err := invoke(l.ctx, l.deps, testSession, "/any", opHistory, raw)
	if err == nil || !strings.Contains(err.Error(), "needs your consent to search") || !strings.Contains(err.Error(), "tap Allow on the card") {
		t.Fatalf("search with a token lacking search:read = %v, want a consent card", err)
	}
}
