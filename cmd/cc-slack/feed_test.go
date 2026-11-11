package main

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/slack-go/slack"

	"github.com/yasyf/cc-interact/agent"
)

const (
	testMonitor      = 317041616
	alertSummary     = "1800000010.000000"
	beforeFeedTS     = "1800000000.000000"
	testRelease      = "1800000400.000000"
	runningLine      = "*:rocket: stark-maclean is running · <@U0000000001> via `claude` · `api`*\n:black_nib: Maclean: Scottish poet."
	finishedLine     = "*:white_check_mark: stark-maclean is live · <@U0000000001> via `claude` · `api` · finished 10:25 PM*\n:black_nib: Maclean."
	monitorTitle     = "Checkout endpoint failing"
	alertSummaryText = "*Monitor alert summary:* " + monitorTitle + "\n*First alerted:* now\n*Latest status:* Triggered"
)

func subscribeFeed(t *testing.T, f *fixture, kinds ...string) {
	t.Helper()
	id, _, err := insertWatch(f.ctx, f.db(), testSubject, agent.TopLevel, kindChannel, testChannel, "", beforeFeedTS, f.now)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.poller.feeds.subscribe(f.ctx, id, kinds, ""); err != nil {
		t.Fatal(err)
	}
}

func datadog(ts, thread, transition, text string) slack.Message {
	return slack.Message{Msg: slack.Msg{
		Timestamp: ts, ThreadTimestamp: thread, User: "UDATADOG", BotID: "BDATADOG", Text: text,
		Metadata: slack.SlackMetadata{EventType: monitorEventType, EventPayload: map[string]any{"monitor_id": float64(testMonitor), "transition_type": transition}},
	}}
}

func (f *fixture) post(m slack.Message) {
	f.t.Helper()
	f.slack.add(testChannel, m)
	thread := m.ThreadTimestamp
	if thread == m.Timestamp {
		thread = ""
	}
	f.poller.nudged(messageNudge{Channel: testChannel, TS: m.Timestamp, ThreadTS: thread})
	f.tick()
}

func feedLines(t *testing.T, f *fixture) []string {
	t.Helper()
	var lines []string
	for _, e := range f.plane.events {
		if e.Type == eventMessage {
			t.Fatalf("a subscribed channel delivered a raw message: %s", e.Payload)
		}
		if e.Type != eventFeed {
			continue
		}
		var got feedPayload
		if err := json.Unmarshal(e.Payload, &got); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, got.Line)
	}
	return lines
}

func TestAFlappingMonitorReadsAsOneOpenAndOneClose(t *testing.T) {
	f := newFixture(t)
	subscribeFeed(t, f, feedAlerts)
	f.tick()
	link := f.id.permalink(testChannel, alertSummary, alertSummary)

	f.post(datadog(alertSummary, "", "alert", alertSummaryText))
	f.post(datadog("1800000011.000000", alertSummary, "alert", "Triggered: "+monitorTitle+"\nThe checkout endpoint failed."))
	f.post(datadog("1800000100.000000", alertSummary, "alert recovery", "Recovered: "+monitorTitle))
	f.post(datadog("1800000200.000000", alertSummary, "alert", "Re-Triggered: "+monitorTitle))
	f.post(datadog("1800000300.000000", alertSummary, "alert recovery", "Recovered: "+monitorTitle))

	open := `ALERT open monitor 317041616 "Checkout endpoint failing" (alert) ` + link
	if got := feedLines(t, f); !slices.Equal(got, []string{open}) {
		t.Fatalf("lines while it flaps = %q, want only %q", got, open)
	}
	quietUntil := tsTime("1800000300.000000").Add(alertQuiet)
	if next := f.tick(); !next.Equal(quietUntil) {
		t.Fatalf("next wake = %s, want the end of its quiet window %s", next, quietUntil)
	}

	f.now = quietUntil
	f.tick()
	closed := `ALERT closed monitor 317041616 "Checkout endpoint failing" after 5m, refired once ` + link
	if got := feedLines(t, f); !slices.Equal(got, []string{open, closed}) {
		t.Fatalf("lines after 30 quiet minutes = %q, want %q then %q", got, open, closed)
	}

	f.post(datadog("1800002400.000000", "", "alert", alertSummaryText))
	if got := feedLines(t, f); len(got) != 3 {
		t.Fatalf("lines after it fires again past its quiet window = %q, want a third, a new open", got)
	}
}

func TestASubscriptionDropsWhatItsKindsLeaveOut(t *testing.T) {
	f := newFixture(t)
	subscribeFeed(t, f, feedAlerts)
	f.tick()

	f.post(slack.Message{Msg: slack.Msg{Timestamp: "1800000010.000000", User: "UOTHER", Text: "is anyone looking at this?"}})
	f.post(slack.Message{Msg: slack.Msg{Timestamp: "1800000020.000000", ThreadTimestamp: alertSummary, User: "UOTHER", Text: "on it"}})

	if got := feedLines(t, f); len(got) != 0 {
		t.Fatalf("lines = %q, want none for posts an alerts subscription leaves out", got)
	}
}

func TestASentryIssueReadsOnceUntilItGoesQuiet(t *testing.T) {
	f := newFixture(t)
	subscribeFeed(t, f, feedAlerts)
	f.tick()
	sentry := func(ts string) slack.Message {
		return slack.Message{Msg: slack.Msg{Timestamp: ts, User: "USENTRY", BotID: "BSENTRY",
			Text: "[api] Error: billing-api is unreachable\n:red_circle: <https://example.sentry.io/issues/7841509141/?referrer=slack|*Error*>"}}
	}

	f.post(sentry("1800000010.000000"))
	f.post(sentry("1800000020.000000"))
	f.now = tsTime("1800000020.000000").Add(alertQuiet)
	f.tick()

	want := []string{`ALERT sentry issue 7841509141 "[api] Error: billing-api is unreachable" ` + f.id.permalink(testChannel, "1800000010.000000", "1800000010.000000")}
	if got := feedLines(t, f); !slices.Equal(got, want) {
		t.Fatalf("lines = %q, want %q and no close line", got, want)
	}
	f.post(sentry("1800002000.000000"))
	if got := feedLines(t, f); len(got) != 2 {
		t.Fatalf("lines after the issue went quiet and came back = %q, want a second", got)
	}
}

func TestAReleaseReadsEachStateItsPostIsEditedTo(t *testing.T) {
	f := newFixture(t)
	subscribeFeed(t, f, feedReleases, feedPosts)
	f.tick()
	link := f.id.permalink(testChannel, testRelease, testRelease)

	f.post(slack.Message{Msg: slack.Msg{Timestamp: testRelease, User: "UPLATY", BotID: "BPLATY", Text: runningLine}})
	f.slack.setText(testChannel, testRelease, finishedLine)
	f.poller.nudged(messageNudge{Channel: testChannel, TS: "1800000500.000000", Subtype: editedSubtype})
	f.tick()

	want := []string{"RELEASE stark-maclean is running · api " + link, "RELEASE stark-maclean is live · api · finished 10:25 PM " + link}
	if got := feedLines(t, f); !slices.Equal(got, want) {
		t.Fatalf("lines = %q, want %q", got, want)
	}
	replies := f.slack.callCount(methodReplies)
	f.poller.nudged(messageNudge{Channel: testChannel, TS: "1800000600.000000", Subtype: editedSubtype})
	f.tick()
	if n := f.slack.callCount(methodReplies); n != replies {
		t.Fatalf("replies calls = %d after an edit once the release finished, want none", n-replies)
	}
}

func TestParseRelease(t *testing.T) {
	tests := []struct {
		name    string
		first   string
		line    string
		running bool
		ok      bool
	}{
		{"running", "*:rocket: pulchritudinous-petri is running · <@U0000000002> · `web` · switches Acme &amp; Globex*", "pulchritudinous-petri is running · web · switches Acme & Globex", true, true},
		{"failed", "*:x: sanguine-neruda failed · <@U0000000001> via `claude` · `api` · finished 9:39 PM*", "sanguine-neruda failed · api · finished 9:39 PM", false, true},
		{"a hotfix live with a red check", "*:warning: :ambulance: Hotfix ultramarine-awoonor is live, but Smoke test on staging went red · <@U0000000001> via `claude` · `web` · finished 6:55 PM*", "Hotfix ultramarine-awoonor is live, but Smoke test on staging went red · web · finished 6:55 PM", false, true},
		{"a freeze", "*:ice_cube: Releases are frozen for 5 stacks · set by Ada Lovelace via Claude*", "", false, false},
		{"a break-glass grant", "Break-glass granted to <mailto:someone@example.com|someone@example.com>", "", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, ok := parseRelease(tt.first + "\n:black_nib: a poet.")
			if ok != tt.ok || r.line != tt.line || r.running != tt.running {
				t.Fatalf("parseRelease = %+v, %t; want line %q, running %t, %t", r, ok, tt.line, tt.running, tt.ok)
			}
		})
	}
}

func TestMonitorName(t *testing.T) {
	for text, want := range map[string]string{
		"*Monitor alert summary:* Cloudflare Origin Error Count (<http://app.example.com|app.example.com>)\n*First alerted:* now": "Cloudflare Origin Error Count (app.example.com)",
		"Re-Triggered: Edge Route Probe — staging\n`<https://edge.example.com/healthz>` has failed":                               "Edge Route Probe — staging",
		"No Data: Cloud executors idle\nno data for 15 minutes":                                                                   "Cloud executors idle",
	} {
		if got := monitorName(text); got != want {
			t.Errorf("monitorName(%q) = %q, want %q", text, got, want)
		}
	}
}
