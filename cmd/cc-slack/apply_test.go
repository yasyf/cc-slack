package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/slack-go/slack"

	"github.com/yasyf/cc-interact/daemon"
)

const (
	ownerDM   = "D" + testUser
	staleTS   = "1800000000.000200"
	wordyTS   = "1800000000.000300"
	hisTS     = "1800000000.000400"
	elsewhere = "1800000000.000500"
	keptTS    = "1800000000.000600"
)

func botMessage(ts, text string) slack.Message {
	return slack.Message{Msg: slack.Msg{Timestamp: ts, ThreadTimestamp: ts, User: "UBOT", BotID: "BBOT", Text: text}}
}

func planLine(channelID, ts, verdict, newText string) map[string]any {
	return map[string]any{
		"ts": ts, "thread_ts": ts, "verdict": verdict, "why": "the cleanup pass", "new_text": newText,
		"permalink": "https://acme.slack.com/archives/" + channelID + "/p" + strings.ReplaceAll(ts, ".", ""),
	}
}

func planFile(t *testing.T, entries ...map[string]any) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cleanup.json")
	if err := os.WriteFile(path, mustJSON(entries), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func applyAgainst(t *testing.T, l *liveDaemon, plan string, dry bool) (*planRun, *[]time.Duration) {
	t.Helper()
	r, err := openPlanRun(plan, dry, applyVia(l.deps, testSession, "/any/cwd"), testWriter{t})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.close)
	var waits []time.Duration
	r.sleep = func(_ context.Context, wait time.Duration) error {
		waits = append(waits, wait)
		return nil
	}
	return r, &waits
}

func runPlan(t *testing.T, r *planRun) applyTally {
	t.Helper()
	tally, err := r.run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return tally
}

func readLines[T any](t *testing.T, path string) []T {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var out []T
	lines := bufio.NewScanner(f)
	lines.Buffer(nil, 1<<20)
	for lines.Scan() {
		var v T
		if err := json.Unmarshal(lines.Bytes(), &v); err != nil {
			t.Fatal(err)
		}
		out = append(out, v)
	}
	if err := lines.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func archivedMessages(t *testing.T, path string) []slack.Message {
	t.Helper()
	lines := readLines[archivedMessage](t, path)
	out := make([]slack.Message, 0, len(lines))
	for _, line := range lines {
		var m slack.Message
		if err := json.Unmarshal(line.Message, &m); err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

func doneOutcomes(t *testing.T, path string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, e := range readLines[doneEntry](t, path) {
		out[e.TS] = e.Outcome
	}
	return out
}

func deletedTS(l *liveDaemon) []string {
	deleted := l.slack.deleted()
	out := make([]string, 0, len(deleted))
	for _, d := range deleted {
		out = append(out, d.ts)
	}
	return out
}

func TestApplyChangesOnlyTheBotsOwnPostsInTheOwnersDM(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	l.slack.add(ownerDM, botMessage(staleTS, "stale status"))
	l.slack.add(ownerDM, slack.Message{Msg: slack.Msg{Timestamp: hisTS, User: testUser, Text: "his own words"}})
	l.slack.add(testChannel, botMessage(elsewhere, "a post in a channel"))

	plan := planFile(t, planLine(ownerDM, staleTS, verdictDelete, ""), planLine(ownerDM, hisTS, verdictDelete, ""))
	r, _ := applyAgainst(t, l, plan, false)
	if tally, want := runPlan(t, r), (applyTally{deleted: 1, refused: 1}); tally != want {
		t.Fatalf("tally = %+v, want %+v", tally, want)
	}
	if got := deletedTS(l); !slices.Equal(got, []string{staleTS}) {
		t.Fatalf("chat.delete reached %v, want only the bot's %s", got, staleTS)
	}
	if got, want := doneOutcomes(t, planFilesFor(plan).done), map[string]string{staleTS: outcomeDeleted, hisTS: outcomeRefused}; !maps.Equal(got, want) {
		t.Fatalf("done log = %v, want %v", got, want)
	}

	for _, body := range []map[string]any{
		{"channel_id": ownerDM, "ts": hisTS, "verdict": verdictDelete},
		{"channel_id": ownerDM, "ts": hisTS, "verdict": verdictRewrite, "text": "words he never wrote"},
		{"channel_id": testChannel, "ts": elsewhere, "verdict": verdictDelete},
	} {
		var res applyResult
		if err := json.Unmarshal(l.do(opApplyChange, body).Body, &res); err != nil {
			t.Fatal(err)
		}
		if res.Outcome != outcomeRefused || res.Reason == "" {
			t.Fatalf("apply-change %v = %+v, want it refused with a reason", body, res)
		}
	}
	if deletes, edits := l.slack.callCount("chat.delete"), l.slack.callCount("chat.update"); deletes != 1 || edits != 0 {
		t.Fatalf("chat.delete %d, chat.update %d, want only the one delete of the bot's post", deletes, edits)
	}
}

func TestApplyArchivesEachMessageBeforeChangingIt(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	l.slack.add(ownerDM, botMessage(staleTS, "stale status"))
	l.slack.add(ownerDM, botMessage(wordyTS, "a wordy card"))

	plan := planFile(t, planLine(ownerDM, staleTS, verdictDelete, ""), planLine(ownerDM, wordyTS, verdictRewrite, "plain words"), planLine(ownerDM, keptTS, verdictKeep, ""))
	archive := planFilesFor(plan).archive
	r, _ := applyAgainst(t, l, plan, false)
	call := r.call
	var changed []string
	r.call = func(ctx context.Context, op daemon.Op, body map[string]any) (applyResult, error) {
		if op == opApplyChange {
			ts, _ := body["ts"].(string)
			changed = append(changed, ts)
			if !slices.ContainsFunc(archivedMessages(t, archive), func(m slack.Message) bool { return m.Timestamp == ts }) {
				t.Errorf("%s reached apply-change before its archive line was written", ts)
			}
		}
		return call(ctx, op, body)
	}
	if tally, want := runPlan(t, r), (applyTally{deleted: 1, rewritten: 1, skipped: 1}); tally != want {
		t.Fatalf("tally = %+v, want %+v", tally, want)
	}
	if !slices.Equal(changed, []string{staleTS, wordyTS}) {
		t.Fatalf("changed %v, want %s then %s", changed, staleTS, wordyTS)
	}
	archived := archivedMessages(t, archive)
	texts := make([]string, 0, len(archived))
	for _, m := range archived {
		texts = append(texts, m.Timestamp+" "+m.Text)
	}
	if want := []string{staleTS + " stale status", wordyTS + " a wordy card"}; !slices.Equal(texts, want) {
		t.Fatalf("archive = %q, want %q", texts, want)
	}
	if edits := l.slack.edited(); len(edits) != 1 || edits[0].ts != wordyTS || edits[0].text != "plain words" {
		t.Fatalf("chat.update = %+v, want %s's text replaced with the plan's", edits, wordyTS)
	}
}

func TestApplyRerunSkipsEntriesAlreadyDone(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	l.slack.add(ownerDM, botMessage(staleTS, "stale status"))
	l.slack.add(ownerDM, botMessage(wordyTS, "another stale status"))

	plan := planFile(t, planLine(ownerDM, staleTS, verdictDelete, ""), planLine(ownerDM, wordyTS, verdictDelete, ""))
	done := planFilesFor(plan).done
	if err := os.WriteFile(done, append(mustJSON(doneEntry{TS: staleTS, Outcome: outcomeDeleted}), '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	first, _ := applyAgainst(t, l, plan, false)
	if tally, want := runPlan(t, first), (applyTally{deleted: 1, skipped: 1}); tally != want {
		t.Fatalf("first run tally = %+v, want %+v", tally, want)
	}
	if got := deletedTS(l); !slices.Equal(got, []string{wordyTS}) {
		t.Fatalf("chat.delete reached %v, want only %s, since the done log holds %s", got, wordyTS, staleTS)
	}
	reads := l.slack.callCount(methodRawMessage)

	again, _ := applyAgainst(t, l, plan, false)
	if tally, want := runPlan(t, again), (applyTally{skipped: 2}); tally != want {
		t.Fatalf("rerun tally = %+v, want %+v", tally, want)
	}
	if n := l.slack.callCount(methodRawMessage); n != reads {
		t.Fatalf("the rerun read %d messages, want none", n-reads)
	}
	if got, want := doneOutcomes(t, done), map[string]string{staleTS: outcomeDeleted, wordyTS: outcomeDeleted}; !maps.Equal(got, want) {
		t.Fatalf("done log = %v, want %v", got, want)
	}
}

func TestApplyWaitsOutSlacksRetryAfter(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	l.slack.add(ownerDM, botMessage(staleTS, "stale status"))
	l.slack.fail(methodRawMessage, &slack.RateLimitedError{RetryAfter: 3 * time.Second})
	l.slack.fail("chat.delete", &slack.RateLimitedError{RetryAfter: 7 * time.Second})

	r, waits := applyAgainst(t, l, planFile(t, planLine(ownerDM, staleTS, verdictDelete, "")), false)
	if tally, want := runPlan(t, r), (applyTally{deleted: 1}); tally != want {
		t.Fatalf("tally = %+v, want %+v", tally, want)
	}
	if want := []time.Duration{3 * time.Second, 7 * time.Second}; !slices.Equal(*waits, want) {
		t.Fatalf("waits = %v, want Slack's Retry-After before each retry, %v", *waits, want)
	}
	if reads, deletes := l.slack.callCount(methodRawMessage), l.slack.callCount("chat.delete"); reads != 2 || deletes != 2 {
		t.Fatalf("reads %d, chat.delete %d, want each tried again once after its wait", reads, deletes)
	}
}

func TestApplyDryRunChangesNothing(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	l.slack.add(ownerDM, botMessage(staleTS, "stale status"))
	l.slack.add(ownerDM, botMessage(wordyTS, "a wordy card"))
	l.slack.add(ownerDM, slack.Message{Msg: slack.Msg{Timestamp: hisTS, User: testUser, Text: "his own words"}})

	plan := planFile(t,
		planLine(ownerDM, staleTS, verdictDelete, ""), planLine(ownerDM, wordyTS, verdictRewrite, "plain words"),
		planLine(ownerDM, hisTS, verdictDelete, ""), planLine(ownerDM, keptTS, verdictKeep, ""),
	)
	r, _ := applyAgainst(t, l, plan, true)
	tally := runPlan(t, r)
	if want := (applyTally{deleted: 1, rewritten: 1, skipped: 1, refused: 1}); tally != want {
		t.Fatalf("tally = %+v, want %+v", tally, want)
	}
	if got, want := tally.summary(true), "dry run: would delete 1, would rewrite 1, skipped 1, refused 1, failed 0"; got != want {
		t.Fatalf("summary = %q, want %q", got, want)
	}
	if deletes, edits := l.slack.callCount("chat.delete"), l.slack.callCount("chat.update"); deletes != 0 || edits != 0 {
		t.Fatalf("chat.delete %d, chat.update %d, want a dry run to change nothing", deletes, edits)
	}
	for _, path := range []string{planFilesFor(plan).archive, planFilesFor(plan).done} {
		if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("stat %s = %v, want a dry run to write no file", path, err)
		}
	}
}

func TestLoadPlanRefusesWhatItCannotApply(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		entries []map[string]any
		err     string
	}{
		{"two channels", []map[string]any{planLine(ownerDM, staleTS, verdictDelete, ""), planLine("D0SOMEONE01", wordyTS, verdictDelete, "")}, "plan names two channels"},
		{"a post verdict", []map[string]any{planLine(ownerDM, staleTS, "post", "")}, `verdict "post" is not delete, rewrite, or keep`},
		{"a rewrite with no text", []map[string]any{planLine(ownerDM, staleTS, verdictRewrite, "")}, "a rewrite with no new_text"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := loadPlan(planFile(t, tc.entries...)); err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Fatalf("loadPlan = %v, want an error naming %q", err, tc.err)
			}
		})
	}
}

func TestRawMessageReadsOneMessageAndHandsBackSlacksRetryAfter(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		status int
		answer string
		err    error
	}{
		{"a message in the thread", http.StatusOK, `{"ok":true,"messages":[{"ts":"` + testThread + `","text":"parent"},{"ts":"` + staleTS + `","text":"stale status"}]}`, nil},
		{"a message the thread no longer holds", http.StatusOK, `{"ok":true,"messages":[{"ts":"` + testThread + `","text":"parent"}]}`, errMessageGone},
		{"a rate-limited read", http.StatusTooManyRequests, "", &slack.RateLimitedError{RetryAfter: 9 * time.Second}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			asked := make(chan string, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				asked <- r.URL.Path + "?" + r.URL.Query().Encode()
				if tc.status == http.StatusTooManyRequests {
					w.Header().Set("Retry-After", "9")
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.answer))
			}))
			t.Cleanup(srv.Close)
			raw, err := newSlackClient("xoxb-test", srv.URL+"/", srv.Client()).RawMessage(withoutRateWait(context.Background()), ownerDM, testThread, staleTS)
			wantAsked := "/conversations.replies?channel=" + ownerDM + "&include_all_metadata=true&inclusive=true&latest=" + staleTS + "&limit=200&oldest=" + staleTS + "&ts=" + testThread
			if got := <-asked; got != wantAsked {
				t.Fatalf("asked %s, want %s", got, wantAsked)
			}
			if rate, ok := errors.AsType[*slack.RateLimitedError](tc.err); ok {
				if got, limited := errors.AsType[*slack.RateLimitedError](err); !limited || got.RetryAfter != rate.RetryAfter {
					t.Fatalf("err = %v, want Slack's Retry-After %s handed back", err, rate.RetryAfter)
				}
				return
			}
			if !errors.Is(err, tc.err) {
				t.Fatalf("err = %v, want %v", err, tc.err)
			}
			if tc.err == nil && !strings.Contains(string(raw), `"text":"stale status"`) {
				t.Fatalf("raw = %s, want the message as Slack sent it", raw)
			}
		})
	}
}
