package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
		{"a long label stays whole beside its number", "Reworking <https://github.com/acme/app/pull/53430|the run page and its step fill read a run journal's kept rows only>.", "Reworking <https://github.com/acme/app/pull/53430|#53430 the run page and its step fill read a run journal's kept rows only>."},
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
	first, _ := actions.Elements.ElementSet[0].(*slack.ButtonBlockElement)
	if first.ActionID != voiceOptionAction+"0" || first.Value != "Hotfix" || first.Style != slack.StylePrimary {
		t.Fatalf("first button = %+v", first)
	}
	answered := voiceAnswered(blocks, "Hotfix", time.Date(2026, 11, 17, 1, 5, 0, 0, time.UTC))
	if len(answered) != len(blocks) || answered[len(answered)-1].BlockType() != slack.MBTContext {
		t.Fatalf("answered = %#v, want the buttons replaced by one context line", answered)
	}
}

func TestVoiceOptionsAreOneToFiveDistinctLabels(t *testing.T) {
	for _, options := range [][]string{{"a", "b", "c", "d", "e", "f"}, {" "}, {"Hold", "hold"}} {
		if voiceOptionsCheck(options) == nil {
			t.Errorf("voiceOptionsCheck(%q) = nil, want a refusal", options)
		}
	}
	if err := voiceOptionsCheck([]string{"Approve", "Approve + sidebar", "Make optional", "Hold"}); err != nil {
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
		if got, _, _, _ := v.held(context.Background(), voiceRequest{Text: "All six are green: #1, #2.", Options: tt.options}); got != tt.want {
			t.Errorf("held with %+v = %q, want %q", tt.stub, got, tt.want)
		}
	}
}

func TestVoiceAnAskForHisApproveGetsItsButtonsAndNeverNamesASettledPR(t *testing.T) {
	asks := stubDecide{answers: map[string]spawnllm.Answer{"approve": spawnllm.BinaryAnswer{PYes: 0.95}, "typed": spawnllm.BinaryAnswer{PYes: 0.9}}}
	v := &voiceSender{decide: asks.decide}
	why, options, _, _ := v.held(context.Background(), voiceRequest{Text: "#57894 is green and needs your Approve to merge.", PRs: []voicePR{{Repo: "Forge-AI/monorepo", Number: 57894, State: "OPEN"}}})
	if why != "" || strings.Join(options, "|") != "Approve #57894|Hold #57894" {
		t.Fatalf("held = %q, %q; want the Approve and Hold buttons attached", why, options)
	}
	why, options, _, _ = v.held(context.Background(), voiceRequest{Text: "#57894 needs your Approve.", PRs: []voicePR{{Repo: "Forge-AI/monorepo", Number: 57894, State: "MERGED"}}})
	if !strings.Contains(why, "#57894 is merged") || options != nil {
		t.Fatalf("held = %q, %q; want a refusal naming the merged PR", why, options)
	}
}

func TestVoiceSettledReplacesTheStateLineAndDropsTheButtons(t *testing.T) {
	question := voiceQuestionBlocks("*Approve #57894?*", []string{"Approve #57894", "Hold #57894"})
	once := voiceSettled(question, "*Approve #57894?*", "✅ #57894 approved", false)
	twice := voiceSettled(once, "*Approve #57894?*", "✅ #57894 merged 3:18 PM", false)
	if len(twice) != 2 || twice[0].BlockType() != slack.MBTSection || contextText(twice[1]) != "✅ #57894 merged 3:18 PM" {
		t.Fatalf("settled twice = %#v, want the section and one state line, no buttons", twice)
	}
	plain := voiceSettled([]slack.Block{slack.NewRichTextBlock("r")}, "#57894 needs your Approve.", "✅ #57894 merged 3:18 PM", false)
	if len(plain) != 2 || plain[0].BlockType() != slack.MBTSection {
		t.Fatalf("a plain post settled = %#v, want its text as a section and the state line", plain)
	}
}

func TestVoiceRefusesAnAskToApproveWhatHeAlreadyApproved(t *testing.T) {
	asks := stubDecide{answers: map[string]spawnllm.Answer{"approve": spawnllm.BinaryAnswer{PYes: 0.95}}}
	v := &voiceSender{decide: asks.decide}
	approved := []voicePR{{Repo: "Forge-AI/monorepo", Number: 53305, State: "OPEN", Approved: true}, {Repo: "Forge-AI/monorepo", Number: 53810, State: "OPEN", Approved: true}}
	why, options, _, _ := v.held(context.Background(), voiceRequest{Text: "Run page v3 is ready to merge: tap and they go in now.", Options: []string{"Merge v3 now", "Hold for now"}, PRs: approved})
	if !strings.Contains(why, "already approved #53305, #53810") || options != nil {
		t.Fatalf("held = %q, %q; want a refusal saying he already approved them", why, options)
	}
	why, options, _, _ = v.held(context.Background(), voiceRequest{Text: "#57894 needs your Approve.", PRs: []voicePR{{Repo: "Forge-AI/monorepo", Number: 57894, State: "OPEN"}}})
	if why != "" || len(options) != 2 {
		t.Fatalf("held = %q, %q; want a PR with no Approve to post with its buttons", why, options)
	}
}

func TestVoiceLinksAnEarlierTypedAskInTheCardsThreadToTheCard(t *testing.T) {
	logTo := filepath.Join(t.TempDir(), voiceLogName)
	at := time.Now().Add(-time.Hour)
	var lines []byte
	for _, out := range []voiceOutcome{
		{ID: "a", At: at, Lane: "iris", ChannelID: "C1", TS: "1794875438.095139", Permalink: "https://x.slack.com/archives/C1/p1794875438095139", After: "Hotfix the two now, or wait? Say hotfix or wait."},
		{ID: "b", At: at, Lane: "iris", ChannelID: "C1", TS: "1794875500.000100", ThreadTS: "1794875438.095139", Permalink: "https://x.slack.com/archives/C1/p1794875500000100", After: "CI on #57810 is green."},
		{ID: "c", At: at, Lane: "other", ChannelID: "C1", TS: "1794875600.000100", ThreadTS: "1794875438.095139", Permalink: "https://x.slack.com/archives/C1/p1794875600000100", After: "Hotfix or wait?"},
	} {
		line, _ := json.Marshal(out)
		lines = append(append(lines, line...), '\n')
	}
	if err := os.WriteFile(logTo, lines, 0o600); err != nil {
		t.Fatal(err)
	}
	same := stubDecide{answers: map[string]spawnllm.Answer{"p0": spawnllm.BinaryAnswer{PYes: 0.9}, "p1": spawnllm.BinaryAnswer{PYes: 0.1}}}
	v := &voiceSender{decide: same.decide, logTo: logTo}
	v.linkEarlier(context.Background(), voiceJob{req: voiceRequest{Lane: "iris", ChannelID: "C1", ThreadTS: "1794875438.095139"}}, "1794877409.837249", "Hotfix #57810 and #57835 now, or wait?")
	written, _ := os.ReadFile(logTo)
	last := strings.Split(strings.TrimSpace(string(written)), "\n")
	var linked voiceOutcome
	if err := json.Unmarshal([]byte(last[len(last)-1]), &linked); err != nil || !strings.HasSuffix(linked.ID, "-asks") || linked.TS != "1794875438.095139" || linked.About[0]["ts"] != "1794877409.837249" || len(last) != 4 {
		t.Fatalf("log = %s, want one link from the 4:30 post to the card", written)
	}
}

func TestVoiceALongQuestionKeepsItsFirstLineWhateverJevCuts(t *testing.T) {
	units := voiceUnits("*Approve #58012 as built, with the note required and shown at the chat's end only, or change it first?* It changes two builtins. The builder ran out of tokens once.")
	ids, _ := spokenUnits(units)
	kept := keptOpener(units, voiceEdits{cut: ids})
	if got := voiceApplied(units, kept); !strings.HasPrefix(got, "*Approve #58012 as built") {
		t.Fatalf("question = %q, want its first line kept", got)
	}
}

func TestVoiceNoRawAngleBracketReachesACard(t *testing.T) {
	blocks := voiceQuestionBlocks(voiceFormat("Merge <https://github.com/acme/app/pull/7|#7 the fix> now? It was <<https://github.com/acme/app/pull/8|#8>> wrapped twice."), []string{"Merge", "Hold"})
	text := sectionText(blocks[:1])
	if strings.Contains(text, "<<") || strings.Contains(text, ">>") {
		t.Fatalf("card text = %q, want no raw doubled brackets", text)
	}
}

func TestVoiceReplaceInPlaceOnlyRewritesTheSameThreadsLastPost(t *testing.T) {
	v := &voiceSender{last: map[string]voiceLast{}}
	now := time.Now()
	v.posted("C1/1.1", voiceLast{ts: "1.2", text: "#7 is green.", at: now})
	if got := v.lastPost("C1/2.2", now); got.ts != "" {
		t.Fatalf("lastPost in another thread = %+v, want none", got)
	}
	if got := v.lastPost("C1/1.1", now.Add(voiceRepeatFor+time.Minute)); got.ts != "" {
		t.Fatalf("lastPost past %v = %+v, want none", voiceRepeatFor, got)
	}
	if got := v.lastPost("C1/1.1", now); got.ts != "1.2" {
		t.Fatalf("lastPost = %+v, want the thread's own last post", got)
	}
}

func TestVoiceRefusesOnceThenWarns(t *testing.T) {
	v := &voiceSender{warned: map[string]time.Time{}}
	if v.warnedBefore("iris", voiceTypedChoice) {
		t.Fatal("first refusal warned, want it to refuse")
	}
	if !v.warnedBefore("iris", voiceTypedChoice) {
		t.Fatal("second try refused again, want it to post with a warning")
	}
}

func TestVoiceWarnsAPostThatAnnouncesAGapWithNoOwnerButPostsIt(t *testing.T) {
	gap := stubDecide{answers: map[string]spawnllm.Answer{"gap": spawnllm.BinaryAnswer{PYes: 0.9}}}
	v := &voiceSender{decide: gap.decide}
	why, options, warning, _ := v.held(context.Background(), voiceRequest{Text: "Two follow-ups came out of the review. No job carries these yet."})
	if why != "" || options != nil || warning != voiceGap {
		t.Fatalf("held = %q, %q, %q; want no refusal and the gap warning", why, options, warning)
	}
}

func TestVoiceRefusesAButtonSlackWouldCutOrThatSaysNothing(t *testing.T) {
	unclear := stubDecide{answers: map[string]spawnllm.Answer{"buttons": spawnllm.BinaryAnswer{PYes: 0.9}}}
	v := &voiceSender{decide: unclear.decide}
	if why, _, _, _ := v.held(context.Background(), voiceRequest{Text: "Which shimmer should the chat show?", Options: []string{"Shimmer only on a", "Both"}}); why != voiceUnclearButton {
		t.Fatalf("held = %q, want the unclear-button refusal", why)
	}
}

func TestVoiceAttachesButtonsOnlyForPRsHeHasNotApproved(t *testing.T) {
	why, options := voiceApproveAsk(voiceRequest{PRs: []voicePR{{Number: 57891, State: "OPEN"}, {Number: 57630, State: "OPEN", Approved: true}}})
	if why != "" || strings.Join(options, "|") != "Approve #57891|Hold #57891" {
		t.Fatalf("voiceApproveAsk = %q, %q; want buttons for #57891 alone", why, options)
	}
}

func TestVoiceRefusesABareLabelLead(t *testing.T) {
	bare := stubDecide{answers: map[string]spawnllm.Answer{"lead": spawnllm.BinaryAnswer{PYes: 0.9}}}
	v := &voiceSender{decide: bare.decide}
	if why, _, _, _ := v.held(context.Background(), voiceRequest{Text: "Release 3 status.\n• Staging's switch-back test didn't run."}); why != voiceBareLead {
		t.Fatalf("held = %q, want the bare-lead refusal", why)
	}
}

func TestVoiceRefusesACardHisRulingAlreadyAnswers(t *testing.T) {
	ruled := stubDecide{answers: map[string]spawnllm.Answer{"ruling": spawnllm.LabelAnswer{Choice: "r0", Probabilities: map[string]float64{"r0": 0.9}}, "pick": spawnllm.LabelAnswer{Choice: "Rehearsal fleet"}}}
	v := &voiceSender{decide: ruled.decide}
	req := voiceRequest{Text: "Release 3: where should staging test the switch back and forward?", Options: []string{"Rehearsal fleet", "Freeze shared staging"}, Rulings: []voiceRuling{{At: "4:58 PM", Binds: "no staging hold"}}}
	if why, _, _, _ := v.held(context.Background(), req); !strings.Contains(why, "his 4:58 PM ruling already answers") {
		t.Fatalf("held = %q, want the ruling refusal", why)
	}
	if ruling, pick := v.ruledBy(context.Background(), req.Text, req.Options, req.Rulings, time.Second); ruling == nil || pick != "Rehearsal fleet" {
		t.Fatalf("ruledBy = %+v, %q; want the 4:58 PM ruling picking the rehearsal fleet", ruling, pick)
	}
	open := stubDecide{answers: map[string]spawnllm.Answer{"ruling": spawnllm.LabelAnswer{Choice: "none", Probabilities: map[string]float64{"none": 0.9}}}}
	v = &voiceSender{decide: open.decide}
	if ruling, _ := v.ruledBy(context.Background(), req.Text, req.Options, req.Rulings, time.Second); ruling != nil {
		t.Fatalf("ruledBy = %+v, want none for a question still his", ruling)
	}
}

func TestVoiceACardsIdRidesItsButtonsNeverItsText(t *testing.T) {
	blocks := voiceCarded(voiceQuestionBlocks("Approve all 3?", []string{"Approve all 3", "Hold all"}), "75976b")
	if got := voiceCardOf(blocks); got != "75976b" {
		t.Fatalf("voiceCardOf = %q, want 75976b", got)
	}
	if strings.Contains(sectionText(blocks[:1]), "75976b") {
		t.Fatal("the card id shows in the post's text")
	}
}

func TestVoiceRefusesAPostThatOverwhelmsTheScreenByJudgementNotCount(t *testing.T) {
	lines := make([]string, 0, 16)
	for n := range 16 {
		lines = append(lines, fmt.Sprintf("• <https://github.com/o/r/pull/%d|#%d fix the reader>", 57900+n, 57900+n))
	}
	text := "These wait on your Approve:\n" + strings.Join(lines, "\n")
	wall := stubDecide{answers: map[string]spawnllm.Answer{"overwhelming": spawnllm.BinaryAnswer{PYes: 0.9}}}
	if why, _, _, _ := (&voiceSender{decide: wall.decide}).held(context.Background(), voiceRequest{Text: text}); why != voiceOverwhelming {
		t.Fatalf("held = %q, want the overwhelming refusal", why)
	}
	if why, _, _, _ := (&voiceSender{decide: stubDecide{}.decide}).held(context.Background(), voiceRequest{Text: text}); why != "" {
		t.Fatalf("held with Jev reading it fine = %q, want no count cap", why)
	}
}

func TestVoiceRefusesALinkInsideALinksLabel(t *testing.T) {
	v := &voiceSender{decide: stubDecide{}.decide}
	for _, text := range []string{
		"<https://github.com/o/r/pull/57535|#57535 tools/ci mac-box prove <https://github.com/o/r/pull/57535|#57535>> is green.",
		"#56808 [#56570](<https://github.com/o/r/pull/56570|#56570>) drew the page.",
	} {
		if why, _, _, _ := v.held(context.Background(), voiceRequest{Text: text}); why != voiceNestedLink {
			t.Errorf("held(%q) = %q, want the nested-link refusal", text, why)
		}
	}
}

func TestVoiceRefusesALabelBuiltFromABodyFragment(t *testing.T) {
	fragment := stubDecide{answers: map[string]spawnllm.Answer{"labels": spawnllm.BinaryAnswer{PYes: 0.9}}}
	v := &voiceSender{decide: fragment.decide}
	if why, _, _, _ := v.held(context.Background(), voiceRequest{Text: "<https://github.com/o/r/pull/57192|#57192 Old tree only. Your words> is green."}); why != voiceFragmentLabel {
		t.Fatalf("held = %q, want the fragment-label refusal", why)
	}
}

func TestVoiceRefusesASecondCardOnTheSamePRsWithinTheHour(t *testing.T) {
	log := filepath.Join(t.TempDir(), voiceLogName)
	first, _ := json.Marshal(voiceOutcome{ID: "a", At: time.Now().Add(-26 * time.Minute), Lane: "sandsql-engine", ChannelID: "C1", TS: "1794881988.266319", Permalink: "https://x.slack.com/archives/C1/p1794881988266319", Options: []string{"Approve #57944", "Hold #57944"}, PRs: []voicePR{{Number: 57944}}})
	if err := os.WriteFile(log, append(first, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	v := &voiceSender{decide: stubDecide{}.decide, logTo: log}
	req := voiceRequest{Lane: "sandsql-engine", Text: "Approve these?", Options: []string{"Approve all", "Hold"}, PRs: []voicePR{{Number: 57944}, {Number: 57950}}}
	if why, _, _, _ := v.held(context.Background(), req); !strings.Contains(why, "already asks him about #57944") {
		t.Fatalf("held = %q, want the second-card refusal", why)
	}
	req.EditTS = "1794881988.266319"
	if why, _, _, _ := v.held(context.Background(), req); why != "" {
		t.Fatalf("held on an edit of that card = %q, want it through", why)
	}
	req.EditTS, req.Lane = "", "blobstore"
	if why, _, _, _ := v.held(context.Background(), req); why != "" {
		t.Fatalf("held from another lane = %q, want it through", why)
	}
}

func TestVoiceLogListsOnlyRowsThatWereSentUnlessAskedForAll(t *testing.T) {
	log := filepath.Join(t.TempDir(), voiceLogName)
	v := &voiceSender{logTo: log}
	v.record(voiceOutcome{ID: "posted", Lane: "releases", Permalink: "https://x.slack.com/archives/C1/p1"})
	v.record(voiceOutcome{ID: "replayed", Lane: "releases"})
	v.record(voiceOutcome{ID: "dry", Lane: "releases", Permalink: "https://x.slack.com/archives/C1/p2", DryRun: true})
	v.record(voiceOutcome{ID: "failed", Lane: "releases", Error: "channel_not_found"})
	v.record(voiceOutcome{ID: "refused", Lane: "releases", Dropped: "refused: typed"})
	v.record(voiceOutcome{ID: "laneless-edit", Permalink: "https://x.slack.com/archives/C1/p3"})
	sent, err := readVoiceLog(log, 10, false)
	if err != nil || len(sent) != 1 || sent[0].ID != "posted" || !sent[0].Sent {
		t.Fatalf("readVoiceLog = %+v, %v; want the one sent post", sent, err)
	}
	if every, err := readVoiceLog(log, 10, true); err != nil || len(every) != 6 {
		t.Fatalf("readVoiceLog(all) = %d rows, %v; want 6", len(every), err)
	}
}

func TestVoiceRefusesACorrectionPostedAsANewPostAndPlumbingTalk(t *testing.T) {
	for key, want := range map[string]string{"correction": voiceCorrection, "plumbing": voicePlumbing} {
		v := &voiceSender{decide: stubDecide{answers: map[string]spawnllm.Answer{key: spawnllm.BinaryAnswer{PYes: 0.9}}}.decide, logTo: filepath.Join(t.TempDir(), voiceLogName)}
		if why, _, _, _ := v.held(context.Background(), voiceRequest{Lane: "releases", Text: "Correction: four are going in, and a builder is on it."}); why != want {
			t.Errorf("held with %s = %q, want %q", key, why, want)
		}
	}
	v := &voiceSender{decide: stubDecide{answers: map[string]spawnllm.Answer{"correction": spawnllm.BinaryAnswer{PYes: 0.9}}}.decide}
	if why, _, _, _ := v.held(context.Background(), voiceRequest{Text: "Four are going in.", EditTS: "1.1"}); why != "" {
		t.Fatalf("held on an edit = %q, want a correction in place let through", why)
	}
}

func TestVoiceAPostThatNeverReachedSlackIsListedUnsentForItsLane(t *testing.T) {
	for out, want := range map[*voiceOutcome]bool{
		{Lane: "releases", Error: "channel_not_found"}:                      true,
		{Lane: "releases", Dropped: "dead link: {pr:57961}"}:                true,
		{Lane: "releases", Dropped: "refused: typed"}:                       false,
		{Lane: "releases", Permalink: "https://x.slack.com/archives/C1/p1"}: false,
		{Lane: "releases", Error: "timeout", DryRun: true}:                  false,
	} {
		if got := out.unsent(); got != want {
			t.Errorf("unsent(%+v) = %v, want %v", *out, got, want)
		}
	}
}

func TestVoiceScoresAPostByItsWorstCheck(t *testing.T) {
	leaning := stubDecide{answers: map[string]spawnllm.Answer{"lead": spawnllm.BinaryAnswer{PYes: 0.2}, "gap": spawnllm.BinaryAnswer{PYes: 0.4}}}
	why, _, _, score := (&voiceSender{decide: leaning.decide}).held(context.Background(), voiceRequest{Text: "Release 3's staging test passed."})
	if why != "" || score < 0.59 || score > 0.61 {
		t.Fatalf("held = %q, score %v; want no refusal and 0.6", why, score)
	}
}

func TestVoiceLogsTheLanesOwnWordsBesideEachPost(t *testing.T) {
	log := filepath.Join(t.TempDir(), voiceLogName)
	v := &voiceSender{logTo: log}
	v.record(voiceOutcome{ID: "a", Lane: "releases", Words: "{pr:57894} merged", Before: "<https://github.com/o/r/pull/57894|#57894> merged", Permalink: "https://x.slack.com/archives/C1/p1"})
	rows, err := readVoiceLog(log, 1, false)
	if err != nil || len(rows) != 1 || rows[0].Words != "{pr:57894} merged" {
		t.Fatalf("rows = %+v, %v; want the lane's own words logged", rows, err)
	}
}

func TestASecondCorrectionEditsTheLanesLatestPostInItsThread(t *testing.T) {
	log := filepath.Join(t.TempDir(), voiceLogName)
	v := &voiceSender{logTo: log}
	v.record(voiceOutcome{ID: "a", At: time.Now(), Lane: "sandsql-engine", ChannelID: "C1", ThreadTS: "1794817727.413579", TS: "1794817887.000100", Permalink: "https://x.slack.com/archives/C1/p1794817887000100"})
	v.record(voiceOutcome{ID: "b", At: time.Now(), Lane: "sandsql-engine", ChannelID: "C1", ThreadTS: "1.1", TS: "1794817890.000100", Permalink: "https://x.slack.com/archives/C1/p1794817890000100"})
	if ts := v.corrected(voiceRequest{Lane: "sandsql-engine", ChannelID: "C1", ThreadTS: "1794817727.413579"}); ts != "1794817887.000100" {
		t.Fatalf("corrected = %q, want the lane's latest post in the same thread", ts)
	}
	if ts := v.corrected(voiceRequest{Lane: "sandsql-engine", ChannelID: "C1", ThreadTS: "9.9"}); ts != "" {
		t.Fatalf("corrected = %q, want none in a thread the lane never posted in", ts)
	}
}

func contextText(block slack.Block) string {
	context, _ := block.(*slack.ContextBlock)
	text, _ := context.ContextElements.Elements[0].(*slack.TextBlockObject)
	return text.Text
}
