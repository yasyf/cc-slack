package main

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/slack-go/slack"
)

func driveCards(t *testing.T, l *liveDaemon, drive string) driveCardsResult {
	t.Helper()
	var res driveCardsResult
	if err := json.Unmarshal(l.do(opDriveCards, map[string]any{"name": drive}).Body, &res); err != nil {
		t.Fatal(err)
	}
	return res
}

func drawCanvas(t *testing.T, l *liveDaemon, body map[string]any) canvasResult {
	t.Helper()
	var res canvasResult
	if err := json.Unmarshal(l.do(opCanvas, body).Body, &res); err != nil {
		t.Fatal(err)
	}
	return res
}

func TestDriveCardsListTheDrivesOpenQuestionsUntilHeAnswers(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	asked := mergeCard()
	asked.Drive = "brook"
	c := postWaiting(t, l, asked)
	elsewhere := mergeCard()
	elsewhere.Drive, elsewhere.Waits = "other", "other:merge"
	postWaiting(t, l, elsewhere)

	all := driveCards(t, l, "brook")
	listed := all.Questions
	if all.TeamURL != "https://acme.slack.com/" || len(listed) != 1 || listed[0].Question != asked.Title || listed[0].Recommended != "Hold" || listed[0].URL != c.url() || len(listed[0].Options) != 2 || listed[0].Card.Title != asked.Title {
		t.Fatalf("questions = %+v, want only brook's open card with its options and link", listed)
	}
	answerOnBoard(t, l, c, "Hold")
	if listed := driveCards(t, l, "brook").Questions; len(listed) != 0 {
		t.Fatalf("questions after his answer = %+v, want none", listed)
	}
}

func TestATapThatNamesItsCardAsTheThreadSettlesTheCardInTheThreadThatHoldsIt(t *testing.T) {
	command, _ := cardOwnerCommand(t, `echo '{"heard":true}'`)
	l, _ := prLinksDaemon(t, map[string]any{"card_command": []string{command}})
	var root map[string]string
	if err := json.Unmarshal(l.do(opDMStatus, map[string]any{"text": "Cloud agents"}).Body, &root); err != nil {
		t.Fatal(err)
	}
	asked := mergeCard()
	asked.Drive = "brook"
	var res map[string]string
	if err := json.Unmarshal(l.do(opDMStatus, map[string]any{"card": asked, "thread_ts": root["ts"]}).Body, &res); err != nil {
		t.Fatal(err)
	}
	id, err := loadIdentity(l.ctx, l.srv.DB())
	if err != nil {
		t.Fatal(err)
	}
	c := postedCard{id: id, thread: res["thread_ts"], ts: res["ts"]}
	if c.thread != root["ts"] || c.ts == c.thread {
		t.Fatalf("card at %s in thread %s, want a reply in thread %s", c.ts, c.thread, root["ts"])
	}
	endWatches(t, l, c.thread)

	tap := mergeTap(c, 2)
	tap.ThreadTS = c.ts
	l.tap(c.id, tap)

	if ans, done := l.app.cards.answer(c.id.DMChannelID, c.thread, "step-3:merge"); !done || ans.Label != "Merge" {
		t.Fatalf("answer = %+v, %v, want his pick on record in the card's own thread", ans, done)
	}
	if listed := driveCards(t, l, "brook").Questions; len(listed) != 0 {
		t.Fatalf("questions after his tap = %+v, want none", listed)
	}
}

func TestACardAskedAgainUnderANewIDTakesTheOneItSupersedesOffTheOpenList(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	first := mergeCard()
	first.Drive = "brook"
	c := postWaiting(t, l, first)
	again := mergeCard()
	again.Drive, again.Waits, again.Title, again.Supersedes = "brook", "step-3:merge-again", "Merge #41 before the freeze?", []string{c.url()}
	fresh := postWaiting(t, l, again)

	if listed := driveCards(t, l, "brook").Questions; len(listed) != 1 || listed[0].URL != fresh.url() {
		t.Fatalf("questions = %+v, want only the card that asks again", listed)
	}
}

func TestAFollowUpAskedUnderTheOpenCardsIDLeavesOneCardOpen(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	first := mergeCard()
	first.Drive = "brook"
	c := postWaiting(t, l, first)
	followUp := mergeCard()
	followUp.Drive, followUp.Title = "brook", "Merge #41 before the freeze, or after it?"
	postWaiting(t, l, followUp)

	if listed := driveCards(t, l, "brook").Questions; len(listed) != 1 || listed[0].URL != c.url() || listed[0].Question != followUp.Title {
		t.Fatalf("questions = %+v, want the one card, now asking the follow-up", listed)
	}
}

func TestCanvasTabsAPageInHisDMRedrawsItAndRecreatesOneHeDeleted(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)

	made := drawCanvas(t, l, map[string]any{"name": "brook", "text": "# Your queue"})
	if page := l.slack.canvasPages()[made.CanvasID]; !made.Created || page != (fakeCanvas{"D" + testUser, "brook", "# Your queue"}) || made.URL != "https://acme.slack.com/docs/T1/"+made.CanvasID {
		t.Fatalf("made %+v drawing %+v, want a canvas tabbed in his DM", made, page)
	}
	redrawn := drawCanvas(t, l, map[string]any{"name": "brook", "file_id": made.CanvasID, "text": "# Status"})
	if page := l.slack.canvasPages()[made.CanvasID].page; redrawn.Created || redrawn.CanvasID != made.CanvasID || page != "# Status" {
		t.Fatalf("redrawn %+v page %q, want the same canvas redrawn in place", redrawn, page)
	}

	l.slack.fail("canvases.edit", slack.SlackErrorResponse{Err: "canvas_deleted"})
	again := drawCanvas(t, l, map[string]any{"name": "brook", "file_id": made.CanvasID, "text": "# Again"})
	if !again.Created || again.CanvasID == made.CanvasID || l.slack.canvasPages()[again.CanvasID].page != "# Again" {
		t.Fatalf("after he deleted it: %+v, want a new canvas in his DM", again)
	}
}

func TestCanvasSlackCallsFullIsDeletedAndDrawnOnAFreshTabOnlyWhenItIsThisAppsTabOfThatTitle(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	made := drawCanvas(t, l, map[string]any{"name": "brook", "text": "# Your queue"})
	other := drawCanvas(t, l, map[string]any{"name": "pier", "text": "# Pier"})

	l.slack.fail("canvases.edit", slack.SlackErrorResponse{Err: "canvas_too_large"})
	l.refused(opCanvas, map[string]any{"name": "brook", "file_id": other.CanvasID, "text": "# Status"}, `titled "pier"`)
	if deleted, pages := l.slack.deletedCanvases(), l.slack.canvasPages(); len(deleted) != 0 || len(pages) != 2 {
		t.Fatalf("deleted %v pages %v, want both canvases kept", deleted, pages)
	}

	l.slack.fail("canvases.edit", slack.SlackErrorResponse{Err: "canvas_too_large"})
	fresh := drawCanvas(t, l, map[string]any{"name": "brook", "file_id": made.CanvasID, "text": "# Status"})
	if !fresh.Created || fresh.Replaced != made.CanvasID || l.slack.canvasPages()[fresh.CanvasID] != (fakeCanvas{"D" + testUser, "brook", "# Status"}) {
		t.Fatalf("fresh %+v, want a new brook tab in his DM drawing the page", fresh)
	}
	if deleted := l.slack.deletedCanvases(); !slices.Equal(deleted, []string{made.CanvasID}) {
		t.Fatalf("deleted %v, want only the full brook canvas", deleted)
	}
}

func TestDriveCardsListAPRCardPostedAsAWaitingStatusCard(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	stack := mergeCard()
	stack.PRs = []cardPR{
		{URL: "https://github.com/acme/app/pull/41", Title: "one cache key", Head: "e718a74", Surprise: "Chats load faster", Refresher: "One key for every chat"},
		{URL: "https://github.com/acme/app/pull/42", Title: "drop the old key", Head: "5c1d2e3", Surprise: "Nothing you see", Refresher: "The old key goes away"},
	}
	c := postWaiting(t, l, stack)

	listed := driveCards(t, l, "pier")
	if len(listed.Questions) != 0 || len(listed.PRCards) != 1 || listed.PRCards[0].URL != c.url() || len(listed.PRCards[0].PRs) != 2 || listed.PRCards[0].PRs[1] != stack.PRs[1].URL || listed.PRCards[0].Card.PRs[0].Surprise != stack.PRs[0].Surprise {
		t.Fatalf("drive cards = %+v, want the stack card as one PR card listing both PRs", listed)
	}
	answerOnBoard(t, l, c, "Hold")
	if listed := driveCards(t, l, "pier").PRCards; len(listed) != 0 {
		t.Fatalf("PR cards after his answer = %+v, want none", listed)
	}
}
