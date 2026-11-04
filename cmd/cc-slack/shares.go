package main

import (
	"cmp"
	"context"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/slack-go/slack"
)

type sharedMessage struct {
	Permalink  string          `json:"permalink"`
	ChannelID  string          `json:"channel_id"`
	TS         string          `json:"ts"`
	User       string          `json:"user"`
	UserName   string          `json:"user_name"`
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

func (a *app) sharedMessages(ctx context.Context, api slackAPI, m slack.Message) []sharedMessage {
	var out []sharedMessage
	for _, at := range m.Attachments {
		if ref, shared := shareRef(at); shared {
			out = append(out, a.sharedMessage(ctx, api, at, ref))
		}
	}
	return out
}

func (a *app) sharedMessage(ctx context.Context, api slackAPI, at slack.Attachment, ref permalinkRef) sharedMessage {
	share := sharedMessage{
		Permalink: shareLink(at), ChannelID: ref.channelID, TS: ref.ts, User: at.AuthorID, UserName: shareAuthor(at), Text: cmp.Or(at.Text, at.Fallback),
	}
	original, err := a.originalMessage(ctx, api, ref)
	if err != nil {
		share.Unreadable = fallback(actionRead, err)
		return share
	}
	files, err := messageFiles(ctx, api, original)
	if err != nil {
		share.Unreadable = fallback(actionRead, err)
		return share
	}
	share.User, share.UserName = cmp.Or(original.User, at.AuthorID), cmp.Or(a.cache.name(ctx, api, original), share.UserName)
	share.Text, share.Files, share.Shares = messageText(original), files, a.sharedMessages(ctx, api, original)
	return share
}

func (a *app) originalMessage(ctx context.Context, api slackAPI, ref permalinkRef) (slack.Message, error) {
	read, err := a.cache.replies(ctx, api, ref.channelID, ref.threadTS)
	if err != nil {
		return slack.Message{}, err
	}
	i := slices.IndexFunc(read.msgs, func(m slack.Message) bool { return m.Timestamp == ref.ts })
	if i < 0 {
		return slack.Message{}, fmt.Errorf("message %s is no longer in %s", ref.ts, ref.channelID)
	}
	return read.msgs[i], nil
}
