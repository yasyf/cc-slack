package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/slack-go/slack"
	"github.com/spf13/cobra"
	"github.com/yasyf/cc-interact/cmd"
	"github.com/yasyf/cc-interact/daemon"
)

type task struct {
	ID      string `json:"id"`
	Status  string `json:"status"`
	Title   string `json:"title"`
	Details string `json:"details,omitempty"`
}

var errOutsideDM = errors.New("this verb works only in your DM with the app, since the permission hook checks no status or stream elsewhere")

func parseTask(flag string) (task, error) {
	parts := strings.SplitN(flag, "|", 4)
	if len(parts) < 3 {
		return task{}, fmt.Errorf("task %q needs id|status|title or id|status|title|details", flag)
	}
	t := task{ID: parts[0], Status: parts[1], Title: parts[2]}
	if len(parts) == 4 {
		t.Details = parts[3]
	}
	return t, nil
}

func streamChunks(plan string, tasks []task) []slack.StreamChunk {
	var chunks []slack.StreamChunk
	for _, t := range tasks {
		chunk := slack.NewTaskUpdateChunk(t.ID, t.Title)
		chunk.Status = slack.TaskCardStatus(t.Status)
		chunk.Details = t.Details
		chunks = append(chunks, chunk)
	}
	if plan != "" {
		chunks = append(chunks, slack.NewPlanUpdateChunk(plan))
	}
	return chunks
}

func (a *app) handleThreadStatus(hc daemon.HandlerCtx) daemon.Reply {
	b, err := decodeBody(hc)
	if err != nil {
		return fail(err.Error())
	}
	if b.ChannelID == "" || b.ThreadTS == "" {
		return fail("thread-status needs channel_id and thread_ts (or a Slack url); an empty status clears the line")
	}
	id, err := loadIdentity(hc.Ctx, hc.DB)
	if err != nil {
		return fail(err.Error())
	}
	api := a.newAPI(id.Token)
	channelID, err := conversationID(hc.Ctx, api, id, b.ChannelID)
	if err != nil {
		return fail(err.Error())
	}
	if channelID != id.DMChannelID {
		return fail(errOutsideDM.Error())
	}
	id.Agent = b.Agent
	params := slack.AssistantThreadsSetStatusParameters{ChannelID: channelID, ThreadTS: b.ThreadTS, Status: b.Status, LoadingMessages: b.Loading, Username: id.username(), IconURL: id.AvatarURL}
	if err := api.SetAssistantThreadsStatusContext(hc.Ctx, params); err != nil {
		return fail(err.Error())
	}
	return ok(map[string]bool{"ok": true})
}

func (a *app) handleStream(hc daemon.HandlerCtx) daemon.Reply {
	b, err := decodeBody(hc)
	if err != nil {
		return fail(err.Error())
	}
	if b.ChannelID == "" || (b.TS == "" && b.ThreadTS == "") {
		return fail("stream needs channel_id and thread_ts to start, or channel_id and ts to continue")
	}
	id, err := loadIdentity(hc.Ctx, hc.DB)
	if err != nil {
		return fail(err.Error())
	}
	api := a.newAPI(id.Token)
	channelID, err := conversationID(hc.Ctx, api, id, b.ChannelID)
	if err != nil {
		return fail(err.Error())
	}
	if channelID != id.DMChannelID {
		return fail(errOutsideDM.Error())
	}
	id.Agent = b.Agent
	chunks := slack.MsgOptionChunks(streamChunks(b.Plan, b.Tasks)...)
	ts := b.TS
	switch {
	case b.TS == "":
		_, ts, err = api.StartStreamContext(hc.Ctx, channelID, chunks, slack.MsgOptionTS(b.ThreadTS), slack.MsgOptionTaskDisplayMode(slack.TaskDisplayMode(b.Mode)), slack.MsgOptionUsername(id.username()), slack.MsgOptionIconURL(id.AvatarURL))
	case b.Stop:
		_, _, err = api.StopStreamContext(hc.Ctx, channelID, b.TS, chunks)
	default:
		_, _, err = api.AppendStreamContext(hc.Ctx, channelID, b.TS, chunks)
	}
	if err != nil {
		return fail(err.Error())
	}
	return ok(postResult{ChannelID: channelID, TS: ts})
}

func threadStatusCmd(d cmd.Deps) *cobra.Command {
	var (
		f         messageFlags
		status    string
		loading   []string
		agentName string
	)
	c := &cobra.Command{
		Use:   "thread-status",
		Short: "Show or clear Slack's working line under a thread in your DM with the app",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			body := f.body()
			body["status"] = status
			body["loading"] = loading
			body["agent"] = agentName
			return runOp(c, d, f.sessionFlags, opThreadStatus, body)
		},
	}
	f.bind(c)
	c.Flags().StringVar(&status, "status", "", "words after the app's name, such as \"is reading the logs…\"; empty clears the line")
	c.Flags().StringArrayVar(&loading, "loading", nil, "a message the line rotates through (repeatable)")
	bindAgent(c, &agentName)
	return c
}

func streamCmd(d cmd.Deps) *cobra.Command {
	var (
		f         messageFlags
		mode      string
		plan      string
		taskFlags []string
		stop      bool
		agentName string
	)
	c := &cobra.Command{
		Use:   "stream",
		Short: "Start, update, or stop a streamed task list in a thread of your DM with the app",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			tasks := make([]task, 0, len(taskFlags))
			for _, flag := range taskFlags {
				t, err := parseTask(flag)
				if err != nil {
					return err
				}
				tasks = append(tasks, t)
			}
			body := f.body()
			body["mode"] = mode
			body["plan"] = plan
			body["tasks"] = tasks
			body["stop"] = stop
			body["agent"] = agentName
			return runOp(c, d, f.sessionFlags, opStream, body)
		},
	}
	f.bind(c)
	c.Flags().StringVar(&f.ts, "ts", "", "the stream's message ts, to update or stop it; omit it to start one in --thread")
	c.Flags().StringVar(&mode, "mode", "plan", "how a started stream draws its tasks: plan or timeline")
	c.Flags().StringVar(&plan, "plan", "", "the plan's title")
	c.Flags().StringArrayVar(&taskFlags, "task", nil, "a task as id|status|title or id|status|title|details, with status pending, in_progress, complete, or error (repeatable)")
	c.Flags().BoolVar(&stop, "stop", false, "end the stream")
	bindAgent(c, &agentName)
	return c
}
