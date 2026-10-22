package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/yasyf/cc-interact/daemon"
)

const (
	testTree     = "root-session"
	testCaptHook = "/opt/capt-hook/bin/capt-hook"
	reachPlace   = "place"
)

type fakeSpend struct {
	session     string
	agent       string
	call        string
	fingerprint string
	summary     string
	released    bool
}

type fakeTicket struct {
	tree        string
	place       map[string]string
	reach       string
	rule        string
	fingerprint string
	revoked     bool
	spends      []fakeSpend
}

type fakeCaptHook struct {
	mu         sync.Mutex
	tickets    map[string]*fakeTicket
	calls      [][]string
	releaseErr error
}

func newFakeCaptHook() *fakeCaptHook { return &fakeCaptHook{tickets: map[string]*fakeTicket{}} }

func (f *fakeCaptHook) mint(t fakeTicket) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := fmt.Sprintf("%012x", len(f.tickets)+1)
	f.tickets[id] = &t
	return id
}

func (f *fakeCaptHook) spent(id string) []fakeSpend {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.tickets[id].spends)
}

func (f *fakeCaptHook) committed(id string) []fakeSpend {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.DeleteFunc(slices.Clone(f.tickets[id].spends), func(s fakeSpend) bool { return s.released })
}

func (f *fakeCaptHook) lastCall() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[len(f.calls)-1]
}

func (f *fakeCaptHook) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func cliFlags(args []string) map[string][]string {
	flags := map[string][]string{}
	for i := 0; i+1 < len(args); i += 2 {
		flags[strings.TrimPrefix(args[i], "--")] = append(flags[strings.TrimPrefix(args[i], "--")], args[i+1])
	}
	return flags
}

func cliPlace(flags map[string][]string) map[string]string {
	place := map[string]string{}
	for _, pair := range flags["place"] {
		key, value, _ := strings.Cut(pair, "=")
		place[key] = value
	}
	return place
}

func (t *fakeTicket) record(id string) map[string]any {
	return map[string]any{
		"id": id, "kind": "slack.write", "tree": t.tree, "place": t.place, "reach": t.reach, "rule": t.rule,
		"evidence": []any{}, "fingerprint": t.fingerprint, "author": "user", "created": "2026-10-02T12:00:00Z",
	}
}

func (f *fakeCaptHook) run(_ context.Context, bin string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, append([]string{bin}, args...))
	if args[0] != "ticket" {
		return nil, fmt.Errorf("fake capt-hook has no %v", args)
	}
	switch args[1] {
	case "spend":
		return f.spend(args[2], cliFlags(args[3:]))
	case "release":
		if f.releaseErr != nil {
			return nil, f.releaseErr
		}
		t := f.tickets[args[2]]
		released := 0
		for i, s := range t.spends {
			if s.call == args[4] && !s.released {
				t.spends[i].released = true
				released++
			}
		}
		return json.Marshal(map[string]any{"ticket": t.record(args[2]), "released": released})
	case "revoke":
		t, found := f.tickets[args[2]]
		if !found {
			return nil, fmt.Errorf("KeyError: 'no ticket %s'", args[2])
		}
		t.revoked = true
		return []byte(args[2] + "  slack.write  revoked\n"), nil
	}
	return nil, fmt.Errorf("fake capt-hook has no %v", args)
}

func (f *fakeCaptHook) spend(id string, flags map[string][]string) ([]byte, error) {
	t, found := f.tickets[id]
	if !found {
		return nil, fmt.Errorf("no ticket %s exists", id)
	}
	fingerprint := flags["fingerprint"][0]
	if t.tree != flags["tree"][0] || !maps.Equal(t.place, cliPlace(flags)) || (t.reach != reachPlace && t.fingerprint != fingerprint) {
		return nil, fmt.Errorf("ticket %s covers %v in session tree %s, which does not include this action", id, t.place, t.tree)
	}
	if t.revoked {
		return nil, fmt.Errorf("ticket %s was revoked at 2026-10-02 12:00 PM", id)
	}
	call := flags["call"][0]
	if !slices.ContainsFunc(t.spends, func(s fakeSpend) bool { return s.call == call }) {
		t.spends = append(t.spends, fakeSpend{
			session: flags["session"][0], agent: flags["agent"][0], call: call, fingerprint: fingerprint, summary: flags["summary"][0],
		})
	}
	return json.Marshal(map[string]any{"ticket": t.record(id)})
}

func field(body map[string]any, key string) string {
	value, _ := body[key].(string)
	return value
}

func asBody(body any) map[string]any {
	raw, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}
	fields := map[string]any{}
	if err := json.Unmarshal(raw, &fields); err != nil {
		panic(err)
	}
	return fields
}

func placeOf(op daemon.Op, body map[string]any) map[string]string {
	raw, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}
	var b opBody
	if err := json.Unmarshal(raw, &b); err != nil {
		panic(err)
	}
	if err := b.fillFromPermalink(); err != nil {
		panic(err)
	}
	switch op {
	case opEdit, opDelete:
		return messagePlace(b.ChannelID, b.messageThread())
	case opChannelCreate:
		return map[string]string{"channel": b.Name, "thread": ""}
	case opChannelArchive, opChannelInvite, opChannelTopic, opChannelJoin:
		return map[string]string{"channel": b.ChannelID, "thread": ""}
	}
	return messagePlace(b.ChannelID, b.ThreadTS)
}

func messagePlace(channelID, threadTS string) map[string]string {
	if strings.HasPrefix(channelID, "U") || strings.HasPrefix(channelID, "W") {
		threadTS = ""
	}
	return map[string]string{"channel": channelID, "thread": threadTS}
}

func fingerprintOf(fields map[string]any) string {
	payload := maps.Clone(fields)
	maps.DeleteFunc(payload, func(key string, _ any) bool { return strings.HasPrefix(key, "ticket_") || key == "no_watch" })
	raw, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:8])
}

func (l *liveDaemon) spending(ticketID string, body any) map[string]any {
	fields := asBody(body)
	fields["ticket_id"], fields["ticket_tree"], fields["ticket_agent"] = ticketID, testTree, "main"
	fields["ticket_call"], fields["ticket_fingerprint"] = "toolu_"+ticketID, fingerprintOf(fields)
	return fields
}

func retried(body map[string]any) map[string]any {
	again := maps.Clone(body)
	again["ticket_call"] = field(body, "ticket_call") + "-retry"
	return again
}

func (l *liveDaemon) hooked(op daemon.Op, body any) map[string]any {
	fields := asBody(body)
	return l.spending(l.tickets.mint(fakeTicket{tree: testTree, place: placeOf(op, fields), reach: reachOnce, rule: "asked-owner", fingerprint: fingerprintOf(fields)}), fields)
}

func (l *liveDaemon) standing(threadTS string) string {
	return l.placeTicket("owner-said-reply-there", threadTS)
}

func (l *liveDaemon) owedUpdate(threadTS string) string {
	return l.placeTicket(owedUpdateRule, threadTS)
}

func (l *liveDaemon) placeTicket(rule, threadTS string) string {
	return l.tickets.mint(fakeTicket{tree: testTree, place: map[string]string{"channel": testChannel, "thread": threadTS}, reach: reachPlace, rule: rule})
}

func (l *liveDaemon) post(op daemon.Op, body any) daemon.Reply {
	l.t.Helper()
	return l.do(op, l.hooked(op, body))
}

func (l *liveDaemon) refused(op daemon.Op, body any, want string) {
	l.t.Helper()
	raw, _ := json.Marshal(body)
	if _, err := invoke(l.ctx, l.deps, testSession, "/any", op, raw); err == nil || !strings.Contains(err.Error(), want) {
		l.t.Fatalf("%s %v: got %v, want an error naming %q", op, body, err, want)
	}
}

func TestExecCommandReportsTheLastLineOfStderr(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	script := filepath.Join(dir, "capt-hook")
	body := "#!/bin/sh\nif [ \"$1\" = ok ]; then echo '{\"ticket\":{}}'; exit 0; fi\n" +
		"echo 'Traceback (most recent call last):' >&2\necho 'Error: Ticket 0123456789ab was revoked at 2026-10-02 12:00 PM.' >&2\nexit 1\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	out, err := execCommand(context.Background(), script, "ok")
	if err != nil || strings.TrimSpace(string(out)) != `{"ticket":{}}` {
		t.Fatalf("ok run = %q, %v", out, err)
	}
	_, err = execCommand(context.Background(), script, "ticket", "spend")
	if err == nil || err.Error() != "Ticket 0123456789ab was revoked at 2026-10-02 12:00 PM." {
		t.Fatalf("refused run = %v, want the reason alone", err)
	}
	var exitErr interface{ ExitCode() int }
	if _, err := execCommand(context.Background(), filepath.Join(dir, "missing")); err == nil || errors.As(err, &exitErr) {
		t.Fatalf("missing binary = %v, want the exec error", err)
	}
}
