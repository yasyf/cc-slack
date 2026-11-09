package main

import (
	"encoding/json"
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
	if all.TeamURL != "https://acme.slack.com/" || len(listed) != 1 || listed[0].Question != asked.Title || listed[0].Recommended != "Hold" || listed[0].URL != c.url() || len(listed[0].Options) != 2 {
		t.Fatalf("questions = %+v, want only brook's open card with its options and link", listed)
	}
	answerOnBoard(t, l, c, "Hold")
	if listed := driveCards(t, l, "brook").Questions; len(listed) != 0 {
		t.Fatalf("questions after his answer = %+v, want none", listed)
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

func TestDriveCardsListAPRCardPostedAsAWaitingStatusCard(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	stack := mergeCard()
	stack.Waits = "merge:acme:app:41-42"
	stack.PRs = []cardPR{
		{URL: "https://github.com/acme/app/pull/41", Title: "one cache key", Head: "e718a74", Change: "Chats load faster", ELI5: "One key for every chat"},
		{URL: "https://github.com/acme/app/pull/42", Title: "drop the old key", Head: "5c1d2e3", Change: "Nothing you see", ELI5: "The old key goes away"},
	}
	c := postWaiting(t, l, stack)

	listed := driveCards(t, l, "pier")
	if len(listed.Questions) != 0 || len(listed.PRCards) != 1 || listed.PRCards[0].URL != c.url() || len(listed.PRCards[0].PRs) != 2 || listed.PRCards[0].PRs[1] != stack.PRs[1].URL {
		t.Fatalf("drive cards = %+v, want the stack card as one PR card listing both PRs", listed)
	}
	answerOnBoard(t, l, c, "Hold")
	if listed := driveCards(t, l, "pier").PRCards; len(listed) != 0 {
		t.Fatalf("PR cards after his answer = %+v, want none", listed)
	}
}
