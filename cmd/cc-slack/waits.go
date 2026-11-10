package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

const mentioned = "\x00"

const waitsNeedsCard = "a line that says something waits on the reader carries the open card it means: link the card by its permalink from `cc-slack drive-cards`, and the post copies it with its buttons into the thread, or draw a PR's tap card with tap_prs; outside your DM with the app, name who decides with a <@U…> mention instead"

var (
	waitsMention  = regexp.MustCompile(`<@[UW][A-Z0-9]+(?:\|[^<>]*)?>`)
	waitsLabel    = regexp.MustCompile(`<[^<>|\s][^<>|]*\|([^<>]*)>`)
	waitsUnread   = regexp.MustCompile("`[^`\n]+`|<[^<>]*>|https?://\\S+")
	waitsClause   = regexp.MustCompile(`[.!?;:,()\[\]—–]+(?:\s|$)|\s[-–—]\s|[;:—–]`)
	waitsWord     = regexp.MustCompile(`[a-z]+(?:['’][a-z]+)*(?:-[a-z]+)*`)
	waitsThankYou = regexp.MustCompile(`\bthank(?:s| you)\b`)
	dmCardLink    = regexp.MustCompile(`^D[A-Z0-9]+$`)
)

var (
	dependsOn = wordSet("wait", "waits", "waiting", "await", "awaits", "awaiting", "need", "needs", "needing",
		"blocked", "blocking", "blocks", "pending", "hinges", "depends", "depending", "rests", "requires", "requiring")
	dependsOnTo  = wordSet("up", "over", "down", "left")
	dependsOnFor = wordSet("ready", "held", "holds", "holding")
	asked        = wordSet("needed", "required", "awaited", "pending", "outstanding")
	reader       = wordSet("you", "your", "you'll")
	readerAct    = wordSet("pick", "tap", "taps", "answer", "approval", "approve", "go", "go-ahead", "ok", "okay", "call",
		"decision", "word", "review", "yes", "sign-off", "signoff", "input", "choice", "say-so", "nod", "ruling", "reply",
		"response", "confirmation", "feedback", "eyes", "look", "click", "vote", "move", "turn", "steer", "verdict")
	readerNext = wordSet("to", "now", "here", "first", "again", "in", "on", "for", "at", "alone", "today", "tonight",
		"tomorrow", "please", "still")
	readerMust = wordSet("need", "have", "got")
	clauseJoin = wordSet("and", "but", "or", "so", "then", "while", "because", "once", "after", "before", "until", "when", "if", "since")
	negates    = wordSet("not", "no", "never", "nothing", "none", "nobody", "without", "neither", "nor")
	decides    = wordSet("call", "move", "turn")
	itIs       = wordSet("it's", "its", "is", "that's")
)

func wordSet(words ...string) map[string]bool {
	set := make(map[string]bool, len(words))
	for _, w := range words {
		set[w] = true
	}
	return set
}

type waitsLine struct {
	line     int
	sentence string
}

func (w waitsLine) String() string {
	return fmt.Sprintf("%d:1 waits-without-card %q: %s", w.line, w.sentence, waitsNeedsCard)
}

func waitsWithoutCard(text string, card func(permalinkRef) bool) (waitsLine, bool) {
	said, found := waitsOnReader(text)
	if !found || slices.ContainsFunc(messageLink.FindAllString(text, -1), func(link string) bool {
		ref, err := parsePermalink(link)
		return err == nil && ref.ts != "" && card(ref)
	}) {
		return waitsLine{}, false
	}
	return said, true
}

func dmCard(ref permalinkRef) bool { return dmCardLink.MatchString(ref.channelID) }

func (w waitsLine) refusal() string {
	return fmt.Sprintf("this post says %q, which waits on him, and links no open card in your DM with the app: add the card's permalink from `cc-slack drive-cards`, and the post draws the card with its buttons in its thread, or pass the PR as tap_prs for its tap card", w.sentence)
}

func (a *app) openCardIn(id identity) func(permalinkRef) bool {
	return func(ref permalinkRef) bool {
		return ref.channelID == id.DMChannelID && a.cards.openCard(ref.channelID, ref.ts)
	}
}

func (a *app) refuseWaitsWithoutCard(id identity, channelID string, b opBody) error {
	if channelID != id.DMChannelID || b.Card != nil || b.About != "" || len(b.TapPRs)+len(linkedPRRefs(b.Text)) > 0 || a.cards.holdsOpenCard(channelID, b.ThreadTS) {
		return nil
	}
	if said, found := waitsWithoutCard(b.Text, a.openCardIn(id)); found {
		return errors.New(said.refusal())
	}
	return nil
}

func (a *app) embedCards(ctx context.Context, db *sql.DB, id identity, channelID, thread string, b opBody) {
	if channelID != id.DMChannelID || b.Card != nil {
		return
	}
	for _, link := range messageLink.FindAllString(b.Text, -1) {
		ref, err := parsePermalink(link)
		if err != nil || ref.threadTS == thread || !a.openCardIn(id)(ref) {
			continue
		}
		if err := a.copyCardInto(ctx, db, id, thread, cardRef{ref.channelID, ref.threadTS, ref.ts}); err != nil {
			notePostWarning(ctx, fmt.Sprintf("NO CARD: the post links the open card %s, and drawing it in thread %s failed: %v", link, thread, err))
		}
	}
}

func waitsOnReader(text string) (waitsLine, bool) {
	for number, line := range strings.Split(text, "\n") {
		shown := waitsUnread.ReplaceAllString(waitsLabel.ReplaceAllString(waitsMention.ReplaceAllString(line, mentioned), "$1"), " ")
		for _, sentence := range strings.FieldsFunc(shown, func(r rune) bool { return r == '.' || r == '!' || r == '?' }) {
			if strings.Contains(sentence, mentioned) {
				continue
			}
			for _, clause := range waitsClause.Split(sentence, -1) {
				if waitsOn(waitsWord.FindAllString(strings.ReplaceAll(waitsThankYou.ReplaceAllString(strings.ToLower(clause), "thanks"), "’", "'"), -1)) {
					return waitsLine{number + 1, strings.TrimSpace(sentence)}, true
				}
			}
		}
	}
	return waitsLine{}, false
}

func waitsOn(words []string) bool {
	for i, w := range words {
		if reader[w] && waitedOnFrom(words, i) && !negated(words, i) {
			return true
		}
	}
	return false
}

func waitedOnFrom(words []string, at int) bool {
	return asksFor(words, at) || objectOf(words, at) && dependsBefore(words, at)
}

func asksFor(words []string, at int) bool {
	next, ahead := wordAt(words, at+1), words[at+1:min(len(words), at+4)]
	if words[at] == "your" {
		return decides[next] && (at == 0 || itIs[words[at-1]]) ||
			readerAct[next] && slices.ContainsFunc(words[min(len(words), at+2):min(len(words), at+5)], func(w string) bool { return asked[w] })
	}
	return (at == 0 || words[at-1] != "if") &&
		(slices.Contains(ahead, "must") || slices.ContainsFunc(ahead, func(w string) bool { return readerMust[w] }) && slices.Contains(ahead, "to"))
}

func objectOf(words []string, at int) bool {
	next := wordAt(words, at+1)
	switch words[at] {
	case "your":
		return readerAct[next]
	case "you":
		return next == "" || readerNext[next]
	}
	return false
}

func dependsBefore(words []string, at int) bool {
	for back := at - 1; back >= max(0, at-5); back-- {
		w := words[back]
		if clauseJoin[w] {
			return false
		}
		if dependsOn[w] || dependsOnTo[w] && words[back+1] == "to" || dependsOnFor[w] && words[back+1] == "for" {
			return true
		}
	}
	return false
}

func wordAt(words []string, at int) string {
	if at < len(words) {
		return words[at]
	}
	return ""
}

func negated(words []string, at int) bool {
	return slices.ContainsFunc(words[max(0, at-6):min(len(words), at+3)], func(w string) bool {
		return negates[w] || strings.HasSuffix(w, "n't")
	})
}
