package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func protocolFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "protocol", name))
	if err != nil {
		t.Fatal(err)
	}
	return bytes.TrimSpace(raw)
}

func decodePush(t *testing.T, name, eventType string, payload any) {
	t.Helper()
	var push socketPush
	if err := json.Unmarshal(protocolFixture(t, name), &push); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if push.Type != "" || push.EventType != eventType {
		t.Fatalf("%s: type %q, event_type %q; want an event push of %q", name, push.Type, push.EventType, eventType)
	}
	if err := json.Unmarshal(push.EventPayload, payload); err != nil {
		t.Fatalf("%s payload: %v", name, err)
	}
}

func TestProtocolHelloMatchesTheSpec(t *testing.T) {
	t.Parallel()
	for name, frame := range map[string]helloFrame{
		"hello.json":       {Type: helloType},
		"hello_since.json": {Type: helloType, Since: "001800000000000-7f3a9c"},
	} {
		got, err := json.Marshal(frame)
		if err != nil {
			t.Fatal(err)
		}
		if want := protocolFixture(t, name); !bytes.Equal(got, want) {
			t.Errorf("%s: the daemon sends %s, the spec says %s", name, got, want)
		}
	}
}

func TestProtocolHelloSendsSinceOnlyForARecentSeq(t *testing.T) {
	t.Parallel()
	now := time.UnixMilli(1800000000000)
	recent := &replayCursor{saved: "001800000000000-7f3a9c"}
	if got := recent.hello(now.Add(time.Minute)); got.Since != recent.saved {
		t.Fatalf("hello a minute after the saved seq = %+v, want since %q", got, recent.saved)
	}
	if got := recent.hello(now.Add(nudgeRetention)); got.Since != "" {
		t.Fatalf("hello past the log's retention = %+v, want no since", got)
	}
}

func TestProtocolCaughtUpIsATypedFrame(t *testing.T) {
	t.Parallel()
	var push socketPush
	if err := json.Unmarshal(protocolFixture(t, "caught_up.json"), &push); err != nil {
		t.Fatal(err)
	}
	if push.Type != caughtUpType || push.Seq != "001800000005000-0b12de" || push.EventType != "" {
		t.Fatalf("caught_up decodes to %+v", push)
	}
}

func TestProtocolMessageNudgeDecodes(t *testing.T) {
	t.Parallel()
	var n messageNudge
	decodePush(t, "message.json", nudgeEventType, &n)
	want := messageNudge{Channel: "C0123ABCDE", TS: "1800000000.000200", ThreadTS: "1800000000.000100", Seq: "001800000001000-a1b2c3"}
	if n != want {
		t.Fatalf("nudge = %+v, want %+v", n, want)
	}
	if !routedSubtypes[n.Subtype] {
		t.Fatalf("subtype %q is not routed", n.Subtype)
	}
}

func TestProtocolCardAskDecodes(t *testing.T) {
	t.Parallel()
	var a cardActionPush
	decodePush(t, "action_ask.json", cardActionEventType, &a)
	k, ok := a.tap()
	if !ok || k.ID != "merge-41" || k.Label != "Merge #41" || k.Expires != 1800003600 {
		t.Fatalf("ask tap = %+v, %v; want the ask's id, label, and expiry from its value", k, ok)
	}
	if !k.expired(time.Unix(1800003600, 0)) || k.expired(slackTime(a.ActionTS)) {
		t.Fatalf("ask expiring at %d: expiry misread", k.Expires)
	}
}

func TestProtocolCardActionDecodes(t *testing.T) {
	t.Parallel()
	var a cardActionPush
	decodePush(t, "action.json", cardActionEventType, &a)
	want := cardActionPush{
		User: "U0123ABCDE", Channel: "D0123ABCDE", MessageTS: "1800000000.000300", ThreadTS: "1800000000.000100",
		ActionID: cardOptionAction + "0", Value: "Ship it", ActionTS: "1800000002.123456", Seq: "001800000002000-d4e5f6",
	}
	if a != want {
		t.Fatalf("card action = %+v, want %+v", a, want)
	}
	if k, ok := a.tap(); !ok || k.Label != "Ship it" || k.ID != "" {
		t.Fatalf("tap = %+v, %v; want the option's value", k, ok)
	}
	if err := a.check(identity{UserID: "U0123ABCDE", DMChannelID: "D0123ABCDE"}); err != nil {
		t.Fatalf("check: %v", err)
	}
}

func TestProtocolShortcutPushValidates(t *testing.T) {
	t.Parallel()
	var p shortcutPayload
	decodePush(t, "shortcut.json", shortcutEventType, &p)
	want := shortcutPayload{
		User: "U0123ABCDE", Channel: "C0123ABCDE", ChannelName: "eng-help", TS: "1800000000.000200", ThreadTS: "1800000000.000100",
		ResponseURL: "https://hooks.slack.com/actions/T0123ABCDE/1/abc", DMTS: "1800000003.000400",
	}
	if !reflect.DeepEqual(p, want) {
		t.Fatalf("shortcut = %+v, want %+v", p, want)
	}
	if err := p.validate(identity{UserID: "U0123ABCDE"}, time.Unix(1800000003, 0).Add(time.Minute)); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

func TestProtocolShortcutMetadataTakesItsDMTSFromTheMessage(t *testing.T) {
	t.Parallel()
	var meta struct {
		EventType    string          `json:"event_type"`
		EventPayload shortcutPayload `json:"event_payload"`
	}
	if err := json.Unmarshal(protocolFixture(t, "shortcut_metadata.json"), &meta); err != nil {
		t.Fatal(err)
	}
	if meta.EventType != shortcutEventType || meta.EventPayload.DMTS != "" {
		t.Fatalf("metadata = %+v, want %s with no dm_ts of its own", meta, shortcutEventType)
	}
	meta.EventPayload.DMTS = "1800000003.000400"
	if err := meta.EventPayload.validate(identity{UserID: "U0123ABCDE"}, time.Unix(1800000003, 0)); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

func TestProtocolUserHeaderIsTheSpecHeader(t *testing.T) {
	t.Parallel()
	if got := http.CanonicalHeaderKey("x-cc-slack-user"); got != userHeader {
		t.Fatalf("user header = %q, want the canonical form of x-cc-slack-user, %q", userHeader, got)
	}
}
