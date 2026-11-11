package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yasyf/cc-interact/agent"
	"github.com/yasyf/cc-interact/daemon"
)

func TestWatchHandlerPrintsOnlySlackEvents(t *testing.T) {
	for _, tc := range []struct {
		name      string
		seq       int64
		typ       string
		lifecycle bool
		printed   bool
	}{
		{"slack message passes", 11, eventMessage, false, true},
		{"watch error passes", 11, eventWatchError, false, true},
		{"relay passes", 11, agent.EventRelay, false, true},
		{"owner stopped passes", 11, eventOwnerStopped, false, true},
		{"lifecycle event is filtered", 11, agent.EventStarted, false, false},
		{"owned message is filtered", 11, ownedType(eventMessage, "agent-1"), false, false},
		{"history is filtered", 10, eventMessage, false, false},
		{"lifecycle flag prints lifecycle events", 11, agent.EventStarted, true, true},
		{"lifecycle flag still skips history", 10, agent.EventStarted, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			data := `{"type":"` + tc.typ + `"}`
			handle := watchHandler(&out, 10, tc.lifecycle, false, false, func(string) bool { return false })
			if _, err := handle(tc.seq, data); err != nil {
				t.Fatal(err)
			}
			want := ""
			if tc.printed {
				want = data + "\n"
			}
			if got := out.String(); got != want {
				t.Fatalf("printed %q, want %q", got, want)
			}
		})
	}
}

func TestAStreamedMessagePrintsAsASlackMessageAndReachesNoChannel(t *testing.T) {
	var out bytes.Buffer
	handle := watchHandler(&out, 10, false, false, false, func(string) bool { return false })
	if _, err := handle(11, `{"type":"`+eventStreamed+`","ts":"1.0"}`); err != nil {
		t.Fatal(err)
	}
	if got, want := out.String(), `{"type":"`+eventMessage+`","ts":"1.0"}`+"\n"; got != want {
		t.Fatalf("printed %q, want %q", got, want)
	}
	if channelEvent(eventStreamed) {
		t.Fatal("a streamed message reached the channel")
	}
	if !channelEvent(eventMessage) {
		t.Fatal("a plain message left the channel")
	}
}

func TestWatchStartsAtTheLogHead(t *testing.T) {
	l := startDaemon(t)
	l.startAgent("agent-before")
	l.deliver(agent.TopLevel, "1800000000.000100", "before the watch")
	l.deps.WindowAlive = func(int) bool { return true }

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var out bytes.Buffer
	c := watchCmd(l.deps)
	c.SetArgs([]string{"--session", testSession, "--cwd", "/any/cwd", "--once"})
	c.SetOut(&out)
	done := make(chan error, 1)
	go func() { done <- c.ExecuteContext(ctx) }()

	for i := 0; ; i++ {
		l.startAgent(fmt.Sprintf("agent-%d", i))
		l.deliver(agent.TopLevel, fmt.Sprintf("1800000001.%06d", i), "after the watch")
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimSpace(out.String()), "\n")
			var m messagePayload
			if err := json.Unmarshal([]byte(lines[0]), &m); err != nil {
				t.Fatal(err)
			}
			if len(lines) != 1 || m.Type != eventMessage || m.Text != "after the watch" {
				t.Fatalf("watch printed %q, want one slack.message sent after it started", out.String())
			}
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func TestWatchUntilProvenExitsOnChannelAck(t *testing.T) {
	l := startDaemon(t)
	l.deps.WindowAlive = func(int) bool { return true }

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var out bytes.Buffer
	c := watchCmd(l.deps)
	c.SetArgs([]string{"--session", testSession, "--cwd", "/any/cwd", "--until-proven"})
	c.SetOut(&out)
	done := make(chan error, 1)
	go func() { done <- c.ExecuteContext(ctx) }()

	select {
	case err := <-done:
		t.Fatalf("watch exited before channel-ack: %v, printed %q", err, out.String())
	case <-time.After(3 * provenPoll):
	}
	l.do(daemon.OpChannelAck, nil)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("watch --until-proven never exited after channel-ack")
	}
	if got := strings.TrimSpace(out.String()); got != provenEvent {
		t.Fatalf("watch printed %q, want %s", got, provenEvent)
	}
}

func TestChannelEventAdmitsOnlyWhatTheSessionActsOn(t *testing.T) {
	for typ, want := range map[string]bool{
		eventMessage:                          true,
		eventAcked:                            true,
		eventAction:                           true,
		eventWatchError:                       true,
		agent.EventRelay:                      true,
		daemon.ProbeEventType:                 true,
		agent.EventStarted:                    false,
		agent.EventDirected:                   false,
		agent.EventDelivered:                  false,
		agent.EventStopped:                    false,
		ownedType(eventMessage, "agent-1"):    false,
		ownedType(eventWatchError, "agent-1"): false,
	} {
		if got := channelEvent(typ); got != want {
			t.Errorf("channelEvent(%q) = %v, want %v", typ, got, want)
		}
	}
}

func printedLine(t *testing.T, whole bool, event any) string {
	t.Helper()
	raw, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if _, err := watchHandler(&out, 10, false, false, whole, func(string) bool { return false })(11, string(raw)); err != nil {
		t.Fatal(err)
	}
	return strings.TrimSuffix(out.String(), "\n")
}

func fitLine(t *testing.T, event, target any) {
	t.Helper()
	printed := printedLine(t, false, event)
	if len(printed) > monitorEvent {
		t.Fatalf("line is %d bytes, want at most %d: %s", len(printed), monitorEvent, printed)
	}
	if err := json.Unmarshal([]byte(printed), target); err != nil {
		t.Fatalf("line %s: %v", printed, err)
	}
}

func longTap(label string) map[string]any {
	question := "Would a faster disk shorten production runs, and which of these should the next change measure first?"
	return map[string]any{
		"type": eventAction, "watch_id": 719, "channel_id": "D0B6Z", "thread_ts": "1793816299.111929", "ts": "1793816299.111929", "action_ts": "1793819436.927939",
		"label": label, "id": "card-faster-disk-runs", "waits": true, "question": question,
		"options":   []string{"Keep gp3 and measure write waits first", "Raise gp3's limits", "Move journals to io2", "Move to local NVMe nodes"},
		"capture":   "Yasyf answered " + strconv.Quote(question) + " with " + strconv.Quote(label) + " at 2026-11-05T05:10:36Z on Slack card https://acme.slack.com/archives/D0B6Z/p1793816299111929",
		"permalink": "https://acme.slack.com/archives/D0B6Z/p1793816299111929",
	}
}

type fittedTap struct {
	actionPayload
	Cut bool `json:"cut"`
}

func TestATapKeepsItsCardAndAnswerWholeAndDropsItsRepeatedWordsFirst(t *testing.T) {
	var line fittedTap

	fitLine(t, longTap("Keep gp3 and measure write waits first"), &line)

	if line.ID != "card-faster-disk-runs" || line.Label != "Keep gp3 and measure write waits first" || line.ActionTS != "1793819436.927939" || line.Cut {
		t.Fatalf("line = %+v, want the card id and the answer whole", line)
	}
	if line.Options != nil || line.Capture != "" {
		t.Fatalf("line = %+v, want the options and the capture gone first", line)
	}
	if !strings.HasPrefix(line.Question, "Would a faster disk") || line.Permalink == "" {
		t.Fatalf("line = %+v, want the question and permalink kept when they fit", line)
	}
}

func TestATapWithALongQuestionStillFitsAndNamesItsCard(t *testing.T) {
	tap := longTap("Yes")
	tap["question"] = strings.Repeat("why ", 200)
	var line fittedTap

	fitLine(t, tap, &line)

	if line.ID != "card-faster-disk-runs" || line.Label != "Yes" || line.Question != "" || line.Cut {
		t.Fatalf("line = %+v, want the question gone and the answer whole", line)
	}
}

func TestATypedAnswerTooLongForAnEventIsCutAndSaysSo(t *testing.T) {
	typed := strings.Repeat("port it first ", 60)
	tap := longTap(typed)
	tap["other"] = true
	var line fittedTap

	fitLine(t, tap, &line)

	if line.ID != "card-faster-disk-runs" || !line.Cut || !strings.HasPrefix(typed, line.Label) || len(line.Label) < 200 {
		t.Fatalf("line = %+v, want the card id whole and as much of the answer as fits, marked cut", line)
	}
}

func TestAMessageDropsItsThreadTitleBeforeItsWords(t *testing.T) {
	said := strings.Repeat("ship it ", 20)
	var line struct {
		messagePayload
		Cut bool `json:"cut"`
	}

	fitLine(t, messagePayload{Type: eventMessage, WatchID: 574, Kind: "thread", ChannelID: "D1", ThreadTS: "1.0", TS: "1.3", User: "U1", UserName: "Ria", Text: said, Permalink: "https://acme.slack.com/archives/D1/p13", ThreadOwner: &threadOwner{Session: "s1", Card: "card-x", Title: strings.Repeat("a long card title ", 20)}}, &line)

	if line.Text != said || line.Cut || line.ThreadOwner == nil || line.ThreadOwner.Card != "card-x" || line.ThreadOwner.Title != "" {
		t.Fatalf("line = %+v, want the words whole and the title gone", line)
	}
}

func TestALongMessageIsCutAndSaysSo(t *testing.T) {
	said := strings.Repeat("a teammate says <https://acme.slack.com|this> ", 30)
	var line struct {
		messagePayload
		Cut bool `json:"cut"`
	}

	fitLine(t, messagePayload{Type: eventMessage, WatchID: 574, Kind: "thread", ChannelID: "C1", ThreadTS: "1.0", TS: "1.3", User: "U2", UserName: "Ria", Text: said, Permalink: "https://acme.slack.com/archives/C1/p13"}, &line)

	if line.TS != "1.3" || line.ChannelID != "C1" || !line.Cut || !strings.HasPrefix(said, line.Text) || line.Permalink != "" {
		t.Fatalf("line = %+v", line)
	}
}

func TestALongWatchErrorIsCutAndNamesItsWatch(t *testing.T) {
	var line struct {
		watchErrorPayload
		Cut bool `json:"cut"`
	}

	fitLine(t, watchErrorPayload{Type: eventWatchError, WatchID: 742, Kind: "thread", ChannelID: "C1", ThreadTS: "1.0", Error: strings.Repeat("channel_not_found ", 60)}, &line)

	if line.WatchID != 742 || !line.Cut || !strings.HasPrefix(line.Error, "channel_not_found") {
		t.Fatalf("line = %+v", line)
	}
}

func TestAStoppedOwnerWithManyWatchesFitsAndSaysSomeAreGone(t *testing.T) {
	watches := make([]ownedWatch, 12)
	for i := range watches {
		watches[i] = ownedWatch{WatchID: int64(700 + i), Kind: "thread", ChannelID: "C0B7", ThreadTS: "1793774674.452900"}
	}
	var line struct {
		ownerStoppedPayload
		Cut bool `json:"cut"`
	}

	fitLine(t, ownerStoppedPayload{Type: eventOwnerStopped, AgentID: "a1b2", Watches: watches}, &line)

	if line.AgentID != "a1b2" || !line.Cut || len(line.Watches) == 0 || len(line.Watches) == len(watches) || line.Watches[0].WatchID != 700 {
		t.Fatalf("line = %+v, want the first watches kept", line)
	}
}

func TestALineThatFitsPrintsAsTheDaemonSentIt(t *testing.T) {
	event := map[string]any{"type": eventAction, "label": "Yes", "id": "card-x", "question": strings.Repeat("q", monitorEvent-65)}
	raw, _ := json.Marshal(event)
	if len(raw) != monitorEvent {
		t.Fatalf("fixture is %d bytes, want %d", len(raw), monitorEvent)
	}

	if printed := printedLine(t, false, event); printed != string(raw) {
		t.Fatalf("printed %s, want %s", printed, raw)
	}
}

func TestWholePrintsALongLineAsTheDaemonSentIt(t *testing.T) {
	event := longTap(strings.Repeat("port it first ", 60))
	raw, _ := json.Marshal(event)

	if printed := printedLine(t, true, event); printed != string(raw) {
		t.Fatalf("printed %d bytes, want the %d the daemon sent", len(printed), len(raw))
	}
}
