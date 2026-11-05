package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/slack-go/slack"
	"github.com/yasyf/cc-interact/agent"
)

func endWatches(t *testing.T, l *liveDaemon, thread string) {
	t.Helper()
	if _, err := l.srv.DB().ExecContext(l.ctx, `UPDATE watches SET status = 'stopped' WHERE thread_ts = ?`, thread); err != nil {
		t.Fatal(err)
	}
}

func mergeTap(c postedCard, n int) cardActionPush {
	return cardActionPush{User: testUser, Channel: c.id.DMChannelID, MessageTS: c.ts, ThreadTS: c.thread,
		ActionID: cardWaitAction + "0:step-3:merge", Value: string(mustJSON(askTap{ID: "step-3:merge", Label: "Merge", Question: "Merge #41 now?", Options: []string{"Merge", "Hold"}})),
		ActionTS: "1800000100.000000", Seq: fmt.Sprintf("001800000100000-abcd%04d", n)}
}

func TestATapNoSessionHoldsShowsItsPickOnTheCopyAndTheCard(t *testing.T) {
	l, id, card, thread := copyDaemon(t, cardBody("Send the report?", "send it"))
	l.do(opCardCopy, map[string]any{"url": id.permalink(id.DMChannelID, card, ""), "thread_ts": thread})
	post := lastPost(l)
	buttons := copiedButtonsOf(t, post)
	at := slices.IndexFunc(buttons, func(b *slack.ButtonBlockElement) bool { return strings.HasPrefix(b.ActionID, cardCopyAction) })
	if at < 0 {
		t.Fatalf("buttons = %+v, want the card's answer", buttons)
	}
	endWatches(t, l, card)
	sent := len(l.slack.sent())

	push := cardActionPush{User: testUser, Channel: id.DMChannelID, MessageTS: post.ts, ThreadTS: thread,
		ActionID: buttons[at].ActionID, Value: buttons[at].Value, ActionTS: "1800000100.000000", Seq: "001800000100000-abcd0123"}
	l.tap(id, push)

	for _, ts := range []string{card, post.ts} {
		edits := editsOf(l.slack.edited(), ts)
		if len(edits) == 0 || !strings.Contains(edits[len(edits)-1], "*send it* reached no one") {
			t.Fatalf("message %s edits = %v, want the pick and that it reached no one", ts, edits)
		}
	}
	if got := l.slack.sent()[sent:]; len(got) != 0 {
		t.Fatalf("posted %+v, want the note on the messages and no new post", got)
	}
	if l.app.replay.latest() != push.Seq {
		t.Fatalf("replay cursor = %q, want it past the tap", l.app.replay.latest())
	}
}

func TestAWaitingTapNoSessionHoldsKeepsTheCardOpenAndSaysSo(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	c := postWaiting(t, l, mergeCard())
	endWatches(t, l, c.thread)

	l.tap(c.id, mergeTap(c, 1))

	edits := editsOf(l.slack.edited(), c.ts)
	if len(edits) == 0 {
		t.Fatal("the card kept showing what it showed before the tap")
	}
	last := edits[len(edits)-1]
	if !strings.Contains(last, "*Merge* reached no one") || !strings.Contains(last, cardWaitAction) || strings.Contains(last, cardWaitHint) {
		t.Fatalf("card = %s, want the note beside its buttons and no line saying the lane waits", last)
	}
	if _, done := l.app.cards.answer(c.id.DMChannelID, c.thread, "step-3:merge"); done {
		t.Fatal("a tap that reached no one reads as answered")
	}
}

func TestAnAckNoSessionHoldsSaysItReachedNoOne(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	l.do(opDMStatus, cardBody("Rolling out"))
	ts := l.slack.sent()[0].ts
	id, err := loadIdentity(l.ctx, l.srv.DB())
	if err != nil {
		t.Fatal(err)
	}
	endWatches(t, l, ts)

	l.tap(id, cardActionPush{User: testUser, Channel: id.DMChannelID, MessageTS: ts, ThreadTS: ts,
		ActionID: cardAckAction, Value: cardAckLabel, ActionTS: "1800000100.000000"})

	edits := editsOf(l.slack.edited(), ts)
	if len(edits) == 0 || !strings.Contains(edits[0], "*"+cardAckLabel+"*") || !strings.Contains(edits[len(edits)-1], "*"+cardAckLabel+"* reached no one") {
		t.Fatalf("edits = %v, want the Ack and that it reached no one", edits)
	}
}

func TestAButtonNoRouteKnowsSaysSoOnTheMessage(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	c := postWaiting(t, l, mergeCard())

	l.tap(c.id, cardActionPush{User: testUser, Channel: c.id.DMChannelID, MessageTS: c.ts, ThreadTS: c.thread,
		ActionID: cardActionPrefix + ":retired", ActionTS: "1800000100.000000"})

	edits := editsOf(l.slack.edited(), c.ts)
	if len(edits) != 1 || !strings.Contains(edits[0], "That button no longer does anything") {
		t.Fatalf("edits = %v, want the message to say the button is dead", edits)
	}
}

func TestATapWhoseEditFailsSaysSoInTheThread(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	c := postWaiting(t, l, mergeCard())
	l.slack.fail("chat.update", errors.New("invalid_blocks"))
	l.slack.fail("chat.update", errors.New("invalid_blocks"))
	sent := len(l.slack.sent())

	l.tap(c.id, mergeTap(c, 1))

	got := l.slack.sent()[sent:]
	if len(got) != 1 || got[0].threadTS != c.thread || !strings.Contains(got[0].text, "*Merge* did not change this message") ||
		!strings.Contains(got[0].text, "the answer on record is *Merge*") {
		t.Fatalf("posted %+v, want one warning in the card's thread naming the answer on record", got)
	}
}

func TestARowShowsItsPickBeforeTheBoardAnswers(t *testing.T) {
	failingPresent(t, "the board is slow and then says no")
	l := startDaemon(t)
	loggedIn(t, l)
	row := prRow(41)
	c := postWaiting(t, l, groupCard(row))

	l.tap(c.id, rowTap(c, row, 0, 520))

	edits := editsOf(l.slack.edited(), c.ts)
	if len(edits) < 2 || !strings.Contains(edits[0], "✓ *"+row.Buttons[0]+"*") || !strings.Contains(edits[len(edits)-1], "did not reach the board") {
		t.Fatalf("edits = %v, want the pick drawn first and the refusal drawn over it", edits)
	}
}

func TestTapShowsReadsTheMessageSlackHolds(t *testing.T) {
	const action = "cc-slack-card:wait:0:ask"
	tap := time.Unix(1800000100, 0)
	button := slack.NewButtonBlockElement(action, "v", slack.NewTextBlockObject(slack.PlainTextType, "Send it", false, false))
	inRow := slack.NewSectionBlock(slack.NewTextBlockObject(slack.MarkdownType, "*1. Send it*", false, false), nil, slack.NewAccessory(button))
	inActions := slack.NewActionBlock("card-actions", button)
	other := slack.NewActionBlock("card-actions", slack.NewButtonBlockElement("cc-slack-card:ack", "v", slack.NewTextBlockObject(slack.PlainTextType, "Ack", false, false)))
	for _, tc := range []struct {
		name   string
		blocks []slack.Block
		edited string
		want   bool
	}{
		{"its row still offers the button and nothing edited it", []slack.Block{inRow}, "", false},
		{"its actions still offer the button and nothing edited it", []slack.Block{inActions}, "", false},
		{"an edit before the tap does not count", []slack.Block{inRow}, "1800000099.000000", false},
		{"an edit in the tap's second counts", []slack.Block{inRow}, "1800000100.000000", true},
		{"the button is gone", []slack.Block{other}, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var post slack.Message
			post.Blocks.BlockSet = tc.blocks
			if tc.edited != "" {
				post.Edited = &slack.Edited{Timestamp: tc.edited}
			}
			if got := tapShows(post, action, tap); got != tc.want {
				t.Fatalf("tapShows = %v, want %v", got, tc.want)
			}
		})
	}
}

func cardOwnerCommand(t *testing.T, answer string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	asked := filepath.Join(dir, "asked")
	command := filepath.Join(dir, "lr")
	script := "#!/bin/sh\ncat >> " + asked + "\n" + answer + "\n"
	if err := os.WriteFile(command, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return command, asked
}

func TestAnAnswerNoSessionHoldsGoesToTheCardsOwnerAndClosesTheCardAndItsCopy(t *testing.T) {
	command, asked := cardOwnerCommand(t, `echo '{"heard":true}'`)
	l, _ := prLinksDaemon(t, map[string]any{"card_command": []string{command}})
	c := postWaiting(t, l, mergeCard())
	var copied postResult
	if err := json.Unmarshal(l.do(opCardCopy, map[string]any{"url": c.url(), "thread_ts": c.thread}).Body, &copied); err != nil {
		t.Fatal(err)
	}
	post := lastPost(l)
	buttons := copiedButtonsOf(t, post)
	at := slices.IndexFunc(buttons, func(b *slack.ButtonBlockElement) bool {
		return strings.HasPrefix(b.ActionID, cardCopyAction) && strings.Contains(b.Text.Text, "Merge")
	})
	if at < 0 {
		t.Fatalf("buttons = %+v, want the card's answer", buttons)
	}
	endWatches(t, l, c.thread)

	l.tap(c.id, cardActionPush{User: testUser, Channel: c.id.DMChannelID, MessageTS: post.ts, ThreadTS: c.thread,
		ActionID: buttons[at].ActionID, Value: buttons[at].Value, ActionTS: "1800000100.000000", Seq: "001800000100000-abcd0124"})

	raw, err := os.ReadFile(asked)
	if err != nil {
		t.Fatal(err)
	}
	var told struct {
		Unheard unheardAnswer `json:"unheard"`
	}
	for asks := json.NewDecoder(bytes.NewReader(raw)); told.Unheard.Card == ""; {
		if err := asks.Decode(&told); err != nil {
			t.Fatalf("asked %s: %v", raw, err)
		}
	}
	if told.Unheard.Card != "step-3:merge" || told.Unheard.Label != "Merge" || told.Unheard.Permalink != c.url() || told.Unheard.Key == "" {
		t.Fatalf("told %+v, want the card, his pick, its link and a key", told.Unheard)
	}
	for _, ts := range []string{c.ts, post.ts} {
		edits := editsOf(l.slack.edited(), ts)
		if len(edits) == 0 || !strings.Contains(edits[0], "*Merge*") || strings.Contains(edits[len(edits)-1], cardWaitAction) {
			t.Fatalf("message %s edits = %v, want the pick at once and then no button left", ts, edits)
		}
	}
	if ans, done := l.app.cards.answer(c.id.DMChannelID, c.thread, "step-3:merge"); !done || ans.Label != "Merge" {
		t.Fatalf("answer = %+v, %v, want his pick on record", ans, done)
	}
}

func TestAnAnswerItsOwnerDidNotHearStaysOpenAndSaysItReachedNoOne(t *testing.T) {
	for _, tc := range []struct{ name, answer string }{
		{"the command fails", "exit 1"},
		{"the command predates the answer", `echo '[]'`},
		{"the command does not say it was heard", `echo '{}'`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			command, _ := cardOwnerCommand(t, tc.answer)
			l, _ := prLinksDaemon(t, map[string]any{"card_command": []string{command}})
			c := postWaiting(t, l, mergeCard())
			endWatches(t, l, c.thread)

			l.tap(c.id, mergeTap(c, 2))

			edits := editsOf(l.slack.edited(), c.ts)
			if len(edits) == 0 || !strings.Contains(edits[len(edits)-1], "*Merge* reached no one") || !strings.Contains(edits[len(edits)-1], cardWaitAction) {
				t.Fatalf("edits = %v, want the buttons kept and that it reached no one", edits)
			}
			if _, done := l.app.cards.answer(c.id.DMChannelID, c.thread, "step-3:merge"); done {
				t.Fatal("an answer no one heard reads as answered")
			}
		})
	}
}

func TestAnAnswerNoSessionHoldsGoesToTheSessionHoldingTheDMAsAnOwedReply(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	c := postWaiting(t, l, mergeCard())
	endWatches(t, l, c.thread)
	if _, _, err := insertWatch(l.ctx, l.srv.DB(), l.subID, agent.TopLevel, kindChannel, c.id.DMChannelID, "", "1700000000.000000", time.Now()); err != nil {
		t.Fatal(err)
	}

	l.tap(c.id, mergeTap(c, 2))

	if got := l.inject(agent.TopLevel); !strings.Contains(got, `with "Merge"`) || !strings.Contains(got, "the session holding the DM") {
		t.Fatalf("holder heard %q, want his pick and that it comes to the DM's holder", got)
	}
	owed, err := l.app.owed.list(l.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(owed) != 1 || owed[0].ThreadTS != c.thread || owed[0].Permalink != c.url() {
		t.Fatalf("owed %+v, want one reply owed on the card's thread", owed)
	}
	edits := editsOf(l.slack.edited(), c.ts)
	if len(edits) == 0 || !strings.Contains(edits[len(edits)-1], "sent to the session holding this DM") || strings.Contains(edits[len(edits)-1], cardWaitAction) {
		t.Fatalf("edits = %v, want the card closed on his pick and naming who heard it", edits)
	}
}

func TestATapReplayedAfterItsAnswerWasRecordedButNotDeliveredIsDeliveredThen(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	c := postWaiting(t, l, mergeCard())
	tap := mergeTap(c, 2)
	l.app.cards.claim(c.id.DMChannelID, c.thread, "step-3:merge", askAnswer{At: slackTime(tap.ActionTS), Label: "Merge", Undelivered: tapKey(tap)})

	l.tap(c.id, tap)

	if got := l.inject(agent.TopLevel); !strings.Contains(got, `answered the waiting ask "step-3:merge" with "Merge"`) {
		t.Fatalf("session heard %q, want the answer the killed daemon never delivered", got)
	}
	if ans, done := l.app.cards.answer(c.id.DMChannelID, c.thread, "step-3:merge"); !done || ans.Undelivered != "" {
		t.Fatalf("answer = %+v, %v, want it on record and delivered", ans, done)
	}
}
