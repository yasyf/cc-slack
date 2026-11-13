package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"

	"github.com/slack-go/slack"
	"github.com/yasyf/cc-interact/daemon"
)

const opOwnChannel daemon.Op = "own-channel"

type ownChannelResult struct {
	ChannelID string `json:"channel_id"`
	Name      string `json:"name"`
	Permalink string `json:"permalink"`
	Created   bool   `json:"created"`
}

// at is the identity for a post or tap in channelID, a channel of the user's own, which the DM's code then serves in place of the DM.
func (id identity) at(channelID string) identity {
	id.DMChannelID = channelID
	return id
}

// ownChannel reports whether channelID reaches the user alone, as the DM with the app does: the DM, or an unarchived private
// channel whose members are exactly the user and the bot. It reads Slack on every call, so a channel the user adds anyone
// to stops being theirs at once.
func ownChannel(ctx context.Context, api slackAPI, id identity, channelID string) (bool, error) {
	if channelID == id.DMChannelID {
		return true, nil
	}
	info, err := api.GetConversationInfoContext(ctx, &slack.GetConversationInfoInput{ChannelID: channelID})
	if err != nil && errorCode(err) == "channel_not_found" {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("conversations.info %s: %w", channelID, err)
	}
	if !info.IsPrivate || info.IsArchived {
		return false, nil
	}
	members, next, err := api.GetUsersInConversationContext(ctx, &slack.GetUsersInConversationParameters{ChannelID: channelID, Limit: 3})
	if err != nil {
		return false, fmt.Errorf("conversations.members %s: %w", channelID, err)
	}
	return next == "" && slices.Equal(slices.Sorted(slices.Values(members)), slices.Sorted(slices.Values([]string{id.UserID, id.BotUserID}))), nil
}

// postingIdentity is the identity dm-status posts as: the DM's, or channelID's when it is a private channel of the user and the bot alone.
func (a *app) postingIdentity(ctx context.Context, db *sql.DB, channelID string) (identity, error) {
	id, err := loadIdentity(ctx, db)
	if err != nil {
		return id, errors.New(fallback(actionWrite, err))
	}
	if channelID == "" {
		return id, nil
	}
	own, err := ownChannel(ctx, a.newAPI(id.Token), id, channelID)
	if err != nil {
		return id, errors.New(fallback(actionWrite, err))
	}
	if !own {
		return id, fmt.Errorf("dm-status posts in your DM or a private channel of you and the bot alone, and %s is neither", channelID)
	}
	return id.at(channelID), nil
}

func (a *app) handleOwnChannel(hc daemon.HandlerCtx) daemon.Reply {
	b, err := decodeBody(hc)
	if err != nil {
		return fail(err.Error())
	}
	if b.Name == "" {
		return fail("own-channel needs name")
	}
	id, err := loadIdentity(hc.Ctx, hc.DB)
	if err != nil {
		return fail(fallback(actionChannel, err))
	}
	user, err := a.userAPI(hc.Ctx, id)
	if err != nil {
		return fail(err.Error())
	}
	channelID, created, err := privateChannel(hc.Ctx, user, b.Name)
	if err != nil {
		return fail(fallback(actionChannel, err))
	}
	if err := a.inviteBot(hc.Ctx, id, channelID); err != nil {
		return fail(err.Error())
	}
	own, err := ownChannel(hc.Ctx, a.newAPI(id.Token), id, channelID)
	if err != nil {
		return fail(fallback(actionChannel, err))
	}
	if !own {
		return fail(fmt.Sprintf("#%s (%s) holds people besides you and the bot, so posts there would reach them; name another channel", b.Name, channelID))
	}
	return ok(ownChannelResult{ChannelID: channelID, Name: b.Name, Permalink: id.channelLink(channelID), Created: created})
}

// privateChannel makes a private channel named name as the user, or finds the one of theirs that already holds the name.
func privateChannel(ctx context.Context, user slackAPI, name string) (string, bool, error) {
	ch, err := user.CreateConversationContext(ctx, slack.CreateConversationParams{ChannelName: name, IsPrivate: true})
	if err == nil {
		return ch.ID, true, nil
	}
	if errorCode(err) != "name_taken" {
		return "", false, fmt.Errorf("conversations.create %s: %w", name, err)
	}
	cursor := ""
	for {
		page, next, err := user.GetConversationsContext(ctx, &slack.GetConversationsParameters{
			Cursor: cursor, ExcludeArchived: true, Limit: listPageLimit, Types: []string{"public_channel", "private_channel"},
		})
		if err != nil {
			return "", false, fmt.Errorf("conversations.list: %w", err)
		}
		if i := slices.IndexFunc(page, func(c slack.Channel) bool { return c.Name == name && c.IsPrivate }); i >= 0 {
			return page[i].ID, false, nil
		}
		if next == "" {
			return "", false, fmt.Errorf("#%s is taken by a channel that is public or that you are not in; name another channel", name)
		}
		cursor = next
	}
}
