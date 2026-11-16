package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-interact/channel"
	"github.com/yasyf/cc-interact/channelsetup"
	"github.com/yasyf/cc-interact/cmd"
	"github.com/yasyf/cc-interact/daemon"
	"github.com/yasyf/cc-interact/procs"
)

var channelPlugin = channelsetup.Plugin{Marketplace: "cc-slack", Name: "cc-slack"}

func claudePID() int {
	if os.Getenv("LONG_RUNNING_DETACHED") != "" {
		return 0
	}
	return procs.ClaudePID()
}

func deps() cmd.Deps {
	d := cmd.Deps{
		Paths:                  appPaths(),
		Version:                appVersion,
		Plugin:                 channelPlugin,
		NewClient:              newClient,
		EnsureCurrent:          nameReconnect(keepIncumbent(requireCurrentState(refuseForeignRollback(func(ctx context.Context) error { return launcher().EnsureCurrent(ctx) })))),
		EnsureCurrentIfRunning: nameReconnect(keepIncumbent(requireCurrentState(refuseForeignRollback(func(ctx context.Context) error { return launcher().EnsureCurrentIfRunning(ctx) })))),
		Stop:                   func(ctx context.Context) error { return launcher().Stop(ctx) },
		ClaudePID:              claudePID,
		WindowAlive:            procs.LiveClaude,
		TerminalEvent:          func(string) bool { return false },
		ChannelEvent:           channelEvent,
		ChannelResumesLive:     true,
		Serve:                  serve,
	}
	d.ChannelTools = func(_ context.Context, session, scope string) ([]channel.Tool, string, string, error) {
		return channelTools(d, session, scope)
	}
	return d
}

func nameReconnect(ensure func(context.Context) error) func(context.Context) error {
	return func(ctx context.Context) error {
		err := ensure(ctx)
		if errors.Is(err, daemon.ErrIncumbentNewer) {
			return fmt.Errorf("%s: %w; this session's %s process is the older build, so run /mcp and reconnect %s, or start a new session, to pick up the current build", binaryName, err, binaryName, binaryName)
		}
		return err
	}
}

func keepIncumbent(ensure func(context.Context) error) func(context.Context) error {
	return func(ctx context.Context) error {
		if err := ensure(ctx); !errors.Is(err, daemon.ErrUpgradeDeferred) {
			return err
		}
		return nil
	}
}

func root() *cobra.Command {
	d := deps()
	r := &cobra.Command{
		Use:           binaryName,
		Short:         "Slack for Claude Code sessions: post as <display name> (<agent>) and watch threads",
		Version:       appVersion,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	r.AddCommand(
		cmd.DaemonCmd(d),
		cmd.StopCmd(d),
		cmd.StatusCmd(d),
		watchCmd(d),
		cmd.ChannelCmd(d),
		cmd.ToolsCmd(d),
		cmd.ChannelCheckCmd(d),
		cmd.ChannelAckCmd(d),
		cmd.SessionRecordCmd(d),
		cmd.AgentStartCmd(d),
		cmd.AgentInjectCmd(d),
		cmd.AgentStopCmd(d),
		cmd.AgentReportCmd(d),
		cmd.SetupChannelsCmd(d, channelPlugin, "cc-slack is on the approved channel allowlist; restart Claude Code to load it with --channels "+channelPlugin.ChannelID()),
		sessionStartCmd(d),
		preCompactCmd(d),
		loginCmd(d),
		reloadCmd(d),
		whoamiCmd(d),
		postCmd(d, "send", "Post a Slack message as <display name> (<agent>)", opSend),
		postCmd(d, "reply", "Reply in a Slack thread as <display name> (<agent>)", opReply),
		reactionCmd(d, "react", "React to a Slack message", opReact),
		reactionCmd(d, "unreact", "Remove a reaction from a Slack message", opUnreact),
		pinCmd(d),
		pickupCmd(d),
		streamCmd(d),
		editCmd(d),
		deleteCmd(d),
		applyCmd(d),
		threadCmd(d),
		historyCmd(d),
		searchCmd(d),
		fileCmd(d),
		dmPeerCmd(d),
		channelInfoCmd(d),
		dmOwedCmd(d),
		operatorChannelCmd(d),
		driveCardsCmd(d),
		canvasCmd(d),
		redrawPRCardsCmd(d),
		drawPRLinesCmd(d),
		withdrawPRCardCmd(d),
		moveCardCmd(d),
		channelsCmd(d),
		usersCmd(d),
		followCmd(d),
		subscribeCmd(d),
		unfollowCmd(d),
		followsCmd(d),
		threadSeenCmd(d),
		dmStatusCmd(d),
		cardAnswerCmd(d),
		cardReopenCmd(d),
		dmRelayCmd(d),
		cardCmd(),
		checkPostCmd(),
		channelCmd(d, opChannelCreate, approveCreate, "Create a public Slack channel as the Claude app"),
		channelCmd(d, opChannelArchive, approveArchive, "Archive a Slack channel as the Claude app"),
		channelCmd(d, opChannelInvite, approveInvite, "Invite people to a Slack channel as the Claude app"),
		channelCmd(d, opChannelTopic, approveTopic, "Set a Slack channel's topic as the Claude app"),
		channelCmd(d, opChannelJoin, approveJoin, "Add the Claude app to a Slack channel, inviting it as you when the channel is private"),
		revokeCmd(d),
		invokeCmd(d),
		superviseCmd(),
	)
	return r
}

func main() {
	if len(os.Args) < 2 || (os.Args[1] != "daemon" && os.Args[1] != "--version") {
		if err := execResolvedExecutable(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if err := execIncumbent(context.Background()); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	if err := root().Execute(); err != nil {
		if status, exited := errors.AsType[exitStatus](err); exited {
			os.Exit(int(status))
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
