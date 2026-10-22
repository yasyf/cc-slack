package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/slack-go/slack"
	"github.com/yasyf/cc-interact/daemon"
)

func TestFallbackNamesTheMCPToolForEachAction(t *testing.T) {
	cases := []struct {
		name   string
		action action
		err    error
		want   []string
	}{
		{"write", actionWrite, slack.SlackErrorResponse{Err: "channel_not_found"}, []string{"can't post here (channel_not_found)", mcpSend, "still needs permission"}},
		{"react", actionReact, slack.SlackErrorResponse{Err: "not_in_channel"}, []string{"can't react here (not_in_channel)", mcpReaction}},
		{"unreact", actionUnreact, slack.SlackErrorResponse{Err: "not_in_channel"}, []string{"can't remove a reaction here (not_in_channel)", mcpUnreact}},
		{"invite", actionReact, inviteNeededError{channelID: "C0PRIVATE1"}, []string{"<#C0PRIVATE1>", "channel-join"}},
		{"read", actionRead, slack.SlackErrorResponse{Err: "missing_scope"}, []string{"can't read this conversation (missing_scope)", mcpThread, mcpHistory}},
		{"watch", actionWatch, slack.SlackErrorResponse{Err: "not_in_channel"}, []string{"can't watch", "member of", mcpThread, mcpHistory}},
		{"rate limit", actionWrite, &slack.RateLimitedError{RetryAfter: time.Second}, []string{"(ratelimited)", mcpSend}},
		{"daemon down", actionRead, errors.New("dial unix: no such file"), []string{"(dial unix: no such file)", mcpThread}},
		{"no identity", actionWrite, errNoIdentity, []string{"run `cc-slack login`", mcpSend}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := fallback(tc.action, tc.err)
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("fallback = %q, want it to contain %q", got, want)
				}
			}
		})
	}
}

func TestFallbackWrapsIdentityErrors(t *testing.T) {
	wrapped := errors.Join(errors.New("load"), errNoIdentity)
	if got := fallback(actionReact, wrapped); !strings.Contains(got, "cc-slack login") || !strings.Contains(got, mcpReaction) {
		t.Fatalf("fallback = %q", got)
	}
}

func TestNewerDaemonRefusalNamesTheReconnectStep(t *testing.T) {
	refusal := fmt.Errorf("%w: 0.1.1790354707 supersedes this build 0.1.1790307835", daemon.ErrIncumbentNewer)
	err := nameReconnect(func(context.Context) error { return refusal })(context.Background())
	if !errors.Is(err, daemon.ErrIncumbentNewer) {
		t.Fatalf("nameReconnect = %v, want it to wrap ErrIncumbentNewer", err)
	}
	for _, got := range []string{err.Error(), fallback(actionWrite, err)} {
		for _, want := range []string{"0.1.1790354707", "0.1.1790307835", "run /mcp and reconnect cc-slack", "start a new session"} {
			if !strings.Contains(got, want) {
				t.Errorf("%q does not contain %q", got, want)
			}
		}
	}
}
