package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/slack-go/slack"

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

func TestLeanedKeepsTheSentenceALaterOneRefersBackTo(t *testing.T) {
	post := map[string]string{"u1": "A builder is on Josh's Mac.", "u2": "Its first step is checking his build.", "u3": "If it does, I'll tell you.", "u4": "I'm fixing the tooling.", "u5": "The release waits on you."}
	got := leaned([]string{"u1", "u2", "u3", "u4", "u5"}, post, []string{"u2", "u4"})
	if strings.Join(got, ",") != "u4" {
		t.Errorf("leaned() = %v, want [u4]", got)
	}
}

func TestVoiceQuestionCarriesAButtonPerOptionAndAnsweringLeavesOnlyHisPick(t *testing.T) {
	blocks := voiceQuestionBlocks("*Hotfix #57810 now, or wait for the Iris hotfix?*\n\nThe hotfix ships in 20 minutes.", []string{"Hotfix", "Wait"})
	actions, ok := blocks[len(blocks)-1].(*slack.ActionBlock)
	if !ok || len(actions.Elements.ElementSet) != 2 {
		t.Fatalf("last block = %#v, want an action block with two buttons", blocks[len(blocks)-1])
	}
	first := actions.Elements.ElementSet[0].(*slack.ButtonBlockElement)
	if first.ActionID != voiceOptionAction+"0" || first.Value != "Hotfix" || first.Style != slack.StylePrimary {
		t.Fatalf("first button = %+v", first)
	}
	answered := voiceAnswered(blocks, "Hotfix", time.Date(2026, 11, 17, 1, 5, 0, 0, time.UTC))
	if len(answered) != len(blocks) || answered[len(answered)-1].BlockType() != slack.MBTContext {
		t.Fatalf("answered = %#v, want the buttons replaced by one context line", answered)
	}
}

func TestVoiceOptionsAreOneToFiveShortLabels(t *testing.T) {
	for _, options := range [][]string{{"a", "b", "c", "d", "e", "f"}, {" "}, {strings.Repeat("x", voiceOptionRunes+1)}} {
		if voiceOptionsCheck(options) == nil {
			t.Errorf("voiceOptionsCheck(%q) = nil, want a refusal", options)
		}
	}
	if err := voiceOptionsCheck([]string{"Hotfix", "Wait"}); err != nil {
		t.Errorf("voiceOptionsCheck = %v", err)
	}
}

func TestVoiceHoldsATypedChoiceOrAMiscountAndLetsItThroughWhenJevCannotAnswer(t *testing.T) {
	for _, tt := range []struct {
		stub    stubDecide
		options []string
		want    string
	}{
		{stubDecide{answers: map[string]spawnllm.Answer{"typed": spawnllm.BinaryAnswer{PYes: 0.95}}}, nil, voiceTypedChoice},
		{stubDecide{answers: map[string]spawnllm.Answer{"miscount": spawnllm.BinaryAnswer{PYes: 0.9}}}, []string{"Merge", "Hold"}, voiceMiscount},
		{stubDecide{answers: map[string]spawnllm.Answer{"typed": spawnllm.BinaryAnswer{PYes: 0.2}, "miscount": spawnllm.BinaryAnswer{PYes: 0.1}}}, nil, ""},
		{stubDecide{err: errors.New("timeout")}, nil, ""},
	} {
		v := &voiceSender{decide: tt.stub.decide}
		if got, _ := v.held(context.Background(), voiceRequest{Text: "All six are green: #1, #2.", Options: tt.options}); got != tt.want {
			t.Errorf("held with %+v = %q, want %q", tt.stub, got, tt.want)
		}
	}
}

func TestVoiceAnAskForHisApproveGetsItsButtonsAndNeverNamesASettledPR(t *testing.T) {
	asks := stubDecide{answers: map[string]spawnllm.Answer{"approve": spawnllm.BinaryAnswer{PYes: 0.95}, "typed": spawnllm.BinaryAnswer{PYes: 0.9}}}
	v := &voiceSender{decide: asks.decide}
	why, options := v.held(context.Background(), voiceRequest{Text: "#57894 is green and needs your Approve to merge.", PRs: []voicePR{{Repo: "Forge-AI/monorepo", Number: 57894, State: "OPEN"}}})
	if why != "" || strings.Join(options, "|") != "Approve #57894|Hold #57894" {
		t.Fatalf("held = %q, %q; want the Approve and Hold buttons attached", why, options)
	}
	why, options = v.held(context.Background(), voiceRequest{Text: "#57894 needs your Approve.", PRs: []voicePR{{Repo: "Forge-AI/monorepo", Number: 57894, State: "MERGED"}}})
	if !strings.Contains(why, "#57894 is merged") || options != nil {
		t.Fatalf("held = %q, %q; want a refusal naming the merged PR", why, options)
	}
}

func TestVoiceSettledReplacesTheStateLineAndDropsTheButtons(t *testing.T) {
	question := voiceQuestionBlocks("*Approve #57894?*", []string{"Approve #57894", "Hold #57894"})
	once := voiceSettled(question, "*Approve #57894?*", "✅ #57894 approved", false)
	twice := voiceSettled(once, "*Approve #57894?*", "✅ #57894 merged 3:18 PM", false)
	if len(twice) != 2 || twice[0].BlockType() != slack.MBTSection || twice[1].(*slack.ContextBlock).ContextElements.Elements[0].(*slack.TextBlockObject).Text != "✅ #57894 merged 3:18 PM" {
		t.Fatalf("settled twice = %#v, want the section and one state line, no buttons", twice)
	}
	plain := voiceSettled([]slack.Block{slack.NewRichTextBlock("r")}, "#57894 needs your Approve.", "✅ #57894 merged 3:18 PM", false)
	if len(plain) != 2 || plain[0].BlockType() != slack.MBTSection {
		t.Fatalf("a plain post settled = %#v, want its text as a section and the state line", plain)
	}
}
