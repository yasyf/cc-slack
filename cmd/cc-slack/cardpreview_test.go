package main

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/slack-go/slack"
)

func previewButtonsOf(t *testing.T, post fakePost) []*slack.ButtonBlockElement {
	t.Helper()
	var previews []slack.Attachment
	if err := json.Unmarshal([]byte(post.attachments), &previews); err != nil {
		t.Fatalf("attachments %q: %v", post.attachments, err)
	}
	var buttons []*slack.ButtonBlockElement
	for _, preview := range previews {
		for _, block := range preview.Blocks.BlockSet {
			if actions, ok := block.(*slack.ActionBlock); ok {
				for _, element := range actions.Elements.ElementSet {
					buttons = append(buttons, element.(*slack.ButtonBlockElement))
				}
			}
		}
	}
	return buttons
}

func lastPost(l *liveDaemon) fakePost {
	posts := l.slack.sent()
	return posts[len(posts)-1]
}

func TestAPostLinkingACardShowsItAndATapOnThePreviewAnswersTheCard(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	l.do(opDMStatus, cardBody("Merge the stack?", "merge it"))
	id, err := loadIdentity(l.ctx, l.srv.DB())
	if err != nil {
		t.Fatal(err)
	}
	card := l.slack.sent()[0].ts
	link := id.permalink(id.DMChannelID, card, "")

	l.do(opDMStatus, map[string]any{"text": "Waiting on <" + link + "|the merge card>"})

	post := lastPost(l)
	if !strings.Contains(post.attachments, "Merge the stack?") || !strings.Contains(post.attachments, link) || post.unfurlLinks != "false" {
		t.Fatalf("post = %+v, want the card's title and link in a preview and no unfurl", post)
	}
	buttons := previewButtonsOf(t, post)
	at := slices.IndexFunc(buttons, func(b *slack.ButtonBlockElement) bool { return strings.HasPrefix(b.ActionID, cardPreviewAction) })
	if at < 0 || buttons[len(buttons)-1].URL != link {
		t.Fatalf("buttons = %+v, want the card's answer and one that opens it", buttons)
	}

	l.tap(id, cardActionPush{User: testUser, Channel: id.DMChannelID, MessageTS: post.ts, ThreadTS: post.ts,
		ActionID: buttons[at].ActionID, Value: buttons[at].Value, ActionTS: "1800000100.000000", Seq: "001800000100000-abcd0123"})

	if edits := editsOf(l.slack.edited(), card); len(edits) != 1 || !strings.Contains(edits[0], "✓ *merge it*") {
		t.Fatalf("card edits = %v, want the card's buttons replaced by the answer", edits)
	}
	var marked []fakeEdit
	for _, edit := range l.slack.edited() {
		if edit.ts == post.ts {
			marked = append(marked, edit)
		}
	}
	if len(marked) != 1 || !strings.Contains(marked[0].attachments, "✓ *merge it*") || strings.Contains(marked[0].attachments, cardPreviewAction) || marked[0].text != post.text {
		t.Fatalf("preview edits = %+v, want its text kept and its buttons replaced by the answer", marked)
	}
}

func TestAStatusCardLinkingACardCarriesThePreviewInItsThread(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	l.do(opDMStatus, cardBody("Merge the stack?", "merge it"))
	id, err := loadIdentity(l.ctx, l.srv.DB())
	if err != nil {
		t.Fatal(err)
	}
	link := id.permalink(id.DMChannelID, l.slack.sent()[0].ts, "")

	l.do(opDMStatus, map[string]any{"card": statusCard{Status: "info", Title: "Status", Body: "Waiting on " + link}})

	post := lastPost(l)
	if post.text != cardPreviewsLead || post.threadTS == "" || !strings.Contains(post.attachments, "Merge the stack?") {
		t.Fatalf("post = %+v, want the preview in the status card's thread", post)
	}
}

func TestOnlyACardInTheDMDrawsAPreview(t *testing.T) {
	l := startDaemon(t)
	loggedIn(t, l)
	l.do(opDMStatus, cardBody("Merge the stack?", "merge it"))
	id, err := loadIdentity(l.ctx, l.srv.DB())
	if err != nil {
		t.Fatal(err)
	}
	card := id.permalink(id.DMChannelID, l.slack.sent()[0].ts, "")

	l.do(opDMStatus, map[string]any{"text": "plain words"})
	plain := id.permalink(id.DMChannelID, lastPost(l).ts, "")
	l.do(opDMStatus, map[string]any{"text": "See " + plain})
	if got := lastPost(l); got.attachments != "" {
		t.Fatalf("a link to plain text drew %q", got.attachments)
	}

	l.post(opSend, map[string]string{"channel_id": testChannel, "text": "See " + card})
	if got := lastPost(l); got.channelID != testChannel || got.attachments != "" {
		t.Fatalf("a post outside the DM carried a DM card: %+v", got)
	}
}
