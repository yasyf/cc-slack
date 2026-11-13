package main

import (
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
	if _, err := l.srv.DB().ExecContext(l.ctx, `UPDATE watches SET status = 'stopped' WHERE thread_ts = ? OR kind = 'dm'`, thread); err != nil {
		t.Fatal(err)
	}
}

func mergeTap(c postedCard, n int) cardActionPush {
	return cardActionPush{User: testUser, Channel: c.id.DMChannelID, MessageTS: c.ts, ThreadTS: c.thread,
		ActionID: cardWaitAction + "0:step-3:merge", Value: string(mustJSON(askTap{ID: "step-3:merge", Label: "Merge", Question: "Merge #41 now?", Options: []string{"Merge", "Hold"}})),
		ActionTS: "1800000100.000000", Seq: fmt.Sprintf("001800000100000-abcd%04d", n)}
}

func owedTaps(t *testing.T, l *liveDaemon) []string {
	t.Helper()
	var owed dmOwedResult
	if err := json.Unmarshal(l.do(opDMOwed, nil).Body, &owed); err != nil {
		t.Fatal(err)
	}
	taps := make([]string, 0, len(owed.Replies))
	for _, r := range owed.Replies {
		taps = append(taps, r.Text)
	}
	return taps
}

func forgetCard(l *liveDaemon, c postedCard) {
	l.app.cards.mu.Lock()
	delete(l.app.cards.threads, threadKey(c.id.DMChannelID, c.thread))
	l.app.cards.mu.Unlock()
}

const keptTap = "kept for the next session to hold this DM"

func TestATapNoSessionHoldsIsKeptForTheNextHolderOnTheCopyAndTheCard(t *testing.T) {
	l, id, card, thread := copyDaemon(t, cardBody("Send the report?", "send it"))
	legacyCopy(t, l, id.permalink(id.DMChannelID, card, ""), thread)
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
		if len(edits) == 0 || !strings.Contains(edits[len(edits)-1], "*send it* · answered") || !strings.Contains(edits[len(edits)-1], keptTap) {
			t.Fatalf("message %s edits = %v, want the pick kept for the next session", ts, edits)
		}
	}
	if got := l.slack.sent()[sent:]; len(got) != 0 {
		t.Fatalf("posted %+v, want the pick on the messages and no new post", got)
	}
	if owed := owedTaps(t, l); len(owed) != 1 || !strings.Contains(owed[0], "Tapped send it") {
		t.Fatalf("owed = %q, want his tap kept for the next session to hold the DM", owed)
	}
	if l.app.replay.latest() != push.Seq {
		t.Fatalf("replay cursor = %q, want it past the tap", l.app.replay.latest())
	}
}

func TestAWaitingTapNoSessionOrDriveTakesIsKeptForTheNextHolder(t *testing.T) {
	for name, daemon := range map[string]func(*testing.T) *liveDaemon{
		"no card command": func(t *testing.T) *liveDaemon {
			l := startDaemon(t)
			loggedIn(t, l)
			return l
		},
		"the card is no drive's": func(t *testing.T) *liveDaemon {
			command := cardOwnerCommand(t, `echo "{\"ok\":false,\"why\":\"step-3:merge is no drive's question card\"}"`)
			l, _ := prLinksDaemon(t, map[string]any{"card_command": []string{command}})
			return l
		},
	} {
		t.Run(name, func(t *testing.T) {
			l := daemon(t)
			c := postWaiting(t, l, mergeCard())
			endWatches(t, l, c.thread)
			sent := len(l.slack.sent())

			l.tap(c.id, mergeTap(c, 1))

			edits := editsOf(l.slack.edited(), c.ts)
			if len(edits) == 0 || !strings.Contains(edits[len(edits)-1], "✓ *Merge* · answered") || !strings.Contains(edits[len(edits)-1], keptTap) || strings.Contains(edits[len(edits)-1], cardWaitAction) {
				t.Fatalf("edits = %v, want his pick in place of the buttons, kept for the next session", edits)
			}
			if owed := owedTaps(t, l); len(owed) != 1 || owed[0] != "Tapped Merge on Merge #41 now?" {
				t.Fatalf("owed = %q, want his tap kept for the next session to hold the DM", owed)
			}
			if got := l.slack.sent()[sent:]; len(got) != 0 {
				t.Fatalf("posted %+v, want nothing asking him to say it again", got)
			}
		})
	}
}

func TestAnAckNoSessionHoldsIsKeptForTheNextHolder(t *testing.T) {
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

	if edits := editsOf(l.slack.edited(), ts); len(edits) == 0 || !strings.Contains(edits[len(edits)-1], "*"+cardAckLabel+"*") || !strings.Contains(edits[len(edits)-1], keptTap) {
		t.Fatalf("edits = %v, want the Ack kept for the next session", edits)
	}
	if owed := owedTaps(t, l); len(owed) != 1 || !strings.Contains(owed[0], "Tapped "+cardAckLabel) {
		t.Fatalf("owed = %q, want the Ack kept for the next session to hold the DM", owed)
	}
}

func TestATapOnACardCCSlackForgotReachesItsDriveAndClosesTheCard(t *testing.T) {
	cmd := fakeCardCommand(t, nil)
	l, _ := prLinksDaemon(t, map[string]any{"card_command": cmd.argv})
	c := postWaiting(t, l, mergeCard())
	forgetCard(l, c)
	sent := len(l.slack.sent())

	l.tap(c.id, mergeTap(c, 1))

	asked := answersOf(cmd.requests(t))
	var got struct {
		Answer questionAnswer `json:"answer"`
	}
	if len(asked) != 1 || json.Unmarshal([]byte(asked[0]), &got) != nil || got.Answer.Waits != "step-3:merge" || got.Answer.Label != "Merge" || got.Answer.URL != c.url() {
		t.Fatalf("card command asked %v, want his Merge under the button's own waits id and the card's link", asked)
	}
	edits := editsOf(l.slack.edited(), c.ts)
	if len(edits) == 0 || !strings.Contains(edits[len(edits)-1], "✓ *Merge* · answered") || !strings.Contains(edits[len(edits)-1], "Merge #41 now?") || strings.Contains(edits[len(edits)-1], cardWaitAction) {
		t.Fatalf("edits = %v, want the card closed on his answer, drawn from the message Slack holds", edits)
	}
	if got := l.slack.sent()[sent:]; len(got) != 0 {
		t.Fatalf("posted %+v, want nothing asking him to tap again", got)
	}
}

func TestACloseOnACardCCSlackForgotSettlesItFromTheMessageSlackHolds(t *testing.T) {
	const board = "http://127.0.0.1:7000/p/asks--ab12"
	for name, tc := range map[string]struct {
		fields map[string]any
		want   string
	}{
		"settled":   {map[string]any{"label": "Landed as 5480445", "other": true, "settled": true, "via": "https://github.com/acme/app/pull/41"}, "https://github.com/acme/app/pull/41|Landed as 5480445"},
		"withdrawn": {map[string]any{"label": "superseded by step 4", "withdrawn": true, "via": board}, "*Withdrawn:* superseded by step 4"},
	} {
		t.Run(name, func(t *testing.T) {
			l := startDaemon(t)
			loggedIn(t, l)
			c := postWaiting(t, l, mergeCard())
			forgetCard(l, c)

			tc.fields["url"] = c.url()
			var res cardAnswerResult
			if err := json.Unmarshal(l.do(opCardAnswer, tc.fields).Body, &res); err != nil || !res.Answered {
				t.Fatalf("close = %+v, %v; want the forgotten card closed", res, err)
			}
			edits := editsOf(l.slack.edited(), c.ts)
			if len(edits) == 0 || !strings.Contains(edits[len(edits)-1], tc.want) || strings.Contains(edits[len(edits)-1], cardWaitAction) {
				t.Fatalf("edits = %v, want %q in place of the buttons", edits, tc.want)
			}
		})
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

func TestATapOnACopyThisDaemonDidNotPostRedrawsThatCopyAnswered(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	c := postWaiting(t, l, mergeCard())
	legacyCopy(t, l, c.url(), c.thread)
	post := lastPost(l)
	buttons := copiedButtonsOf(t, post)
	at := slices.IndexFunc(buttons, func(b *slack.ButtonBlockElement) bool {
		return strings.HasPrefix(b.ActionID, cardCopyAction) && b.Text.Text == "Merge"
	})
	if at < 0 {
		t.Fatalf("buttons = %+v, want the copy's Merge", buttons)
	}
	l.app.cards.mu.Lock()
	for _, thread := range l.app.cards.threads {
		thread.Copies = nil
	}
	l.app.cards.mu.Unlock()
	sent := len(l.slack.sent())

	l.tap(c.id, cardActionPush{User: testUser, Channel: c.id.DMChannelID, MessageTS: post.ts, ThreadTS: c.thread,
		ActionID: buttons[at].ActionID, Value: buttons[at].Value, ActionTS: "1800000100.000000", Seq: "001800000100000-abcd0124"})

	edits := editsOf(l.slack.edited(), post.ts)
	if len(edits) == 0 || !strings.Contains(edits[len(edits)-1], "✓ *Merge*") || strings.Contains(edits[len(edits)-1], cardWaitAction) ||
		!strings.Contains(edits[len(edits)-1], cardUndoAction) {
		t.Fatalf("copy edits = %v, want it answered with its Pick buttons gone and its Undo shown", edits)
	}
	if got := l.slack.sent()[sent:]; slices.ContainsFunc(got, func(p fakePost) bool { return strings.Contains(p.text, "did not change this message") }) {
		t.Fatalf("posted %+v, want no warning under a copy that now shows the answer", got)
	}
	if l.app.cards.copyIn(c.id.DMChannelID, c.thread, c.ts) != post.ts {
		t.Fatal("the copy is not remembered, so a later redraw of the card misses it")
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

func cardOwnerCommand(t *testing.T, answer string) string {
	t.Helper()
	command := filepath.Join(t.TempDir(), "lr")
	script := "#!/bin/sh\ncat >/dev/null\n" + answer + "\n"
	if err := os.WriteFile(command, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return command
}

func TestAnAnswerNoSessionHoldsGoesToTheCardsOwnerAsHisAnswerAndClosesTheCardAndItsCopyWithAnUndo(t *testing.T) {
	cmd := fakeCardCommand(t, nil)
	l, _ := prLinksDaemon(t, map[string]any{"card_command": cmd.argv})
	c := postWaiting(t, l, mergeCard())
	legacyCopy(t, l, c.url(), c.thread)
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

	want := []string{string(mustJSON(map[string]any{"answer": map[string]any{
		"waits": "step-3:merge", "label": "Merge", "other": false, "undone": false, "url": c.url(), "action_ts": "1800000100.000000", "source": "pressed the button",
	}}))}
	if got := answersOf(cmd.requests(t)); !slices.Equal(got, want) {
		t.Fatalf("card command asked %v, want his pick as the card's answer, as a tap on a card whose session is live sends it", got)
	}
	for _, ts := range []string{c.ts, post.ts} {
		edits := editsOf(l.slack.edited(), ts)
		if len(edits) == 0 || !strings.Contains(edits[0], "*Merge*") || strings.Contains(edits[len(edits)-1], cardWaitAction) || !strings.Contains(edits[len(edits)-1], cardUndoAction) {
			t.Fatalf("message %s edits = %v, want the pick at once, then no button left but Undo", ts, edits)
		}
	}
	ans, done := l.app.cards.answer(c.id.DMChannelID, c.thread, "step-3:merge")
	if !done || ans.Label != "Merge" || ans.Tap == "" || ans.UndoUntil.Sub(ans.At) != cardUndoWindow {
		t.Fatalf("answer = %+v, %v, want his pick on record, undoable for %s", ans, done, cardUndoWindow)
	}
}

func TestAnUndoOnACardWhoseSessionEndedReachesWhoHeardItsAnswerForTenMinutes(t *testing.T) {
	card := statusCard{Status: "needs_you", Title: "Which store keeps the tally?", Waits: "demo:j3-661440", Options: []string{"Ring", "Table"}, Recommended: "Ring"}
	picked := time.Now()
	pick := func(l *liveDaemon, c postedCard, n int, at time.Time, seq string) {
		l.tap(c.id, cardActionPush{User: testUser, Channel: c.id.DMChannelID, MessageTS: c.ts, ThreadTS: c.thread,
			ActionID: fmt.Sprintf("%s%d:%s", cardWaitAction, n, card.Waits), Value: string(mustJSON(askTap{ID: card.Waits, Label: card.Options[n], Question: card.Title, Options: card.Options})),
			ActionTS: fmt.Sprintf("%d.%06d", at.Unix(), n), Seq: seq})
	}
	undo := func(l *liveDaemon, c postedCard, at time.Time, seq string) {
		l.tap(c.id, cardActionPush{User: testUser, Channel: c.id.DMChannelID, MessageTS: c.ts, ThreadTS: c.thread,
			ActionID: cardUndoAction + card.Waits, ActionTS: fmt.Sprintf("%d.000900", at.Unix()), Seq: seq})
	}
	lastEdit := func(l *liveDaemon, c postedCard) string {
		edits := editsOf(l.slack.edited(), c.ts)
		if len(edits) == 0 {
			t.Fatal("the card was never redrawn")
		}
		return edits[len(edits)-1]
	}
	undoneThenLate := func(t *testing.T, l *liveDaemon, c postedCard) {
		t.Helper()
		if drawn := lastEdit(l, c); !strings.Contains(drawn, "✓ *Ring* · answered") || !strings.Contains(drawn, cardUndoAction+card.Waits) {
			t.Fatalf("card %s, want the pick marked with an Undo button beside it", drawn)
		}
		undo(l, c, picked.Add(time.Minute), "001800000100000-abcd1611")
		if _, done := l.app.cards.answer(c.id.DMChannelID, c.thread, card.Waits); done {
			t.Fatal("the undone answer is still on record")
		}
		if drawn := lastEdit(l, c); !strings.Contains(drawn, cardWaitAction+"0:"+card.Waits) || strings.Contains(drawn, "✓ *") || strings.Contains(drawn, cardUndoAction) {
			t.Fatalf("card %s, want its buttons back with no answer and no Undo", drawn)
		}
		repicked := picked.Add(2 * time.Minute)
		pick(l, c, 1, repicked, "001800000100000-abcd1612")
		undo(l, c, repicked.Add(11*time.Minute), "001800000100000-abcd1613")
		if ans, _ := l.app.cards.answer(c.id.DMChannelID, c.thread, card.Waits); ans.Label != "Table" {
			t.Fatalf("answer = %+v, want the new pick to stand", ans)
		}
		if drawn := lastEdit(l, c); !strings.Contains(drawn, "had passed") || strings.Contains(drawn, cardUndoAction) {
			t.Fatalf("card %s, want the late Undo refused on the card and its button gone", drawn)
		}
	}

	t.Run("the card's owner", func(t *testing.T) {
		cmd := fakeCardCommand(t, nil)
		l, _ := prLinksDaemon(t, map[string]any{"card_command": cmd.argv})
		c := postWaiting(t, l, card)
		endWatches(t, l, c.thread)

		pick(l, c, 0, picked, "001800000100000-abcd1610")
		undoneThenLate(t, l, c)

		answer := func(label, at string, undone bool) string {
			return string(mustJSON(map[string]any{"answer": map[string]any{
				"waits": card.Waits, "label": label, "other": false, "undone": undone, "url": c.url(), "action_ts": at, "source": "pressed the button",
			}}))
		}
		want := []string{
			answer("Ring", fmt.Sprintf("%d.000000", picked.Unix()), false),
			answer("Ring", fmt.Sprintf("%d.000900", picked.Add(time.Minute).Unix()), true),
			answer("Table", fmt.Sprintf("%d.000001", picked.Add(2*time.Minute).Unix()), false),
		}
		if got := answersOf(cmd.requests(t)); !slices.Equal(got, want) {
			t.Fatalf("card command asked %v, want his pick, its undo and his new pick, and nothing for the late Undo", got)
		}
	})

	t.Run("the session holding the DM", func(t *testing.T) {
		l := startDaemon(t)
		loggedIn(t, l)
		c := postWaiting(t, l, card)
		endWatches(t, l, c.thread)
		if _, _, err := insertWatch(l.ctx, l.srv.DB(), l.subID, agent.TopLevel, kindChannel, c.id.DMChannelID, "", "1700000000.000000", time.Now()); err != nil {
			t.Fatal(err)
		}

		pick(l, c, 0, picked, "001800000100000-abcd1610")
		if got := l.inject(agent.TopLevel); !strings.Contains(got, `with "Ring"`) {
			t.Fatalf("holder heard %q, want his pick", got)
		}
		undoneThenLate(t, l, c)
		if got := l.inject(agent.TopLevel); !strings.Contains(got, `undid their answer "Ring"`) || !strings.Contains(got, "treat it as never given") || strings.Contains(got, `undid their answer "Table"`) {
			t.Fatalf("holder heard %q, want the undo of his first pick as an answer never given, and nothing for the late Undo", got)
		}
	})
}

func TestAQuestionTapTheCardCommandDidNotFinishShowsRecordingUntilItsRetryRecordsIt(t *testing.T) {
	prev := prCardPatience
	prCardPatience = 500 * time.Millisecond
	t.Cleanup(func() { prCardPatience = prev })
	cmd := stallingCardCommand(t)
	l, _ := prLinksDaemon(t, map[string]any{"card_command": cmd.argv})
	c := postWaiting(t, l, mergeCard())
	endWatches(t, l, c.thread)

	l.tap(c.id, mergeTap(c, 2))

	if edits := editsOf(l.slack.edited(), c.ts); len(edits) == 0 || !strings.Contains(edits[len(edits)-1], "⏳ *Merge* · recording since") || strings.Contains(edits[len(edits)-1], cardWaitAction) {
		t.Fatalf("edits = %v, want his tap shown as recording, with no buttons, while the card command had not finished", edits)
	}
	if ans, done := l.app.cards.answer(c.id.DMChannelID, c.thread, "step-3:merge"); !done || ans.Label != "Merge" || ans.Owed == nil {
		t.Fatalf("answer = %+v, %v, want Merge kept and owed to the card command", ans, done)
	}
	within(t, 10*time.Second, "the owed tap recorded", func() bool {
		return len(answersOf(cmd.requests(t))) == 1
	})
	within(t, 5*time.Second, "the card showing his recorded answer", func() bool {
		ans, done := l.app.cards.answer(c.id.DMChannelID, c.thread, "step-3:merge")
		edits := editsOf(l.slack.edited(), c.ts)
		return done && ans.Owed == nil && len(edits) > 0 && strings.Contains(edits[len(edits)-1], "✓ *Merge* · answered")
	})
}

func TestAnAnswerNoSessionHoldsGoesToTheSessionHoldingTheDMAndOwesNothingTheCardRecorded(t *testing.T) {
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
	if len(owed) != 0 {
		t.Fatalf("owed %+v, want nothing owed for a tap the card recorded as its answer", owed)
	}
	edits := editsOf(l.slack.edited(), c.ts)
	if len(edits) == 0 || !strings.Contains(edits[len(edits)-1], "sent to the session holding this DM") || strings.Contains(edits[len(edits)-1], cardWaitAction) {
		t.Fatalf("edits = %v, want the card closed on his pick and naming who heard it", edits)
	}
}

func TestATapPassedOnFromACardWhoseSessionEndedStillTellsTheBoard(t *testing.T) {
	calls := fakePresent(t)
	l := startDaemon(t)
	loggedIn(t, l)
	c := postWaiting(t, l, mergeCard())
	endWatches(t, l, c.thread)
	if _, _, err := insertWatch(l.ctx, l.srv.DB(), l.subID, agent.TopLevel, kindChannel, c.id.DMChannelID, "", "1700000000.000000", time.Now()); err != nil {
		t.Fatal(err)
	}
	before := presentCalls(t, calls)

	l.tap(c.id, mergeTap(c, 2))

	got := strings.TrimPrefix(presentCalls(t, calls), before)
	if !strings.Contains(got, testSession+" slack-sync answered --session "+testSession) || !strings.Contains(got, `"waits":"step-3:merge"`) || !strings.Contains(got, `"label":"Merge"`) {
		t.Fatalf("cc-present calls after the tap %q, want the answer sent to the card's board", got)
	}
}

func TestATapReplayedAfterItsAnswerWasRecordedButNotDeliveredIsDeliveredThen(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	c := postWaiting(t, l, mergeCard())
	tap := mergeTap(c, 2)
	if _, _, err := l.app.cards.claim(c.id.DMChannelID, c.thread, "step-3:merge", askAnswer{At: slackTime(tap.ActionTS), Label: "Merge", Undelivered: tapKey(tap)}); err != nil {
		t.Fatal(err)
	}

	l.tap(c.id, tap)

	if got := l.inject(agent.TopLevel); !strings.Contains(got, `answered the waiting ask "step-3:merge" with "Merge"`) {
		t.Fatalf("session heard %q, want the answer the killed daemon never delivered", got)
	}
	if ans, done := l.app.cards.answer(c.id.DMChannelID, c.thread, "step-3:merge"); !done || ans.Undelivered != "" {
		t.Fatalf("answer = %+v, %v, want it on record and delivered", ans, done)
	}
}

func explainTap(c postedCard, card statusCard) cardActionPush {
	return cardActionPush{User: testUser, Channel: c.id.DMChannelID, MessageTS: c.ts, ThreadTS: c.thread,
		ActionID: cardExplainAction + card.Waits, Value: string(mustJSON(askTap{ID: card.Waits, Label: cardExplainLabel, Question: card.Title})),
		ActionTS: fmt.Sprintf("%d.000050", time.Now().Unix()), Seq: "001800000100000-abcd0140"}
}

func TestAnExplainMoreOnACardWhoseSessionEndedReachesTheSessionHoldingTheDM(t *testing.T) {
	card := statusCard{Status: "needs_you", Title: "Which store keeps the tally?", Waits: "demo:j3-661440", Options: []string{"Ring", "Table"}, Recommended: "Ring"}
	cmd := fakeCardCommand(t, nil)
	l, _ := prLinksDaemon(t, map[string]any{"card_command": cmd.argv})
	c := postWaiting(t, l, card)
	endWatches(t, l, c.thread)
	if _, _, err := insertWatch(l.ctx, l.srv.DB(), l.subID, agent.TopLevel, kindChannel, c.id.DMChannelID, "", "1700000000.000000", time.Now()); err != nil {
		t.Fatal(err)
	}

	l.tap(c.id, explainTap(c, card))

	if got := l.inject(agent.TopLevel); !strings.Contains(got, `explain the waiting ask "demo:j3-661440" more`) || !strings.Contains(got, "the session holding the DM") {
		t.Fatalf("holder heard %q, want the ask to explain the card, sent to it as the session holding the DM", got)
	}
	edits := editsOf(l.slack.edited(), c.ts)
	if len(edits) == 0 || !strings.Contains(edits[len(edits)-1], "You asked for more") || strings.Contains(edits[len(edits)-1], "reached no one") ||
		!strings.Contains(edits[len(edits)-1], cardWaitAction) {
		t.Fatalf("edits = %v, want the card saying an explanation follows, still offering its answers", edits)
	}
	if _, done := l.app.cards.answer(c.id.DMChannelID, c.thread, card.Waits); done {
		t.Fatal("Explain more answered the card")
	}
	if got := answersOf(cmd.requests(t)); len(got) != 0 {
		t.Fatalf("card command asked %v, want no answer, since Explain more answers nothing", got)
	}
}
