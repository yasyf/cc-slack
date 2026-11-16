package main

import (
	"cmp"
	"context"
	"net/url"
	"slices"
	"strings"

	"github.com/slack-go/slack"
)

type sharedMessage struct {
	Permalink string `json:"permalink"`
	ChannelID string `json:"channel_id"`
	TS        string `json:"ts"`
	User      string `json:"user"`
	UserName  string `json:"user_name"`
	authorship
	Text       string          `json:"text"`
	Files      []messageFile   `json:"files,omitempty"`
	Shares     []sharedMessage `json:"shares,omitempty"`
	Unreadable string          `json:"unreadable,omitempty"`
}

func shareLink(at slack.Attachment) string { return cmp.Or(at.FromURL, at.OriginalURL) }

func shareRef(at slack.Attachment) (permalinkRef, bool) {
	u, err := url.Parse(shareLink(at))
	if err != nil || !strings.HasSuffix(u.Hostname(), ".slack.com") {
		return permalinkRef{}, false
	}
	ref, err := parsePermalink(shareLink(at))
	return ref, err == nil && ref.ts != ""
}

func shareAuthor(at slack.Attachment) string {
	return cmp.Or(at.AuthorName, at.AuthorSubname, at.AuthorID)
}

func linkUnfurls(m slack.Message) []string {
	var out []string
	for _, at := range m.Attachments {
		if _, shared := shareRef(at); !shared && shareLink(at) != "" {
			out = append(out, shareLink(at))
		}
	}
	return out
}

func (a *app) sharedMessages(ctx context.Context, api slackAPI, id identity, m slack.Message) []sharedMessage {
	var out []sharedMessage
	for _, at := range m.Attachments {
		if ref, shared := shareRef(at); shared {
			out = append(out, a.sharedMessage(ctx, api, id, at, ref))
		}
	}
	return out
}

func (a *app) sharedMessage(ctx context.Context, api slackAPI, id identity, at slack.Attachment, ref permalinkRef) sharedMessage {
	share := sharedMessage{
		Permalink: shareLink(at), ChannelID: ref.channelID, TS: ref.ts, User: at.AuthorID, UserName: shareAuthor(at), Text: cmp.Or(at.Text, at.Fallback),
	}
	original, found, err := a.originalMessage(ctx, ref)
	if err != nil || !found {
		share.authorship = a.unfurledAuthorship(ctx, api, id, at, share.Text)
	}
	if err != nil {
		share.Unreadable = fallback(actionRead, err)
		return share
	}
	if !found {
		return share
	}
	share.authorship = id.authorship(original)
	files, err := messageFiles(ctx, api, original)
	if err != nil {
		share.Unreadable = fallback(actionRead, err)
		return share
	}
	share.User, share.UserName = cmp.Or(original.User, at.AuthorID), cmp.Or(a.cache.name(ctx, api, original), share.UserName)
	share.Text, share.Files, share.Shares = ownText(original), files, a.sharedMessages(ctx, api, id, original)
	return share
}

func (a *app) unfurledAuthorship(ctx context.Context, api slackAPI, id identity, at slack.Attachment, text string) authorship {
	// Slack's unfurl of a message names no author_id only when a bot posted it.
	if at.AuthorID == "" {
		return authorship{FromBot: true}
	}
	return id.authorship(slack.Message{Msg: slack.Msg{User: at.AuthorID, BotID: a.cache.user(ctx, api, at.AuthorID).botID, Text: text}})
}

func (a *app) originalMessage(ctx context.Context, ref permalinkRef) (slack.Message, bool, error) {
	msgs, err := a.cache.cached(ctx, ref.channelID, ref.threadTS)
	if err != nil {
		return slack.Message{}, false, err
	}
	i := slices.IndexFunc(msgs, func(m slack.Message) bool { return m.Timestamp == ref.ts })
	if i < 0 {
		return slack.Message{}, false, nil
	}
	return msgs[i], true, nil
}
