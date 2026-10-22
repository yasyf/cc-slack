package main

import (
	"errors"
	"fmt"

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
)

const (
	mcpSend     = "mcp__slack__slack_send_message"
	mcpThread   = "mcp__slack__slack_get_thread"
	mcpHistory  = "mcp__slack__slack_conversations_history"
	mcpReaction = "mcp__slack__slack_add_reaction"
	mcpUnreact  = "mcp__slack__slack_remove_reaction"
)

func fallback(a action, err error) string {
	if errors.As(err, new(inviteNeededError)) || errors.As(err, new(sharedError)) {
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
	code := errorCode(err)
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
	default:
		return fmt.Sprintf("cc-slack can't read this conversation (%s); fall back to %s for a thread or %s for a channel", code, mcpThread, mcpHistory)
	}
}

func fallbackTool(a action) string {
	switch a {
	case actionWrite:
		return mcpSend
	case actionReact:
		return mcpReaction
	case actionUnreact:
		return mcpUnreact
	default:
		return mcpThread + " / " + mcpHistory
	}
}
