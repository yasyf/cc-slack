package main

import (
	"reflect"
	"regexp"
	"slices"
	"testing"
)

func TestAPRCardDrawsOnlySectionsHisWordsAskFor(t *testing.T) {
	asked := map[string]string{
		cardStaging:    `answer e6706d2: "Tested on staging? I shouldn't have to ask you should be telling me proactively"`,
		cardStoredData: `answer 83ffdff: "How will you migrate the old ones / avoid vrwsking sbyhrigh?"`,
		cardChanged:    `answer 82203c5: "If a new version replaces it the old card should be updated in addition to new card in slack"`,
		cardSummary:    `long-running landing.md: "a summary of wtf the PR is doing, and use exact technical terms rather than being roundabout"`,
		cardImpact:     `monorepo AGENTS.md, Nov 13: "this MUST be usedto analyze impact before doing anything tht coulc imapct FDEs, regardless of what the human says"`,
		cardRefresher:  `answer 7a320c0: "the ELI5 should be come ELI haven't read this code in a while but generally know this codebsase"`,
		cardSurprise:   `answer 7a320c0: "maybe “biggest unexpected change”"`,
		cardHateMost:   `answer 987970a: "one line previews (or two) of for users/plan/risk"; answer 396167a: "replacing risk, adding the eli5"`,
		cardPlan:       `answer 4c141fd: "Things like against the plan are only useful if there is a deviation"`,
		"Show me":      `answer 597f3e1: "you have to have one of the show-me variants for every PR card"`,
	}
	var pr cardPR
	fields := reflect.ValueOf(&pr).Elem()
	for i := range fields.NumField() {
		if f := fields.Field(i); f.Kind() == reflect.String {
			f.SetString("Words.")
		}
	}
	pr.URL, pr.Title = "https://github.com/acme/app/pull/1", ""
	drawn := map[string]bool{}
	for _, label := range regexp.MustCompile(`(?m)^\*([^*\n]+)\*[\n ]`).FindAllStringSubmatch(sectionText(slices.Concat(pr.section(view(), 0, 1, nil), pr.blocks(view(), true))), -1) {
		drawn[label[1]] = true
	}
	for label := range drawn {
		if asked[label] == "" {
			t.Errorf("a PR card draws a %q section, which no words of his ask for", label)
		}
	}
	for label, words := range asked {
		if !drawn[label] {
			t.Errorf("a PR card with every field filled draws no %q section, which he asked for in %s", label, words)
		}
	}
}
