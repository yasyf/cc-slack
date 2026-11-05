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
	cardCopyBlock  = cardActionPrefix + "-copy"
	cardCopyLead   = "Answer it here:"
)

var (
	messageLink = regexp.MustCompile(`https://[\w.-]+/archives/[A-Z0-9]+/p\d{16}(?:\?[^\s|>)]*)?`)
	alertLink   = regexp.MustCompile(`https://[\w.-]*(?:sentry\.io/\S*?issues|datadoghq\.com/monitors)/\d+`)
)

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
	res, err := a.post(hc.Ctx, hc.DB, id, id.DMChannelID, b.ThreadTS, cardCopyLead, copied, nil)
	if err != nil {
		return fail(fallback(actionWrite, err))
	}
	a.cards.rememberCopy(id.DMChannelID, b.ThreadTS, ref.ts, res.TS, a.now())
	return ok(res)
}

func (a *app) cardCopy(ctx context.Context, id identity, ref permalinkRef, link string) ([]slack.Block, error) {
	blocks, answers, err := a.copyBlocks(ctx, id, ref, link)
	if err != nil {
		return nil, err
	}
	if answers == 0 {
		return nil, errors.New("that card has no answer left to tap")
	}
	return blocks, nil
}

func (a *app) copyBlocks(ctx context.Context, id identity, ref permalinkRef, link string) ([]slack.Block, int, error) {
	if ref.channelID != id.DMChannelID || ref.ts == "" {
		return nil, 0, fmt.Errorf("%s is not a message in your DM with the app", link)
	}
	card, err := messageAt(ctx, a.newAPI(id.Token), ref.channelID, ref.threadTS, ref.ts)
	if err != nil {
		return nil, 0, err
	}
	var title string
	var blocks []slack.Block
	var buttons []slack.BlockElement
	picks := 0
	for _, block := range card.Blocks.BlockSet {
		switch b := block.(type) {
		case *slack.HeaderBlock:
			title = cmp.Or(title, b.Text.Text)
			blocks = append(blocks, b)
		case *slack.SectionBlock:
			row, isPick := copiedPick(b, ref)
			if isPick {
				picks++
			}
			blocks = append(blocks, row)
		case *slack.ActionBlock:
			buttons = append(buttons, copiedButtons(b, ref)...)
		case *slack.ContextBlock, *slack.TableBlock:
			blocks = append(blocks, block)
		}
	}
	if title == "" {
		return nil, 0, errors.New("that message drew as text, with no card to copy")
	}
	if len(buttons) == 0 {
		return blocks, picks, nil
	}
	return append(blocks, slack.NewActionBlock(cardCopyBlock+"-"+ref.ts, buttons...)), picks + len(buttons), nil
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
		return slack.NewSectionBlock(row.Text, row.Fields, nil, slack.SectionBlockOptionBlockID(row.BlockID)), false
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
		if err != nil {
			p.log.Printf("card copy %s stays as drawn: %v", copied.ts, err)
			continue
		}
		answered := mrkdwn(cardCopyBlock+"-answered", line+" · "+slackLink(id.permalink(card.channel, card.ts, card.thread), "see the card"))
		if _, _, _, err := api.UpdateMessageContext(ctx, copied.channel, copied.ts, slack.MsgOptionText(post.Text, false), slack.MsgOptionBlocks(post.Blocks.BlockSet[0], answered), slack.MsgOptionAttachments([]slack.Attachment{}...)); err != nil {
			p.log.Printf("card copy %s stays as drawn: %v", copied.ts, err)
		}
	}
}
