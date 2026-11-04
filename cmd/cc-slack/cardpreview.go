package main

import (
	"cmp"
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/slack-go/slack"
)

const (
	cardPreviewAction = cardActionPrefix + ":preview:"
	cardPreviewOpen   = cardLinkAction + "preview"
	cardPreviewBlock  = cardActionPrefix + "-preview"
	cardPreviewLimit  = 5
	cardPreviewChars  = 600
	cardPreviewsLead  = "The cards this names:"
)

var messageLink = regexp.MustCompile(`https://[\w.-]+/archives/[A-Z0-9]+/p\d{16}(?:\?[^\s|>)]*)?`)

// cardPreviews draws a preview for each DM card a post to the DM links to: its
// title, its question and its answer buttons, so he reads and taps it where the
// link stands. A link that is not a card's stays a link.
func (a *app) cardPreviews(ctx context.Context, id identity, channelID string, texts ...string) []slack.Attachment {
	if channelID != id.DMChannelID {
		return nil
	}
	var links []string
	for _, text := range texts {
		for _, link := range messageLink.FindAllString(text, -1) {
			if !slices.Contains(links, link) {
				links = append(links, link)
			}
		}
	}
	api := a.newAPI(id.Token)
	var previews []slack.Attachment
	for _, link := range links {
		ref, err := parsePermalink(link)
		if err != nil || ref.channelID != id.DMChannelID || len(previews) == cardPreviewLimit {
			continue
		}
		card, err := messageAt(ctx, api, ref.channelID, ref.threadTS, ref.ts)
		if err != nil {
			a.poller.log.Printf("preview of %s: %v", link, err)
			continue
		}
		if preview, ok := cardPreview(card, ref, link); ok {
			previews = append(previews, preview)
		}
	}
	return previews
}

func messageAt(ctx context.Context, api slackAPI, channelID, threadTS, ts string) (slack.Message, error) {
	messages, _, _, err := api.GetConversationRepliesContext(ctx, &slack.GetConversationRepliesParameters{ChannelID: channelID, Timestamp: threadTS, Oldest: ts, Latest: ts, Inclusive: true})
	if err != nil {
		return slack.Message{}, err
	}
	at := slices.IndexFunc(messages, func(m slack.Message) bool { return m.Timestamp == ts })
	if at < 0 {
		return slack.Message{}, fmt.Errorf("no message %s in thread %s of %s", ts, threadTS, channelID)
	}
	return messages[at], nil
}

func cardPreview(card slack.Message, ref permalinkRef, link string) (slack.Attachment, bool) {
	var title, body string
	var buttons []slack.BlockElement
	for _, block := range card.Blocks.BlockSet {
		switch b := block.(type) {
		case *slack.HeaderBlock:
			title = cmp.Or(title, b.Text.Text)
		case *slack.SectionBlock:
			if b.Text != nil {
				body = cmp.Or(body, b.Text.Text)
			}
		case *slack.ActionBlock:
			buttons = append(buttons, previewButtons(b, ref)...)
		}
	}
	if title == "" {
		return slack.Attachment{}, false
	}
	open := button(cardPreviewOpen, "Open the card")
	open.URL = link
	return slack.Attachment{Blocks: slack.Blocks{BlockSet: []slack.Block{
		section("", strings.TrimSpace("*"+slackLink(link, title)+"*\n"+clipRunes(body, cardPreviewChars))),
		slack.NewActionBlock(cardPreviewBlock, append(buttons, open)...),
	}}}, true
}

func previewButtons(actions *slack.ActionBlock, ref permalinkRef) []slack.BlockElement {
	var buttons []slack.BlockElement
	for _, element := range actions.Elements.ElementSet {
		b, ok := element.(*slack.ButtonBlockElement)
		if !ok || !strings.HasPrefix(b.ActionID, cardWaitAction) && !strings.HasPrefix(b.ActionID, cardOptionAction) {
			continue
		}
		answer := *b
		answer.ActionID = cardPreviewAction + ref.ts + ":" + ref.threadTS + ":" + b.ActionID
		buttons = append(buttons, &answer)
	}
	return buttons
}

// onCard reads a tap on a preview's button as the same tap on the card itself.
func (a cardActionPush) onCard() (cardActionPush, bool) {
	rest, previewed := strings.CutPrefix(a.ActionID, cardPreviewAction)
	if !previewed {
		return a, false
	}
	ts, rest, _ := strings.Cut(rest, ":")
	thread, action, _ := strings.Cut(rest, ":")
	a.MessageTS, a.ThreadTS, a.ActionID = ts, thread, action
	return a, true
}

// markPreview replaces the tapped preview's buttons with what he tapped, so a
// status he reads later never offers a question he already answered.
func (p *poller) markPreview(ctx context.Context, id identity, a cardActionPush, label string) {
	api := p.newAPI(id.Token)
	post, err := messageAt(ctx, api, a.Channel, a.ThreadTS, a.MessageTS)
	if err != nil {
		p.log.Printf("preview %s stays as drawn: %v", a.MessageTS, err)
		return
	}
	for _, preview := range post.Attachments {
		for n, block := range preview.Blocks.BlockSet {
			if tapped(block, a.ActionID) {
				preview.Blocks.BlockSet[n] = mrkdwn(cardPreviewBlock, "✓ *"+mrkdwnEscapes.Replace(clipRunes(label, cardMaxDetailLen))+"*")
			}
		}
	}
	if _, _, _, err := api.UpdateMessageContext(ctx, a.Channel, a.MessageTS, slack.MsgOptionText(post.Text, false), slack.MsgOptionAttachments(post.Attachments...)); err != nil {
		p.log.Printf("preview %s stays as drawn: %v", a.MessageTS, err)
	}
}

func tapped(block slack.Block, actionID string) bool {
	actions, ok := block.(*slack.ActionBlock)
	return ok && slices.ContainsFunc(actions.Elements.ElementSet, func(element slack.BlockElement) bool {
		b, isButton := element.(*slack.ButtonBlockElement)
		return isButton && b.ActionID == actionID
	})
}
