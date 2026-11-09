package main

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"github.com/yasyf/cc-interact/cmd"
	"github.com/yasyf/cc-interact/daemon"
)

type threadSeenResult struct {
	Newest    string `json:"newest"`
	Delivered bool   `json:"delivered"`
}

func (a *app) handleThreadSeen(hc daemon.HandlerCtx) daemon.Reply {
	b, err := decodeBody(hc)
	if err != nil {
		return fail(err.Error())
	}
	if b.ChannelID == "" || b.ThreadTS == "" {
		return fail("thread-seen needs channel_id and thread_ts")
	}
	newest := b.ThreadTS
	err = hc.DB.QueryRowContext(hc.Ctx, `SELECT oldest_ts FROM cursors WHERE channel_id = ? AND thread_ts = ?`, b.ChannelID, b.ThreadTS).Scan(&newest)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fail(fmt.Sprintf("read the thread's cursor: %v", err))
	}
	sub, found, err := hc.Subjects.Find(hc.Ctx, hc.Window, hc.Scope)
	if err != nil || !found {
		return ok(threadSeenResult{Newest: newest})
	}
	seen, err := delivered(hc.Ctx, hc.DB, sub.ID, messageDedup(watch{Owner: b.Owner, ChannelID: b.ChannelID}, newest))
	if err != nil {
		return fail(err.Error())
	}
	return ok(threadSeenResult{Newest: newest, Delivered: seen})
}

func threadSeenCmd(d cmd.Deps) *cobra.Command {
	var (
		f                      sessionFlags
		channel, thread, owner string
	)
	c := &cobra.Command{
		Use:    "thread-seen",
		Hidden: true,
		Short:  "Print the newest message the daemon has read in a thread and whether it reached this session's agent",
		Args:   cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return runOp(c, d, f, opThreadSeen, map[string]any{"channel_id": channel, "thread_ts": thread, "owner": owner})
		},
	}
	f.bind(c)
	c.Flags().StringVar(&channel, "channel", "", "the thread's channel id")
	c.Flags().StringVar(&thread, "thread", "", "the thread's root ts")
	c.Flags().StringVar(&owner, "owner", "", "the agent id the message reached, empty for the top-level session")
	return c
}
