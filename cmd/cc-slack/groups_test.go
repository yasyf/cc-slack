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
	if err := card.check(time.Now(), false); err != nil {
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
