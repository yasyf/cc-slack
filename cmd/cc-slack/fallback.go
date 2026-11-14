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

type asUserError struct {
	botErr  error
	userErr error
	called  bool
}

func (e asUserError) Error() string {
	return fmt.Sprintf("Slack refused the bot (%s), and sending it as you failed: %v", failureCode(e.botErr), e.userErr)
}

func (e asUserError) Unwrap() error { return e.userErr }

func fallback(a action, err error) string {
	if explained(err) {
		return err.Error()
	}
	if a == actionEdit {
		return fmt.Sprintf("cc-slack can't edit this message (%s); only the bot can edit its own posts", errorCode(err))
	}
	if a == actionDelete {
		return fmt.Sprintf("cc-slack can't delete this message (%s); only the bot can delete its own posts", errorCode(err))
	}
	if a == actionChannel {
		return fmt.Sprintf("cc-slack can't change this channel (%s), so ask the user to do it in Slack", errorCode(err))
	}
	if a == actionFile {
		return fmt.Sprintf("cc-slack can't download this file (%s); the bot reads only files shared where it is a member", errorCode(err))
	}
	if errors.Is(err, errNoIdentity) {
		return fmt.Sprintf("%v: run `cc-slack login`", err)
	}
	if errors.Is(err, daemon.ErrIncumbentNewer) || errors.Is(err, errStaleProcess) {
		return err.Error()
	}
	code := failureCode(err)
	switch a {
	case actionWrite:
		return fmt.Sprintf("cc-slack can't post here (%s)", code)
	case actionReact:
		return fmt.Sprintf("cc-slack can't react here (%s)", code)
	case actionUnreact:
		return fmt.Sprintf("cc-slack can't remove a reaction here (%s)", code)
	case actionWatch:
		return fmt.Sprintf("cc-slack can't watch this conversation (%s): the bot watches only conversations it is a member of, "+
			"so invite it there, or read it once with slack_thread or slack_history, which read it as you with [oauth] client_id set", code)
	case actionUsers:
		return fmt.Sprintf("cc-slack can't look people up (%s); a user id such as U0123ABCDE resolves through users.info, "+
			"an exact email through users.lookupByEmail and any other query through users.list", code)
	case actionChannels:
		return fmt.Sprintf("cc-slack can't list channels (%s)", code)
	default:
		return fmt.Sprintf("cc-slack can't read this conversation (%s)", code)
	}
}

func explained(err error) bool {
	return errors.As(err, new(inviteNeededError)) || errors.As(err, new(outsideDMError)) || errors.As(err, new(sharedError)) ||
		errors.As(err, new(blockLimitError)) || errors.As(err, new(userConsentError)) || errors.As(err, new(asUserError))
}

func failureCode(err error) string {
	code := errorCode(err)
	if call, ok := errors.AsType[slackCallError](err); ok {
		code = "Slack's " + call.method + " answered " + errorCode(call.err)
	}
	return code + slackSaid(err)
}

func slackSaid(err error) string {
	if resp, ok := errors.AsType[slack.SlackErrorResponse](err); ok && len(resp.ResponseMetadata.Messages) > 0 {
		return ": " + strings.Join(resp.ResponseMetadata.Messages, "; ")
	}
	return ""
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
		if rate, limited := errors.AsType[*slack.RateLimitedError](err); limited {
			return fmt.Sprintf("cc-slack can't read thread %s now (%s): Slack limits the read for %ds more, so read the thread again after that", thread, code, int(math.Ceil(rate.RetryAfter.Seconds())))
		}
	}
	return fallback(actionRead, err)
}
