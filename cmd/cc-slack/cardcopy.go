package main

import (
	"bytes"
	"cmp"
	"context"
	"database/sql"
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
	res, err := a.post(hc.Ctx, hc.DB, id, id.DMChannelID, b.ThreadTS, cardCopyLead, copied, nil, a.cards.workObjectsOf(ref.channelID, ref.ts, a.now())...)
	if err != nil {
		return fail(fallback(actionWrite, err))
	}
	a.cards.rememberCopy(id.DMChannelID, b.ThreadTS, liveCardCopy{Card: ref.ts, TS: res.TS}, a.now())
	return ok(res)
}

func (a *app) askAgain(ctx context.Context, db *sql.DB, id identity, card cardRef) error {
	if err := a.cards.redrawCopies(ctx, id, card); err != nil {
		return err
	}
	link := id.permalink(card.channel, card.ts, card.thread)
	ref, err := parsePermalink(link)
	if err != nil {
		return err
	}
	copied, err := a.cardCopy(ctx, id, ref, link)
	if err != nil {
		return err
	}
	under := card.thread
	if under == card.ts {
		under = ""
	}
	res, err := a.post(ctx, db, id, card.channel, under, cardCopyLead, copied, nil, a.cards.workObjectsOf(card.channel, card.ts, a.now())...)
	if err != nil {
		return err
	}
	a.cards.rememberCopy(card.channel, cmp.Or(under, res.TS), liveCardCopy{Card: card.ts, TS: res.TS}, a.now())
	return nil
}

func sameCard(was, now statusCard) bool {
	was.Images, now.Images = nil, nil
	return bytes.Equal(mustJSON(was), mustJSON(now))
}

func (a *app) cardCopy(ctx context.Context, id identity, ref permalinkRef, link string) ([]slack.Block, error) {
	blocks, answers, err := copyBlocks(ctx, a.newAPI(id.Token), id, ref, link)
	if err != nil {
		return nil, err
	}
	if answers == 0 {
		return nil, errors.New("that card has no answer left to tap")
	}
	return blocks, nil
}

func copyBlocks(ctx context.Context, api slackAPI, id identity, ref permalinkRef, link string) ([]slack.Block, int, error) {
	if ref.channelID != id.DMChannelID || ref.ts == "" {
		return nil, 0, fmt.Errorf("%s is not a message in your DM with the app", link)
	}
	card, err := messageAt(ctx, api, ref.channelID, ref.threadTS, ref.ts)
	if err != nil {
		return nil, 0, err
	}
	blocks, answers := copiedCard(card.Blocks.BlockSet, ref)
	if blocks == nil {
		return nil, 0, errors.New("that message drew as text, with no card to copy")
	}
	return blocks, answers, nil
}

func copyLead(blocks []slack.Block, answers int) string {
	if answers > 0 {
		return cardCopyLead
	}
	for _, block := range blocks {
		if header, isHeader := block.(*slack.HeaderBlock); isHeader {
			return header.Text.Text
		}
	}
	panic("a copied card always carries its header")
}

func copiedCard(card []slack.Block, ref permalinkRef) ([]slack.Block, int) {
	var title string
	var blocks []slack.Block
	var buttons, links []slack.BlockElement
	picks := 0
	for _, block := range card {
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
			answered, linked := copiedButtons(b, ref)
			buttons, links = append(buttons, answered...), append(links, linked...)
		case *slack.ContextBlock, *slack.DataTableBlock:
			blocks = append(blocks, block)
		}
	}
	if title == "" {
		return nil, 0
	}
	if len(buttons) == 0 {
		return blocks, picks
	}
	return append(blocks, slack.NewActionBlock(cardCopyBlock+"-"+ref.ts, append(buttons, links...)...)), picks + len(buttons)
}

func carriedCard(card []slack.Block, ref permalinkRef) slack.Block {
	blocks, _ := copiedCard(card, ref)
	if blocks == nil {
		return nil
	}
	var title, detail, note string
	var answer *slack.ButtonBlockElement
	offer := func(b *slack.ButtonBlockElement) {
		if answer == nil && strings.HasPrefix(b.ActionID, cardCopyAction) {
			answer = b
		}
	}
	for _, block := range blocks {
		switch b := block.(type) {
		case *slack.HeaderBlock:
			title = cmp.Or(title, "*"+mrkdwnEscapes.Replace(b.Text.Text)+"*")
		case *slack.SectionBlock:
			if b.Text != nil {
				detail = cmp.Or(detail, b.Text.Text)
			}
			if b.Accessory != nil && b.Accessory.ButtonElement != nil {
				offer(b.Accessory.ButtonElement)
			}
		case *slack.ContextBlock:
			for _, element := range b.ContextElements.Elements {
				if text, isText := element.(*slack.TextBlockObject); isText {
					note = cmp.Or(note, text.Text)
				}
			}
		case *slack.ActionBlock:
			for _, element := range b.Elements.ElementSet {
				if button, isButton := element.(*slack.ButtonBlockElement); isButton {
					offer(button)
				}
			}
		}
	}
	text := strings.TrimSpace(title + "\n" + cmp.Or(detail, note))
	row := slack.NewSectionBlock(slack.NewTextBlockObject(slack.MarkdownType, clipRunes(text, cardMaxSectionLen), false, false), nil, nil, slack.SectionBlockOptionBlockID(cardCopyBlock+"-"+ref.ts))
	if answer != nil {
		row.Accessory = slack.NewAccessory(answer)
	}
	return row
}

func answers(actionID string) bool {
	return strings.HasPrefix(actionID, cardWaitAction) || strings.HasPrefix(actionID, cardOptionAction)
}

func copiedAnswer(b *slack.ButtonBlockElement, ref permalinkRef) *slack.ButtonBlockElement {
	answer := *b
	answer.ActionID = cardCopyAction + ref.ts + ":" + ref.threadTS + ":" + b.ActionID
	return &answer
}

func copiedButtons(actions *slack.ActionBlock, ref permalinkRef) (answered, links []slack.BlockElement) {
	for _, element := range actions.Elements.ElementSet {
		b, isButton := element.(*slack.ButtonBlockElement)
		switch {
		case !isButton:
		case answers(b.ActionID):
			answered = append(answered, copiedAnswer(b, ref))
		case strings.HasPrefix(b.ActionID, cardLinkAction+"github-"):
			links = append(links, b)
		}
	}
	return answered, links
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

func (l *liveCards) markCopies(ctx context.Context, id identity, card cardRef, line string) {
	api := l.newAPI(id.Token)
	undo := l.undoing(card)
	for _, copied := range l.copiesOf(card.channel, card.ts) {
		post, err := messageAt(ctx, api, copied.channel, copied.thread, copied.ts)
		if err != nil {
			l.log.Printf("card copy %s stays as drawn: %v", copied.ts, err)
			continue
		}
		if err := markCopy(ctx, api, id, card, copied, post, line, undo); err != nil {
			l.log.Printf("card copy %s stays as drawn: %v", copied.ts, err)
		}
	}
}

func (l *liveCards) redrawCopies(ctx context.Context, id identity, card cardRef) error {
	copies := l.copiesOf(card.channel, card.ts)
	if len(copies) == 0 {
		return nil
	}
	link := id.permalink(card.channel, card.ts, card.thread)
	ref, err := parsePermalink(link)
	if err != nil {
		return err
	}
	api := l.newAPI(id.Token)
	blocks, answers, err := copyBlocks(ctx, api, id, ref, link)
	if err != nil {
		return err
	}
	entities := l.workObjectsOf(card.channel, card.ts, l.now())
	for _, copied := range copies {
		if err := updateMessage(ctx, api, copied.channel, copied.ts, copyLead(blocks, answers), blocks, entities...); err != nil {
			return err
		}
	}
	return nil
}

func markCopy(ctx context.Context, api slackAPI, id identity, card, copied cardRef, post slack.Message, line, undo string) error {
	blocks := []slack.Block{post.Blocks.BlockSet[0], mrkdwn(cardCopyBlock+"-answered", line+" · "+slackLink(id.permalink(card.channel, card.ts, card.thread), "see the card"))}
	if undo != "" {
		blocks = append(blocks, slack.NewActionBlock(cardCopyBlock+"-undo", button(cardCopyAction+card.ts+":"+card.thread+":"+cardUndoAction+undo, cardUndoLabel)))
	}
	_, _, _, err := api.UpdateMessageContext(ctx, copied.channel, copied.ts, slack.MsgOptionText(post.Text, false), slack.MsgOptionBlocks(blocks...), slack.MsgOptionAttachments([]slack.Attachment{}...))
	return err
}
