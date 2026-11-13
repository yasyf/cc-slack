package main

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/slack-go/slack"
)

type linkDraw struct {
	channel, thread  string
	cards            []cardRef
	inline, unfurled bool
}

func (l *liveCards) linkDrawIn(channel, thread, at, text string, blocks []slack.Block, unfurled bool) linkDraw {
	d := linkDraw{channel: channel, thread: thread, unfurled: unfurled}
	plain := false
	seen := map[string]bool{}
	eachText(text, blocks, func(t string) string {
		for _, link := range messageLink.FindAllString(withoutTables(t), -1) {
			ref, err := parsePermalink(link)
			if err != nil || ref.channelID == channel && ref.ts == thread || seen[ref.channelID+"/"+ref.ts] {
				continue
			}
			seen[ref.channelID+"/"+ref.ts] = true
			if card, draws := l.draws(channel, thread, at, ref); draws {
				d.cards = append(d.cards, card)
				continue
			}
			plain = true
		}
		return t
	})
	d.inline = len(d.cards) > 0 && (!plain || unfurled)
	return d
}

func (l *liveCards) draws(channel, thread, at string, ref permalinkRef) (cardRef, bool) {
	t, m := l.liveMessage(ref.channelID, ref.ts)
	if m == nil || !t.asks(m) {
		return cardRef{}, false
	}
	card := cardRef{ref.channelID, t.Thread, m.TS}
	return card, !l.shows(channel, thread, at, card)
}

func (l *liveCards) shows(channel, thread, at string, card cardRef) bool {
	if thread == "" {
		return false
	}
	if card.channel == channel && card.thread == thread {
		return true
	}
	t, ok := l.threads[threadKey(channel, thread)]
	return ok && slices.ContainsFunc(t.Copies, func(c liveCardCopy) bool { return c.Card == card.ts && c.TS != at })
}

func (l *liveCards) drawsIn(channel, thread, at string) func(permalinkRef) bool {
	return func(ref permalinkRef) bool {
		l.mu.Lock()
		defer l.mu.Unlock()
		_, draws := l.draws(channel, thread, at, ref)
		return draws
	}
}

func (d linkDraw) marked(text string, blocks []slack.Block, extra []slack.MsgOption) (string, []slack.Block, []slack.MsgOption) {
	if !d.inline && !d.unfurled {
		return text, blocks, extra
	}
	if d.inline {
		post := strconv.FormatInt(time.Now().UnixNano()+freshPosts.Add(1), 36)
		text, blocks = eachText(text, blocks, func(t string) string { return d.fresh(t, post) })
	}
	return text, blocks, append(slices.Clone(extra), slack.MsgOptionEnableLinkUnfurl())
}

func (d linkDraw) fresh(text, post string) string {
	return messageLink.ReplaceAllStringFunc(text, func(link string) string {
		ref, err := parsePermalink(link)
		if err != nil || !slices.ContainsFunc(d.cards, func(c cardRef) bool { return c.channel == ref.channelID && c.ts == ref.ts }) {
			return link
		}
		joint := "?"
		if strings.Contains(link, "?") {
			joint = "&"
		}
		return link + joint + freshLinkParam + "=" + post
	})
}

func (t *liveThread) drew(ts string, cards []cardRef) {
	t.Copies = slices.DeleteFunc(t.Copies, func(c liveCardCopy) bool { return c.TS == ts && c.Link })
	for _, card := range cards {
		t.Copies = append(t.Copies, liveCardCopy{Card: card.ts, TS: ts, Link: true})
	}
}

func (l *liveCards) drew(channel, thread, ts string, cards []cardRef, at time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	t := l.draft(channel, thread)
	before := slices.Clone(t.Copies)
	t.drew(ts, cards)
	if slices.Equal(before, t.Copies) {
		return
	}
	t.Updated = at
	l.commit(t)
}

func alertsUnfurl(id identity, channelID, text string, blocks []slack.Block) bool {
	return channelID == id.DMChannelID && linksTo(text, blocks, alertLink)
}

func (a *app) linkDraw(id identity, channelID, threadTS, text string, blocks []slack.Block, files []string) linkDraw {
	if len(files) > 0 {
		return linkDraw{}
	}
	text = withoutHidden(text)
	a.cards.mu.Lock()
	defer a.cards.mu.Unlock()
	return a.cards.linkDrawIn(channelID, threadTS, "", text, blocks, alertsUnfurl(id, channelID, text, blocks))
}

func (a *app) drewLinks(ctx context.Context, db *sql.DB, id identity, d linkDraw, res postResult) {
	under := cmp.Or(d.thread, res.TS)
	if d.inline {
		a.cards.drew(d.channel, under, res.TS, d.cards, a.now())
		return
	}
	for _, card := range d.cards {
		if err := a.postCardLink(ctx, db, id, d.channel, under, card); err != nil {
			notePostWarning(ctx, fmt.Sprintf("NO CARD: %s links the card %s, which did not draw in its thread: %v", res.Permalink, id.permalink(card.channel, card.ts, card.thread), err))
		}
	}
}

func (a *app) postCard(ctx context.Context, db *sql.DB, id identity, t *liveThread, text string, blocks []slack.Block, extra ...slack.MsgOption) (postResult, error) {
	d := a.cards.linkDrawIn(t.Channel, t.Thread, "", text, blocks, alertsUnfurl(id, t.Channel, text, blocks))
	if !d.inline {
		d = linkDraw{unfurled: d.unfurled}
	}
	text, blocks, extra = d.marked(text, blocks, extra)
	res, err := a.send(ctx, db, id, t.Channel, t.Thread, text, blocks, nil, extra...)
	if err == nil {
		t.drew(res.TS, d.cards)
	}
	return res, err
}
