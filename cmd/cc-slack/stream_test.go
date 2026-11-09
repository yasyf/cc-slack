package main

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/slack-go/slack"
)

const testStreamTS = "1791000000.000900"

func TestPickupShowsEyesAndTheWorkingLineTogetherAndNothingElse(t *testing.T) {
	l := startDaemon(t)
	loginAs(t, l, "yasyf")
	l.do(opPickup, map[string]string{"channel_id": testChannel, "ts": "1.2", "thread_ts": "1.1"})
	if got := l.slack.reactedTo("1.2"); !slices.Equal(got, []string{"eyes"}) {
		t.Fatalf("reactions on the picked-up message = %v, want eyes", got)
	}
	l.slack.mu.Lock()
	defer l.slack.mu.Unlock()
	if len(l.slack.statuses) != 1 || l.slack.statuses[0].ThreadTS != "1.1" || l.slack.statuses[0].Status != workingStatus {
		t.Fatalf("working line = %+v, want one %q under thread 1.1", l.slack.statuses, workingStatus)
	}
	for _, method := range []string{"chat.postMessage", "chat.startStream"} {
		if l.slack.calls[method] != 0 {
			t.Fatalf("a pickup called %s", method)
		}
	}
}

func TestAPickupWithAStateNamesItOnTheWorkingLine(t *testing.T) {
	l := startDaemon(t)
	loginAs(t, l, "yasyf")
	l.do(opPickup, map[string]string{"channel_id": testChannel, "ts": "1.2", "thread_ts": "1.1"})
	l.do(opPickup, map[string]string{"channel_id": testChannel, "ts": "1.2", "thread_ts": "1.1", "state": "Waiting for a subagent…"})
	l.slack.mu.Lock()
	defer l.slack.mu.Unlock()
	if got := l.slack.statuses[0].LoadingMessages; !slices.Equal(got, pickupLoading) {
		t.Fatalf("first working line = %v, want %v", got, pickupLoading)
	}
	if last := l.slack.statuses[len(l.slack.statuses)-1]; last.ThreadTS != "1.1" || !slices.Equal(last.LoadingMessages, []string{"Waiting for a subagent…"}) {
		t.Fatalf("working line after the state = %+v, want it to say Waiting for a subagent… under thread 1.1", last)
	}
}

func TestALookWithNoWorkTakesTheEyesAndLineAwayWithNoSteps(t *testing.T) {
	l := startDaemon(t)
	loginAs(t, l, "yasyf")
	l.do(opPickup, map[string]string{"channel_id": testChannel, "ts": "1.1"})
	l.do(opPickup, map[string]any{"channel_id": testChannel, "ts": "1.1", "done": true})
	if n := l.slack.callCount("reactions.remove"); n != 1 {
		t.Fatalf("reactions.remove calls = %d, want 1", n)
	}
	if n := l.slack.callCount("chat.startStream"); n != 0 {
		t.Fatalf("a look with no work started %d streams", n)
	}
	l.slack.mu.Lock()
	defer l.slack.mu.Unlock()
	if last := l.slack.statuses[len(l.slack.statuses)-1]; last.Status != "" || last.ThreadTS != "1.1" {
		t.Fatalf("last working line = %+v, want it cleared under thread 1.1", last)
	}
}

func TestWorkAddsSeparateStepsAndKeepsTheWorkingLineUp(t *testing.T) {
	l := startDaemon(t)
	dm := loginAs(t, l, "yasyf").DMChannelID
	checking := map[string]string{"id": "2", "status": taskInProgress, "title": "Checking the logs"}
	l.do(opStream, map[string]any{"channel_id": dm, "thread_ts": "1.1", "tasks": []any{checking}})
	l.do(opStream, map[string]any{"channel_id": dm, "thread_ts": "1.1", "ts": testStreamTS, "tasks": []any{checking}})
	l.do(opStream, map[string]any{"channel_id": dm, "thread_ts": "1.1", "ts": testStreamTS, "stop": true})
	l.slack.mu.Lock()
	defer l.slack.mu.Unlock()
	if got := l.slack.streams[0].Get("task_display_mode"); got != timelineMode {
		t.Fatalf("task_display_mode = %q, want %q", got, timelineMode)
	}
	want := []string{"chat.startStream", "assistant.threads.setStatus", "chat.appendStream", "assistant.threads.setStatus", "chat.stopStream", "assistant.threads.setStatus"}
	got := slices.DeleteFunc(slices.Clone(l.slack.sequence), func(method string) bool { return !slices.Contains(want, method) })
	if !slices.Equal(got, want) {
		t.Fatalf("calls = %v, want the line set after each stream call, since Slack clears it on a post: %v", got, want)
	}
	if got := l.slack.statuses[0]; got.Status != workingStatus || got.ThreadTS != "1.1" || !slices.Equal(got.LoadingMessages, []string{"Checking the logs…"}) {
		t.Fatalf("working line during the steps = %+v", got)
	}
	if got := l.slack.statuses[2]; got.Status != "" {
		t.Fatalf("working line after stop = %+v, want it cleared", got)
	}
}

func TestStepsInAChannelNeedARecipientAndATicket(t *testing.T) {
	l := startDaemon(t)
	loginAs(t, l, "yasyf")
	start := map[string]any{"channel_id": testChannel, "thread_ts": "1.1", "plan": "Looking at this"}
	l.refused(opStream, start, "needs recipient")
	start["recipient"] = "U0DANA"
	l.refused(opStream, start, "no ticket covers this post")
	if n := l.slack.callCount("chat.startStream"); n != 0 {
		t.Fatalf("a refused start reached Slack %d times", n)
	}
	l.do(opStream, l.hooked(opStream, start))
	l.slack.mu.Lock()
	defer l.slack.mu.Unlock()
	if got := l.slack.streams[0]; got.Get("recipient_user_id") != "U0DANA" || got.Get("recipient_team_id") != l.slack.auth.TeamID {
		t.Fatalf("stream form = %v, want the recipient and team", got)
	}
}

func TestARefusedWorkingLineKeepsTheStepsAndSaysSo(t *testing.T) {
	l := startDaemon(t)
	dm := loginAs(t, l, "yasyf").DMChannelID
	l.slack.fail("assistant.threads.setStatus", slack.SlackErrorResponse{Err: "not_allowed"})
	reply := l.do(opStream, map[string]any{"channel_id": dm, "thread_ts": "1.1", "plan": "Looking at this"})
	var res streamResult
	if err := json.Unmarshal(reply.Body, &res); err != nil {
		t.Fatal(err)
	}
	if res.TS != testStreamTS || res.LineError == "" {
		t.Fatalf("result = %+v, want the stream's ts and the line's refusal", res)
	}
}

func TestAPickupSlackRefusesTheWorkingLineForFailsAndSaysWhy(t *testing.T) {
	l := startDaemon(t)
	loginAs(t, l, "yasyf")
	l.slack.fail("assistant.threads.setStatus", slack.SlackErrorResponse{Err: "invalid_thread_ts"})
	l.refused(opPickup, map[string]string{"channel_id": testChannel, "ts": "1.2", "thread_ts": "1.1"}, "refused the working line under thread 1.1: invalid_thread_ts")
	if got := l.slack.reactedTo("1.2"); !slices.Equal(got, []string{"eyes"}) {
		t.Fatalf("reactions = %v, want the eyes kept", got)
	}
}

func TestPickupAndStepsRefuseAnAgentNameThatWouldShowAsWords(t *testing.T) {
	l := startDaemon(t)
	dm := loginAs(t, l, "yasyf").DMChannelID
	l.refused(opPickup, map[string]string{"channel_id": testChannel, "ts": "1.1", "agent": "Deploy Is Broken"}, "not a lowercase agent name")
	l.refused(opStream, map[string]string{"channel_id": dm, "thread_ts": "1.1", "agent": "Deploy Is Broken"}, "not a lowercase agent name")
	if n := l.slack.callCount("assistant.threads.setStatus") + l.slack.callCount("chat.startStream"); n != 0 {
		t.Fatalf("a refused agent name reached Slack %d times", n)
	}
}

func TestParseTaskKeepsPipesInDetails(t *testing.T) {
	got, err := parseTask("2|in_progress|Checking|logs | traces")
	if err != nil || got.ID != "2" || got.Status != "in_progress" || got.Title != "Checking" || got.Details != "logs | traces" {
		t.Fatalf("parseTask = %+v, %v", got, err)
	}
	if _, err := parseTask("2|in_progress"); err == nil {
		t.Fatal("parseTask accepted a task with no title")
	}
}
