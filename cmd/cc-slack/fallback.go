package main

import (
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/slack-go/slack"
	"github.com/yasyf/cc-interact/daemon"
)

type action int

const (
	actionWrite action = iota
	actionRead
	actionReact
	actionUnreact
	actionEdit
	actionDelete
	actionWatch
	actionChannel
	actionFile
	actionUsers
	actionChannels
)

const (
	mcpSend     = "mcp__slack__slack_send_message"
	mcpReaction = "mcp__slack__slack_add_reaction"
	mcpUnreact  = "mcp__slack__slack_remove_reaction"
	mcpUsers    = "mcp__slack__slack_users_search"
	mcpChannels = "mcp__slack__slack_list_conversations"
)

type slackCallError struct {
	method string
	err    error
}

func (e slackCallError) Error() string { return e.method + ": " + e.err.Error() }

func (e slackCallError) Unwrap() error { return e.err }

type outsideDMError struct {
	channelID string
	peer      string
	lookupErr error
	cause     error
}

func (e outsideDMError) Error() string {
	head := fmt.Sprintf("cc-slack can't post here (%s): %s is a 1:1 DM the app cannot join", errorCode(e.cause), e.channelID)
	if e.lookupErr != nil {
		return fmt.Sprintf("%s, and reading its other member as you failed (%v); post to that person as usual with channel_id set to their user id, "+
			"which opens the group DM of that person, you and the app", head, e.lookupErr)
	}
	return fmt.Sprintf("%s, your DM with <@%s>; post to them as usual with channel_id %s, which opens the group DM of that person, you and the app", head, e.peer, e.peer)
}

func (e outsideDMError) Unwrap() error { return e.cause }

func explainsItself(err error) bool {
	return errors.As(err, new(inviteNeededError)) || errors.As(err, new(outsideDMError)) || errors.As(err, new(notConversationError)) ||
		errors.As(err, new(sharedError)) || errors.As(err, new(blockLimitError)) || errors.As(err, new(userConsentError))
}

func fallback(a action, err error) string {
	if explainsItself(err) {
		return err.Error()
	}
	if a == actionEdit {
		return fmt.Sprintf("cc-slack can't edit this message (%s); only the bot can edit its own posts, so no Slack MCP tool stands in", errorCode(err))
	}
	if a == actionDelete {
		return fmt.Sprintf("cc-slack can't delete this message (%s); only the bot can delete its own posts, so no Slack MCP tool stands in", errorCode(err))
	}
	if a == actionChannel {
		return fmt.Sprintf("cc-slack can't change this channel (%s); no Slack MCP tool creates, archives, invites to, or sets the topic of a channel, so ask the user to do it in Slack", errorCode(err))
	}
	if a == actionFile {
		return fmt.Sprintf("cc-slack can't download this file (%s); the bot reads only files shared where it is a member, so no Slack MCP tool stands in", errorCode(err))
	}
	if errors.Is(err, errNoIdentity) {
		return withFallback(fmt.Sprintf("%v: run `cc-slack login`", err), ", or fall back to ", a)
	}
	if errors.Is(err, daemon.ErrIncumbentNewer) || errors.Is(err, errStaleProcess) {
		return withFallback(err.Error(), "; until then, fall back to ", a)
	}
	code := failureCode(err)
	switch a {
	case actionWrite:
		return fmt.Sprintf("cc-slack can't post here (%s); fall back to %s (posts as you, still needs permission)", code, mcpSend)
	case actionReact:
		return fmt.Sprintf("cc-slack can't react here (%s); fall back to %s (reacts as you)", code, mcpReaction)
	case actionUnreact:
		return fmt.Sprintf("cc-slack can't remove a reaction here (%s); fall back to %s (removes yours)", code, mcpUnreact)
	case actionWatch:
		return fmt.Sprintf("cc-slack can't watch this conversation (%s): the bot watches only conversations it is a member of, "+
			"so invite it there, or read it once with slack_thread or slack_history, which read it as you with [oauth] client_id set", code)
	case actionUsers:
		return fmt.Sprintf("cc-slack can't look people up (%s); a user id such as U0123ABCDE resolves through users.info, "+
			"an exact email through users.lookupByEmail and any other query through users.list, or search as you with %s", code, mcpUsers)
	case actionChannels:
		return fmt.Sprintf("cc-slack can't list channels (%s); fall back to %s (lists as you)", code, mcpChannels)
	default:
		return fmt.Sprintf("cc-slack can't read this conversation (%s)", code)
	}
}

func failureCode(err error) string {
	code := errorCode(err)
	if call, ok := errors.AsType[slackCallError](err); ok {
		code = "Slack's " + call.method + " answered " + errorCode(call.err)
	}
	if resp, ok := errors.AsType[slack.SlackErrorResponse](err); ok && len(resp.ResponseMetadata.Messages) > 0 {
		code += ": " + strings.Join(resp.ResponseMetadata.Messages, "; ")
	}
	return code
}

func threadReadFailure(err error, named string, id identity, thread string) string {
	code, dm := errorCode(err), id.DMChannelID
	searched := named
	if named == "" {
		searched = "your DM with the app, " + dm + ", the conversation a read with no channel_id searches"
	}
	switch code {
	case "user_not_found":
		return fmt.Sprintf("%s is not a conversation the bot can read (%s); your DM with the app is %s, which channel_id may omit", named, code, dm)
	case "channel_not_found":
		if id.Config.OAuth.enabled() {
			return fmt.Sprintf("%s is not a conversation the bot or you can read (%s); your DM with the app is %s, which channel_id may omit", named, code, dm)
		}
		return fmt.Sprintf("%s is not a conversation the bot can read (%s); one the bot is not in reads as you once the cc-slack config sets [oauth] client_id, and your DM with the app is %s, which channel_id may omit", named, code, dm)
	case "thread_not_found":
		return fmt.Sprintf("thread %s is not in %s (%s); pass its channel_id", thread, searched, code)
	case "ratelimited":
		rate, _ := errors.AsType[*slack.RateLimitedError](err)
		return fmt.Sprintf("cc-slack can't read thread %s now (%s): Slack limits the read for %ds more, so read the thread again after that", thread, code, int(math.Ceil(rate.RetryAfter.Seconds())))
	}
	return fallback(actionRead, err)
}

func withFallback(msg, join string, a action) string {
	if tool := fallbackTool(a); tool != "" {
		return msg + join + tool
	}
	return msg
}

func fallbackTool(a action) string {
	switch a {
	case actionWrite:
		return mcpSend
	case actionReact:
		return mcpReaction
	case actionUnreact:
		return mcpUnreact
	case actionUsers:
		return mcpUsers
	case actionChannels:
		return mcpChannels
	default:
		return ""
	}
}
