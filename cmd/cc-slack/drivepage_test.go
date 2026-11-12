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
	command := cardOwnerCommand(t, `echo '{"heard":true}'`)
	l, _ := prLinksDaemon(t, map[string]any{"card_command": []string{command}})
	asked := mergeCard()
	asked.Drive = "brook"
	c := postDriveCardDrawnInTextThreadBeforeRootPlacement(t, l, asked)
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

func TestDriveCardsKeepEveryClosedCardWithWhatClosedIt(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	answered := mergeCard()
	answered.Drive = "brook"
	a := postWaiting(t, l, answered)
	answerOnBoard(t, l, a, "Hold")
	withdrawn := mergeCard()
	withdrawn.Drive, withdrawn.Waits, withdrawn.Title = "brook", "step-4:cache", "Grow the cache disk?"
	w := postWaiting(t, l, withdrawn)
	l.do(opCardAnswer, map[string]any{"waits": withdrawn.Waits, "url": w.url(), "label": "his order replaced it", "withdrawn": true, "via": w.url()})
	replaced := mergeCard()
	replaced.Drive, replaced.Waits, replaced.Title = "brook", "step-5:deploy", "Deploy before the freeze?"
	r := postWaiting(t, l, replaced)
	again := mergeCard()
	again.Drive, again.Waits, again.Title, again.Supersedes = "brook", "step-5:deploy-again", "Deploy #41 before the freeze?", []string{r.url()}
	fresh := postWaiting(t, l, again)

	listed := driveCards(t, l, "brook")
	byWaits := map[string]driveClosedCard{}
	for _, c := range listed.Closed {
		byWaits[c.Waits] = c
	}
	if c := byWaits[answered.Waits]; c.URL != a.url() || c.Answer == nil || c.Answer.Label != "Hold" || c.Answer.Withdrawn || c.ReplacedBy != "" {
		t.Fatalf("answered card = %+v, want his Hold on it", c)
	}
	if c := byWaits[withdrawn.Waits]; c.URL != w.url() || c.Answer == nil || !c.Answer.Withdrawn || c.Answer.Label != "his order replaced it" || c.Title != withdrawn.Title {
		t.Fatalf("withdrawn card = %+v, want it withdrawn with its reason", c)
	}
	if c := byWaits[replaced.Waits]; c.URL != r.url() || c.ReplacedBy != fresh.url() || c.Answer != nil {
		t.Fatalf("replaced card = %+v, want it pointing at %s", c, fresh.url())
	}
	if len(listed.Closed) != 3 || len(listed.Questions) != 1 || listed.Questions[0].URL != fresh.url() {
		t.Fatalf("closed = %+v, questions = %+v, want three closed cards and the one asking again open", listed.Closed, listed.Questions)
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
	again := postWaiting(t, l, followUp)

	if listed := driveCards(t, l, "brook").Questions; len(listed) != 1 || again.ts == c.ts || listed[0].URL != again.url() || listed[0].Question != followUp.Title {
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

func TestDriveCardsDropACardAnsweredOnACopyLinkedWithoutItsThreadThatNoSessionHolds(t *testing.T) {
	command := cardOwnerCommand(t, `echo '{"heard":true}'`)
	l, _ := prLinksDaemon(t, map[string]any{"card_command": []string{command}})
	asked := mergeCard()
	asked.Drive = "brook"
	c := postDriveCardDrawnInTextThreadBeforeRootPlacement(t, l, asked)
	copied, merge := copyCard(t, l, c.bareLink(), postThread(t, l, "Elsewhere."))
	endWatches(t, l, c.thread)

	tapCopy(l, c, copied, merge)

	if ans, done := l.app.cards.answer(c.id.DMChannelID, c.thread, "step-3:merge"); !done || ans.Label != "Merge" {
		t.Fatalf("answer = %+v, %v, want Merge on record for the card in thread %s", ans, done, c.thread)
	}
	if listed := driveCards(t, l, "brook").Questions; len(listed) != 0 {
		t.Fatalf("questions after his tap = %+v, want none", listed)
	}
}

func TestDriveCardsListOneCardWhereACardLinkedWithoutItsThreadIsReplacedInPlace(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	asked := mergeCard()
	asked.Drive = "brook"
	c := postDriveCardDrawnInTextThreadBeforeRootPlacement(t, l, asked)
	again := mergeCard()
	again.Drive, again.Waits, again.Title, again.Replaces = "brook", "step-4:merge", "Merge #41 and #42 now?", c.bareLink()
	l.do(opDMStatus, map[string]any{"card": again})

	if listed := driveCards(t, l, "brook").Questions; len(listed) != 1 || listed[0].URL != c.url() || listed[0].Question != again.Title {
		t.Fatalf("questions = %+v, want the one card at %s, now asking %q", listed, c.url(), again.Title)
	}
}
