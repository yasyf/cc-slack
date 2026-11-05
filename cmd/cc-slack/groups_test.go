package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yasyf/cc-interact/agent"
)

func prRow(number int) cardRow {
	return cardRow{ID: fmt.Sprintf("pg:%d:abc1234", number), Text: fmt.Sprintf("*app PR %d* Fix login", number), Buttons: []string{"Approve", "Hold"}}
}

func prRows(n int) []cardRow {
	rows := make([]cardRow, 0, n)
	for i := range n {
		rows = append(rows, prRow(41+i))
	}
	return rows
}

func groupCard(rows ...cardRow) statusCard {
	return statusCard{Status: "needs_you", Title: "Login", Body: "People can sign in again.", Group: "pg:login", Rows: rows}
}

func failingPresent(t *testing.T, reason string) {
	t.Helper()
	bin := t.TempDir()
	message := filepath.Join(bin, "reason")
	if err := os.WriteFile(message, []byte(reason), 0o600); err != nil {
		t.Fatal(err)
	}
	body := "#!/bin/sh\ncat > /dev/null\ncat '" + message + "' >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(bin, presentBinary), []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func rowTap(c postedCard, row cardRow, button, n int) cardActionPush {
	return cardActionPush{User: testUser, Channel: c.id.DMChannelID, MessageTS: c.ts, ThreadTS: c.thread,
		ActionID: fmt.Sprintf("%s%d:%s", cardRowAction, button, row.ID), Value: string(mustJSON(askTap{ID: row.ID, Label: row.Buttons[button]})),
		ActionTS: fmt.Sprintf("%d.%06d", time.Now().Unix(), n), Seq: fmt.Sprintf("001800000100000-abcd%04d", n)}
}

func TestGroupCardRowsCarryTheirOwnButtonsAndShowTheirAnswer(t *testing.T) {
	card := groupCard(prRow(41), prRow(42))
	if err := card.check(time.Now(), false, ""); err != nil {
		t.Fatalf("a well-formed group card: %v", err)
	}
	at := time.Date(2026, 10, 26, 20, 0, 0, 0, time.UTC)
	blocks := card.replyBlocks(cardView{at: at, now: at, answers: map[string]askAnswer{"pg:42:abc1234": {At: at, Label: "Hold"}}})
	buttons := cardButtons(t, blocks)
	got := make([]string, 0, len(buttons))
	for _, b := range buttons {
		got = append(got, b.ActionID+"="+b.Text.Text)
		if k, ok := (cardActionPush{ActionID: b.ActionID, Value: b.Value}).tap(); !ok || !k.Row || k.ID != "pg:41:abc1234" || k.Label != b.Text.Text {
			t.Fatalf("tap on %s = %+v, %v; want a row tap naming the row and its button", b.ActionID, k, ok)
		}
	}
	if want := []string{cardRowAction + "0:pg:41:abc1234=Approve", cardRowAction + "1:pg:41:abc1234=Hold"}; !slices.Equal(got, want) {
		t.Fatalf("buttons = %q, want the open row's own buttons and none on the answered row or for Ack and Pause", got)
	}
	if raw := blocksJSON(t, blocks); !strings.Contains(raw, "*app PR 42* Fix login\\n✓ *Hold* · answered") {
		t.Fatalf("blocks %s, want the answered row to keep its text and show its answer", raw)
	}
	if merged := card.merged(card); merged.Group != "" || merged.Rows != nil {
		t.Fatal("the root card kept the group's rows")
	}
}

func TestGroupCardEditsInPlaceAndAnEmptyGroupIsNeverPosted(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	var skipped map[string]string
	if err := json.Unmarshal(l.do(opDMStatus, map[string]any{"card": groupCard()}).Body, &skipped); err != nil {
		t.Fatal(err)
	}
	if len(l.slack.sent()) != 0 || skipped["ts"] != "" {
		t.Fatalf("an empty group never posted was posted: %+v, result %v", l.slack.sent(), skipped)
	}
	c := postWaiting(t, l, groupCard(prRow(41), prRow(42)))
	posts := len(l.slack.sent())
	again := postWaiting(t, l, groupCard(prRow(42)))
	if again.ts != c.ts || len(l.slack.sent()) != posts {
		t.Fatalf("a re-push posted %d messages at %s, want the group's card at %s edited", len(l.slack.sent())-posts, again.ts, c.ts)
	}
	if edits := editsOf(l.slack.edited(), c.ts); len(edits) == 0 || strings.Contains(edits[len(edits)-1], "PR 41") {
		t.Fatalf("card edits %v, want the answered row gone", edits)
	}
	done := groupCard()
	done.Status, done.Body = "done", "Every pull request here has your answer."
	if emptied := postWaiting(t, l, done); emptied.ts != c.ts {
		t.Fatalf("an emptied group posted at %s, want its card at %s edited", emptied.ts, c.ts)
	}
}

func TestAKeyedCardRedrawsTheCardWithItsKeyInPlace(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	card := statusCard{Status: "info", Title: "The relay fix", Body: "Checks are running.", Key: "r1"}
	c := postWaiting(t, l, card)
	posts := len(l.slack.sent())
	card.Status, card.Body = "done", "Checks passed."
	if again := postWaiting(t, l, card); again.ts != c.ts || len(l.slack.sent()) != posts {
		t.Fatalf("a re-post posted %d messages at %s, want the card at %s edited", len(l.slack.sent())-posts, again.ts, c.ts)
	}
	if edits := editsOf(l.slack.edited(), c.ts); len(edits) == 0 || !strings.Contains(edits[len(edits)-1], "Checks passed.") {
		t.Fatalf("card edits %v, want the new body drawn over the old card", edits)
	}
	card.Key = "r2"
	if other := postWaiting(t, l, card); other.ts == c.ts || len(l.slack.sent()) != posts+1 {
		t.Fatalf("another key posted at %s with %d new messages, want one card of its own", other.ts, len(l.slack.sent())-posts)
	}
}

func TestAGroupCardThatGainsARowPostsAtTheBottomAndDeletesItsEarlierCard(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	c := postWaiting(t, l, groupCard(prRow(41)))
	posts := len(l.slack.sent())
	fresh := postWaiting(t, l, groupCard(prRow(41), prRow(42)))
	if fresh.ts == c.ts || len(l.slack.sent()) != posts+1 {
		t.Fatalf("a group gaining a row posted %d messages at %s, want one new message below the card at %s", len(l.slack.sent())-posts, fresh.ts, c.ts)
	}
	if !slices.ContainsFunc(l.slack.deleted(), func(d fakeEdit) bool { return d.ts == c.ts }) {
		t.Fatalf("deletes %+v, want the superseded card at %s gone", l.slack.deleted(), c.ts)
	}
	if again := postWaiting(t, l, groupCard(prRow(42))); again.ts != fresh.ts {
		t.Fatalf("a re-push with no new row posted at %s, want the card at %s edited", again.ts, fresh.ts)
	}
}

func TestRowTapDecidesOnTheBoardInsteadOfSteeringTheSession(t *testing.T) {
	calls := fakePresent(t)
	l := startDaemon(t)
	loggedIn(t, l)
	row := prRow(41)
	c := postWaiting(t, l, groupCard(row, prRow(42)))
	l.tap(c.id, rowTap(c, row, 0, 500))
	got := presentCalls(t, calls)
	for _, part := range []string{testSession + " slack-sync decided --session " + testSession, `"row":"pg:41:abc1234"`, `"label":"Approve"`,
		`"permalink":` + string(mustJSON(c.url()))} {
		if !strings.Contains(got, part) {
			t.Fatalf("cc-present calls %q lack %s", got, part)
		}
	}
	if steer := l.inject(agent.TopLevel); steer != "" {
		t.Fatalf("a row tap steered the session %q; the board reports it", steer)
	}
	if edits := editsOf(l.slack.edited(), c.ts); len(edits) == 0 || !strings.Contains(edits[len(edits)-1], "✓ *Approve* · answered") ||
		!strings.Contains(edits[len(edits)-1], cardRowAction+"0:pg:42:abc1234") {
		t.Fatalf("card edits %v, want the tapped row answered and the other row still asking", edits)
	}
	l.tap(c.id, rowTap(c, row, 1, 501))
	if again := strings.TrimPrefix(presentCalls(t, calls), got); again != "" {
		t.Fatalf("a second tap on an answered row reached the board: %q", again)
	}
}

func TestRowTapTheBoardRefusesShowsWhyAndLeavesTheRowOpen(t *testing.T) {
	failingPresent(t, `verdict head "abc1234" is not the row's head "def5678"`)
	l := startDaemon(t)
	loggedIn(t, l)
	row := prRow(41)
	c := postWaiting(t, l, groupCard(row))
	l.tap(c.id, rowTap(c, row, 0, 510))
	if edits := editsOf(l.slack.edited(), c.ts); len(edits) == 0 || !strings.Contains(edits[len(edits)-1], "did not reach the board") ||
		!strings.Contains(edits[len(edits)-1], "is not the row's head") {
		t.Fatalf("card edits %v, want the refusal shown on the row", edits)
	}
	if l.app.cards.answered(c.id.DMChannelID, c.thread, row.ID) {
		t.Fatal("a row the board refused reads as answered")
	}
}

func prCardRow() cardRow {
	return cardRow{ID: testWaits, Buttons: []string{"Approve", "Hold"}, PRCard: true, PR: &cardPR{URL: tapPR41[0], Change: "Chats load faster.", Plan: "Checked.", Blocks: "Only your approval."}}
}

func TestAPRCardRowTapAppliesThroughTheCardCommandNotTheBoard(t *testing.T) {
	calls := fakePresent(t)
	cmd := fakeCardCommand(t, []string{"acme/app#41"}, mergeRowCard())
	l, _ := cardingDaemon(t, cmd.argv, "acme/app#41")
	row := prCardRow()
	c := postWaiting(t, l, groupCard(row, prRow(42)))
	l.tap(c.id, rowTap(c, row, 0, 500))
	if got := cmd.requests(t); !slices.ContainsFunc(got, func(ask string) bool {
		return strings.Contains(ask, `"answer"`) && strings.Contains(ask, `"waits":"`+testWaits+`"`) && strings.Contains(ask, `"label":"Approve"`) && strings.Contains(ask, `"repo":"acme/app"`)
	}) {
		t.Fatalf("card command asked %v, want his Approve on the PR", got)
	}
	if onBoard := presentCalls(t, calls); onBoard != "" {
		t.Fatalf("a PR card row reached the board: %q", onBoard)
	}
	if edits := editsOf(l.slack.edited(), c.ts); len(edits) == 0 || !strings.Contains(edits[len(edits)-1], "✓ *Approve* · answered") ||
		!strings.Contains(edits[len(edits)-1], cardRowAction+"0:pg:42:abc1234") {
		t.Fatalf("card edits %v, want the tapped row answered and the other row still asking", edits)
	}
}

func TestARefusedPRCardRowSaysWhyAndCanBeTappedAgain(t *testing.T) {
	cmd, _ := refusingCardCommand(t, "#41 moved to 09b10f1 since the card asked "+testWaits, mergeRowCard())
	l, _ := cardingDaemon(t, cmd.argv, "acme/app#41")
	row := prCardRow()
	c := postWaiting(t, l, groupCard(row))
	l.tap(c.id, rowTap(c, row, 0, 500))
	if edits := editsOf(l.slack.edited(), c.ts); len(edits) == 0 || !strings.Contains(edits[len(edits)-1], "did not count") || !strings.Contains(edits[len(edits)-1], "moved to 09b10f1") {
		t.Fatalf("card edits %v, want the refusal on the row", edits)
	}
	asked := len(cmd.requests(t))
	l.tap(c.id, rowTap(c, row, 0, 501))
	if got := len(cmd.requests(t)); got != asked+1 {
		t.Fatalf("a second tap after a refusal asked %d times, want %d", got, asked+1)
	}
}

func TestAGroupCardCarriesMorePRRowsOnlyWhileTheirWordsAndBlocksFit(t *testing.T) {
	short := make([]cardRow, 0, 15)
	long := make([]cardRow, 0, 15)
	for number := range 15 {
		row := prCardRow()
		row.ID = fmt.Sprintf("merge:%d:e718a74", number)
		pr := *row.PR
		pr.Change = strings.Repeat("words ", 30)
		row.PR = &pr
		short = append(short, row)
		wide := pr
		wide.Change = strings.Repeat("words ", 200)
		row.PR = &wide
		long = append(long, row)
	}
	card := groupCard(short...)
	if err := card.check(time.Now(), false, ""); err != nil {
		t.Fatalf("15 short PR rows: %v", err)
	}
	at := time.Date(2026, 11, 5, 8, 0, 0, 0, time.UTC)
	if blocks := card.replyBlocks(cardView{at: at, now: at}); len(blocks) > cardMaxBlocks {
		t.Fatalf("15 short PR rows draw %d blocks, past Slack's %d", len(blocks), cardMaxBlocks)
	}
	if err := groupCard(long...).check(time.Now(), false, ""); err == nil {
		t.Fatal("15 PR rows past the visible limit were accepted")
	}
	if err := groupCard(long[:cardMaxPRRows]...).check(time.Now(), false, ""); err != nil {
		t.Fatalf("%d PR rows of any length up to the block limit: %v", cardMaxPRRows, err)
	}
}

func TestAPRCardRowNeedsAPR(t *testing.T) {
	row := prRow(41)
	row.PRCard = true
	if err := groupCard(row).check(time.Now(), false, ""); err == nil {
		t.Fatal("a row answering as a PR card with no PR was accepted")
	}
}
