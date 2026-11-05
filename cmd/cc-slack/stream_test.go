package main

import "testing"

func TestThreadStatusAndStreamRefuseAnyChannelButTheDM(t *testing.T) {
	l := startDaemon(t)
	loginAs(t, l, "yasyf")
	l.refused(opThreadStatus, map[string]string{"channel_id": testChannel, "thread_ts": "1.1", "status": "is working…"}, "only in your DM")
	l.refused(opStream, map[string]any{"channel_id": testChannel, "thread_ts": "1.1", "plan": "Working"}, "only in your DM")
	if n := l.slack.callCount("assistant.threads.setStatus") + l.slack.callCount("chat.startStream"); n != 0 {
		t.Fatalf("a refused verb reached Slack %d times", n)
	}
}

func TestThreadStatusAndStreamRunInTheDM(t *testing.T) {
	l := startDaemon(t)
	dm := loginAs(t, l, "yasyf").DMChannelID
	reading := map[string]string{"id": "1", "status": "in_progress", "title": "Reading the thread"}
	l.do(opThreadStatus, map[string]any{"channel_id": dm, "thread_ts": "1.1", "status": "is working…", "loading": []string{"Reading"}})
	l.do(opStream, map[string]any{"channel_id": dm, "thread_ts": "1.1", "mode": "plan", "plan": "Working", "tasks": []any{reading}})
	l.do(opStream, map[string]any{"channel_id": dm, "ts": "1791000000.000900", "tasks": []any{reading}})
	l.do(opStream, map[string]any{"channel_id": dm, "ts": "1791000000.000900", "stop": true, "plan": "Done"})
	for _, method := range []string{"assistant.threads.setStatus", "chat.startStream", "chat.appendStream", "chat.stopStream"} {
		if n := l.slack.callCount(method); n != 1 {
			t.Fatalf("%s calls = %d, want 1", method, n)
		}
	}
	l.slack.mu.Lock()
	defer l.slack.mu.Unlock()
	if got := l.slack.statuses[0]; got.Status != "is working…" || got.ThreadTS != "1.1" || len(got.LoadingMessages) != 1 {
		t.Fatalf("status params = %+v", got)
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
