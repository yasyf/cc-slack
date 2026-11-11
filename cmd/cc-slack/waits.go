package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

const (
	mentioned        = "\x00"
	pastedTitleWords = 3
)

const (
	waitsNeedsCard = "a line that says something waits on the reader links the open card it means: link the card's permalink from `cc-slack drive-cards` under words that name it, as <permalink|the lock card>, and Slack draws the card under the line, or draw a PR's tap card with tap_prs; outside your DM with the app, name who decides with a <@U…> mention instead"
	cardLinkNamed  = "link a card under words that name it, as <permalink|the lock card>, and Slack draws the card under the line; a bare permalink, or one labeled with its own URL, shows him a raw URL"
)

var (
	waitsMention  = regexp.MustCompile(`<@[UW][A-Z0-9]+(?:\|[^<>]*)?>`)
	waitsLabel    = regexp.MustCompile(`<[^<>|\s][^<>|]*\|([^<>]*)>`)
	wrappedLink   = regexp.MustCompile(`<(https://[^<>|\s]+)\|([^<>]*)>`)
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
	whatWaits  = wordSet("what", "what's", "whatever")
	everyWaits = wordSet("everything", "anything", "all", "whichever")
	whatFill   = wordSet("still", "else", "is", "are", "now", "that", "which")
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
	return fmt.Sprintf("this post says %q, which waits on him, and links no open card in your DM with the app: link the card's permalink from `cc-slack drive-cards` under words that name it, as <permalink|the lock card>, and Slack draws the card under the line, or pass the PR as tap_prs for its tap card", w.sentence)
}

type rawCardLink struct {
	line int
	link string
}

func (w rawCardLink) String() string {
	return fmt.Sprintf("%d:1 raw-card-link %q: %s", w.line, w.link, cardLinkNamed)
}

func rawCardLinks(text string, card func(permalinkRef) bool) []rawCardLink {
	var found []rawCardLink
	for number, line := range strings.Split(text, "\n") {
		shown := wrappedLink.ReplaceAllStringFunc(line, func(link string) string {
			m := wrappedLink.FindStringSubmatch(link)
			if namesLink(m[1], m[2]) {
				return ""
			}
			return m[1]
		})
		for _, link := range messageLink.FindAllString(shown, -1) {
			if ref, err := parsePermalink(link); err == nil && ref.ts != "" && card(ref) {
				found = append(found, rawCardLink{number + 1, link})
			}
		}
	}
	return found
}

func namesLink(url, label string) bool {
	return !strings.Contains(strings.TrimPrefix(url, "https://"), strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(label), "https://"), "http://"))
}

func (a *app) refuseCardCopies(id identity, channelID string, b opBody) error {
	if raw := rawCardLinks(b.Text, a.storedCardIn(id)); len(raw) > 0 {
		return fmt.Errorf("this post shows the card link %s as a raw URL: %s", raw[0].link, cardLinkNamed)
	}
	if slices.ContainsFunc(b.Blocks, func(raw json.RawMessage) bool { return bytes.Contains(raw, []byte(`"`+cardActionPrefix)) }) {
		return errors.New("these blocks copy a card: link the card in the text under words that name it instead, and Slack draws the card under the line")
	}
	if channelID != id.DMChannelID {
		return nil
	}
	for _, c := range a.cards.askingCards(channelID) {
		if c.ref.thread != b.ThreadTS && pastesTitle(b.Text, c.title) && !linksMessage(b.Text, c.ref.ts) {
			return fmt.Errorf("this post repeats the open card %q without its link: link it as %s, and Slack draws the card under the line", c.title, slackLink(id.permalink(c.ref.channel, c.ref.ts, c.ref.thread), c.title))
		}
	}
	return nil
}

func (a *app) refuseBlanking(id identity, b opBody) error {
	if len(b.Blocks) > 0 || b.ChannelID != id.DMChannelID && b.ChannelID != id.UserID {
		return nil
	}
	card, drawn := a.cards.drawnAt(id.DMChannelID, b.TS)
	if !drawn {
		return nil
	}
	return fmt.Errorf("this message draws the card %s, and an edit with no blocks replaces its layout and buttons with plain text: redraw it with dm-status and its card instead, under the same waits id while it waits on him, or once he answered under a new waits id whose supersedes names this permalink, and every copy of it follows", id.permalink(card.channel, card.ts, card.thread))
}

func (a *app) storedCardIn(id identity) func(permalinkRef) bool {
	return func(ref permalinkRef) bool {
		_, found := a.cards.cardAt(ref.channelID, ref.ts)
		return ref.channelID == id.DMChannelID && found
	}
}

func pastesTitle(text, title string) bool {
	words := strings.Fields(strings.ToLower(title))
	return len(words) >= pastedTitleWords && strings.Contains(strings.Join(strings.Fields(strings.ToLower(text)), " "), strings.Join(words, " "))
}

func linksMessage(text, ts string) bool {
	return slices.ContainsFunc(messageLink.FindAllString(text, -1), func(link string) bool {
		ref, err := parsePermalink(link)
		return err == nil && ref.ts == ts
	})
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
	return (at == 0 || words[at-1] != "if" && !whatWaits[words[at-1]]) &&
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
			return !namesNothing(words, back)
		}
	}
	return false
}

func namesNothing(words []string, verb int) bool {
	for back := verb - 1; back >= 0; back-- {
		if whatWaits[words[back]] || everyWaits[words[back]] {
			return true
		}
		if !whatFill[words[back]] {
			return false
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
