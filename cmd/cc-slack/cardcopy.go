package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/slack-go/slack"
	"github.com/yasyf/cc-interact/daemon"
)

const (
	cardCopyAction = cardActionPrefix + ":preview:"
	cardCopyOpen   = cardLinkAction + "copy"
	cardCopyBlock  = cardActionPrefix + "-copy"
	cardCopyChars  = 600
	cardCopyLead   = "Answer it here:"
)

var messageLink = regexp.MustCompile(`https://[\w.-]+/archives/[A-Z0-9]+/p\d{16}(?:\?[^\s|>)]*)?`)

func (a *app) handleCardCopy(hc daemon.HandlerCtx) daemon.Reply {
	b, err := decodeBody(hc)
	if err != nil {
		return fail(err.Error())
	}
	if b.URL == "" || b.ThreadTS == "" {
		return fail("card-copy needs url, the open card's permalink, and thread_ts, the thread to post the copy in")
	}
	id, err := loadIdentity(hc.Ctx, hc.DB)
	if err != nil {
		return fail(err.Error())
	}
	ref, err := parsePermalink(b.URL)
	if err != nil {
		return fail(err.Error())
	}
	copied, err := a.cardCopy(hc.Ctx, id, ref, b.URL)
	if err != nil {
		return fail(err.Error())
	}
	res, err := a.post(hc.Ctx, hc.DB, id, id.DMChannelID, b.ThreadTS, cardCopyLead, nil, nil, slack.MsgOptionAttachments(copied))
	if err != nil {
		return fail(fallback(actionWrite, err))
	}
	a.cards.rememberCopy(id.DMChannelID, b.ThreadTS, ref.ts, res.TS, a.now())
	return ok(res)
}

func (a *app) cardCopy(ctx context.Context, id identity, ref permalinkRef, link string) (slack.Attachment, error) {
	if ref.channelID != id.DMChannelID || ref.ts == "" {
		return slack.Attachment{}, fmt.Errorf("%s is not a message in your DM with the app", link)
	}
	card, err := messageAt(ctx, a.newAPI(id.Token), ref.channelID, ref.threadTS, ref.ts)
	if err != nil {
		return slack.Attachment{}, err
	}
	var title, body string
	var rows []slack.Block
	var buttons []slack.BlockElement
	for _, block := range card.Blocks.BlockSet {
		switch b := block.(type) {
		case *slack.HeaderBlock:
			title = cmp.Or(title, b.Text.Text)
		case *slack.SectionBlock:
			if row, isPick := copiedPick(b, ref); isPick {
				rows = append(rows, row)
			} else if b.Text != nil && len(rows) == 0 {
				body = cmp.Or(body, b.Text.Text)
			}
		case *slack.ActionBlock:
			buttons = append(buttons, copiedButtons(b, ref)...)
		}
	}
	if title == "" || len(rows)+len(buttons) == 0 {
		return slack.Attachment{}, errors.New("that card has no answer left to tap")
	}
	open := button(cardCopyOpen, "Open the card")
	open.URL = link
	blocks := append([]slack.Block{section("", strings.TrimSpace("*"+slackLink(link, title)+"*\n"+clipRunes(body, cardCopyChars)))}, rows...)
	return slack.Attachment{Fallback: title, Blocks: slack.Blocks{BlockSet: append(blocks, slack.NewActionBlock(cardCopyBlock+"-"+ref.ts, append(buttons, open)...))}}, nil
}

func answers(actionID string) bool {
	return strings.HasPrefix(actionID, cardWaitAction) || strings.HasPrefix(actionID, cardOptionAction)
}

func copiedAnswer(b *slack.ButtonBlockElement, ref permalinkRef) *slack.ButtonBlockElement {
	answer := *b
	answer.ActionID = cardCopyAction + ref.ts + ":" + ref.threadTS + ":" + b.ActionID
	return &answer
}

func copiedButtons(actions *slack.ActionBlock, ref permalinkRef) []slack.BlockElement {
	var buttons []slack.BlockElement
	for _, element := range actions.Elements.ElementSet {
		if b, isButton := element.(*slack.ButtonBlockElement); isButton && answers(b.ActionID) {
			buttons = append(buttons, copiedAnswer(b, ref))
		}
	}
	return buttons
}

func copiedPick(row *slack.SectionBlock, ref permalinkRef) (slack.Block, bool) {
	if row.Accessory == nil || row.Accessory.ButtonElement == nil || !answers(row.Accessory.ButtonElement.ActionID) {
		return nil, false
	}
	return slack.NewSectionBlock(row.Text, nil, slack.NewAccessory(copiedAnswer(row.Accessory.ButtonElement, ref)), slack.SectionBlockOptionBlockID(row.BlockID)), true
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

func (a cardActionPush) onCard() cardActionPush {
	rest, copied := strings.CutPrefix(a.ActionID, cardCopyAction)
	if !copied {
		return a
	}
	ts, rest, _ := strings.Cut(rest, ":")
	thread, action, _ := strings.Cut(rest, ":")
	a.MessageTS, a.ThreadTS, a.ActionID = ts, thread, action
	return a
}

func (p *poller) markCopies(ctx context.Context, id identity, card cardRef, line string) {
	api := p.newAPI(id.Token)
	for _, copied := range p.cards.copiesOf(card.channel, card.ts) {
		post, err := messageAt(ctx, api, copied.channel, copied.thread, copied.ts)
		if err != nil || len(post.Attachments) == 0 {
			p.log.Printf("card copy %s stays as drawn: %v", copied.ts, err)
			continue
		}
		drawn := post.Attachments[0]
		drawn.Blocks.BlockSet = []slack.Block{drawn.Blocks.BlockSet[0], mrkdwn(cardCopyBlock+"-answered", line+" · "+slackLink(id.permalink(card.channel, card.ts, card.thread), "see the card"))}
		if _, _, _, err := api.UpdateMessageContext(ctx, copied.channel, copied.ts, slack.MsgOptionText(post.Text, false), slack.MsgOptionAttachments(drawn)); err != nil {
			p.log.Printf("card copy %s stays as drawn: %v", copied.ts, err)
		}
	}
}
