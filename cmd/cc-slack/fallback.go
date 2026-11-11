package main

import (
	"errors"
	"fmt"
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
	mcpThread   = "mcp__slack__slack_get_thread"
	mcpHistory  = "mcp__slack__slack_conversations_history"
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

func fallback(a action, err error) string {
	if errors.As(err, new(inviteNeededError)) || errors.As(err, new(sharedError)) || errors.As(err, new(blockLimitError)) {
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
		return fmt.Sprintf("%v: run `cc-slack login`, or fall back to %s", err, fallbackTool(a))
	}
	if errors.Is(err, daemon.ErrIncumbentNewer) || errors.Is(err, errStaleProcess) {
		return fmt.Sprintf("%v; until then, fall back to %s", err, fallbackTool(a))
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
		return fmt.Sprintf("cc-slack can't watch this conversation (%s): the bot reads only conversations it is a member of, "+
			"so invite it there, or read with %s for a thread or %s for a channel", code, mcpThread, mcpHistory)
	case actionUsers:
		return fmt.Sprintf("cc-slack can't look people up (%s); a user id such as U0123ABCDE resolves through users.info, "+
			"an exact email through users.lookupByEmail and any other query through users.list, or search as you with %s", code, mcpUsers)
	case actionChannels:
		return fmt.Sprintf("cc-slack can't list channels (%s); fall back to %s (lists as you)", code, mcpChannels)
	default:
		return fmt.Sprintf("cc-slack can't read this conversation (%s); fall back to %s for a thread or %s for a channel", code, mcpThread, mcpHistory)
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

func threadReadFailure(err error, named, dm, thread string) string {
	code := errorCode(err)
	searched := named
	if named == "" {
		searched = "your DM with the app, " + dm + ", the conversation a read with no channel_id searches"
	}
	switch code {
	case "user_not_found":
		return fmt.Sprintf("%s is not a conversation the bot can read (%s); your DM with the app is %s, which channel_id may omit", named, code, dm)
	case "channel_not_found":
		return fmt.Sprintf("%s is not a conversation the bot can read (%s); a private channel the bot is not in reads through %s, and your DM with the app is %s, which channel_id may omit", named, code, mcpThread, dm)
	case "thread_not_found":
		return fmt.Sprintf("thread %s is not in %s (%s); pass its channel_id", thread, searched, code)
	}
	return fallback(actionRead, err)
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
		return mcpThread + " / " + mcpHistory
	}
}
