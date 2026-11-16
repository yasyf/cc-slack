package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	spawnllm "github.com/yasyf/spawnllm/go"
)

func TestVoiceFormat(t *testing.T) {
	tests := []struct {
		name string
		text string
		want string
	}{
		{"full title becomes number", "Reworking <https://github.com/acme/app/pull/53430|the run page and its step fill read a run journal's kept rows only>.", "Reworking <https://github.com/acme/app/pull/53430|#53430>."},
		{"few words stay beside number", "<https://github.com/acme/app/pull/7|reads old journals> is up", "<https://github.com/acme/app/pull/7|#7 reads old journals> is up"},
		{"number label kept", "<https://github.com/acme/app/pull/7|#7 the fix>", "<https://github.com/acme/app/pull/7|#7 the fix>"},
		{"bare link gets a label", "see https://github.com/acme/app/pull/12 now", "see <https://github.com/acme/app/pull/12|#12> now"},
		{"the writer's own words after the number stay", "<https://github.com/acme/app/pull/57746|#57746 staging's SandDB reads its own bucket>", "<https://github.com/acme/app/pull/57746|#57746 staging's SandDB reads its own bucket>"},
		{"a bare Slack link reads as its time", "Your card asks: https://acme.slack.com/archives/C0X/p1794869182955989?thread_ts=1794817747.843259&cid=C0X .", "Your card asks: <https://acme.slack.com/archives/C0X/p1794869182955989?thread_ts=1794817747.843259&cid=C0X|2:46 PM message>."},
		{"other links untouched", "<https://example.com/a|a long page title with many words>", "<https://example.com/a|a long page title with many words>"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := voiceFormat(tt.text); got != tt.want {
				t.Errorf("voiceFormat() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestVoiceMechanical(t *testing.T) {
	tests := []struct {
		name string
		text string
		want string
	}{
		{"clean", "Merged <https://github.com/acme/app/pull/7|#7>.", ""},
		{"slack token", "token xoxb-1234567890-abcdefghij", "secret: the post holds a token or key"},
		{"github token", "ghp_" + strings.Repeat("a", 36), "secret: the post holds a token or key"},
		{"unresolved ref", "see {pr:7|the fix}", "dead link: {pr:7|the fix} was never resolved"},
		{"localhost link", "open <http://localhost:8080/x|the page>", "dead link: <http://localhost:8080/x|the page> opens nothing he can reach"},
		{"file path link", "open </Users/me/x.md|the notes>", "dead link: </Users/me/x.md|the notes> opens nothing he can reach"},
		{"mention passes", "<@U0123ABCDE> look", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := voiceMechanical(tt.text); got != tt.want {
				t.Errorf("voiceMechanical() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestVoiceUnitsSplitSentencesOutsideLinks(t *testing.T) {
	units := voiceUnits("Right. It came from <https://x.example/a.b|two PRs. Both> written.\n\n- a bullet. Still one")
	texts := make([]string, 0, len(units))
	for _, u := range units {
		texts = append(texts, u.text)
	}
	want := []string{"Right.", "It came from <https://x.example/a.b|two PRs. Both> written.", "", "- a bullet. Still one"}
	if strings.Join(texts, "|") != strings.Join(want, "|") {
		t.Errorf("voiceUnits() = %q, want %q", texts, want)
	}
}

func TestVoiceApplied(t *testing.T) {
	text := "You're right. It came from how two PRs were written. Both are being reworked; the first is ready about 3 PM.\n\nThe delete stays held."
	units := voiceUnits(text)
	tests := []struct {
		name  string
		edits voiceEdits
		want  string
	}{
		{"no edits", voiceEdits{}, text},
		{"cut one", voiceEdits{cut: []string{"u2"}}, "You're right. Both are being reworked; the first is ready about 3 PM.\n\nThe delete stays held."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := voiceApplied(units, tt.edits); got != tt.want {
				t.Errorf("voiceApplied() =\n%q\nwant\n%q", got, tt.want)
			}
		})
	}
}

type stubDecide struct {
	answers map[string]spawnllm.Answer
	err     error
}

func (s stubDecide) decide(_ context.Context, _ any, _ map[string]spawnllm.Question, _ spawnllm.DecideOpts) (spawnllm.Decision, error) {
	return spawnllm.Decision{Answers: s.answers}, s.err
}

func TestVoiceAsk(t *testing.T) {
	units := voiceUnits("A. B. C. D.")
	tests := []struct {
		name string
		stub stubDecide
		want voiceEdits
	}{
		{"the opener is cut only when Jev is very sure", stubDecide{answers: map[string]spawnllm.Answer{"cut_u1": spawnllm.BinaryAnswer{PYes: 0.85}, "cut_u3": spawnllm.BinaryAnswer{PYes: 0.9}, "cut_u4": spawnllm.BinaryAnswer{PYes: 0.5}}}, voiceEdits{cut: []string{"u3"}}},
		{"every sure unit but the opener is cut", stubDecide{answers: map[string]spawnllm.Answer{"cut_u1": spawnllm.BinaryAnswer{PYes: 0.9}, "cut_u2": spawnllm.BinaryAnswer{PYes: 0.9}, "cut_u3": spawnllm.BinaryAnswer{PYes: 0.9}, "cut_u4": spawnllm.BinaryAnswer{PYes: 0.9}}}, voiceEdits{cut: []string{"u2", "u3", "u4"}}},
		{"a repeat of the last post", stubDecide{answers: map[string]spawnllm.Answer{"repeat": spawnllm.BinaryAnswer{PYes: 0.9}}}, voiceEdits{repeat: 0.9}},
		{"failed call edits nothing", stubDecide{err: errors.New("timeout")}, voiceEdits{note: "failed: timeout"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := &voiceSender{decide: tt.stub.decide}
			got := v.ask(context.Background(), "rules", "", "the earlier post", units)
			if got.repeat != tt.want.repeat || strings.Join(got.cut, ",") != strings.Join(tt.want.cut, ",") || got.note != tt.want.note {
				t.Errorf("ask() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestTidyVoiceDropsAHeadingWhoseSectionWasCut(t *testing.T) {
	got := tidyVoice("Gone now.\n\n*What changes now*\n• The delete stays held.\n\n*So it doesn't happen again*\n\n")
	if want := "Gone now.\n\n*What changes now*\n• The delete stays held."; got != want {
		t.Errorf("tidyVoice() = %q, want %q", got, want)
	}
}
