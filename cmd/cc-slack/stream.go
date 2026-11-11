package main

import (
	"cmp"
	"context"
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

const (
	pickupReaction = "eyes"
	workingStatus  = "is working…"
	timelineMode   = "timeline"
	taskInProgress = "in_progress"
)

var (
	pickupLoading  = []string{"Reading the thread…", "Looking at this…"}
	arrivalLoading = []string{"Picked up…"}
)

type pickupResult struct {
	OK        bool   `json:"ok"`
	LineError string `json:"line_error,omitempty"`
}

type streamResult struct {
	postResult
	LineError string `json:"line_error,omitempty"`
}

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

func inProgress(tasks []task) []string {
	var titles []string
	for _, t := range tasks {
		if t.Status == taskInProgress {
			titles = append(titles, t.Title+"…")
		}
	}
	return titles
}

func streamWords(plan string, tasks []task) string {
	words := make([]string, 0, 2*len(tasks)+1)
	words = append(words, plan)
	for _, t := range tasks {
		words = append(words, t.Title, t.Details)
	}
	return strings.TrimSpace(strings.Join(words, "\n"))
}

func setWorkingLine(ctx context.Context, api slackAPI, id identity, channelID, threadTS, status string, loading []string) string {
	params := slack.AssistantThreadsSetStatusParameters{ChannelID: channelID, ThreadTS: threadTS, Status: status, LoadingMessages: loading, Username: id.username(), IconURL: id.AvatarURL}
	if err := api.SetAssistantThreadsStatusContext(ctx, params); err != nil {
		return err.Error()
	}
	return ""
}

func (a *app) handlePickup(hc daemon.HandlerCtx) daemon.Reply {
	b, err := decodeBody(hc)
	if err != nil {
		return fail(err.Error())
	}
	if b.ChannelID == "" || b.TS == "" {
		return fail("pickup needs channel_id and ts (or a Slack url)")
	}
	if err := checkAgent(b.Agent); err != nil {
		return fail(err.Error())
	}
	id, err := loadIdentity(hc.Ctx, hc.DB)
	if err != nil {
		return fail(fallback(actionReact, err))
	}
	id.Agent = b.Agent
	api := a.newAPI(id.Token)
	channelID, err := conversationID(hc.Ctx, api, id, b.ChannelID)
	if err != nil {
		return fail(fallback(actionReact, err))
	}
	act, settled, apply, status, loading := actionReact, "already_reacted", slackAPI.AddReactionContext, workingStatus, pickupLoading
	if b.State != "" {
		loading = []string{b.State}
	}
	if b.Done {
		act, settled, apply, status, loading = actionUnreact, "no_reaction", slackAPI.RemoveReactionContext, "", nil
	}
	err = a.joinAndRetry(hc.Ctx, api, channelID, func() error {
		return apply(api, hc.Ctx, pickupReaction, slack.ItemRef{Channel: channelID, Timestamp: b.TS})
	})
	if err != nil && errorCode(err) != settled {
		return fail(fallback(act, err))
	}
	thread, err := messageThreadOf(hc.Ctx, api, channelID, b)
	if err != nil {
		return fail(fmt.Sprintf("the working line is unchanged: cc-slack could not %v", err))
	}
	thread = cmp.Or(thread, b.TS)
	line := setWorkingLine(hc.Ctx, api, id, channelID, thread, status, loading)
	if line != "" && !b.Done {
		return fail(fmt.Sprintf("the eyes are on %s, but Slack refused the working line under thread %s: %s", b.TS, thread, line))
	}
	return ok(pickupResult{OK: true, LineError: line})
}

func (a *app) startStream(hc daemon.HandlerCtx, api slackAPI, id identity, channelID string, b opBody, chunks []slack.MsgOption) daemon.Reply {
	options := append(chunks, slack.MsgOptionTS(b.ThreadTS), slack.MsgOptionTaskDisplayMode(slack.TaskDisplayMode(timelineMode)), slack.MsgOptionUsername(id.username()), slack.MsgOptionIconURL(id.AvatarURL))
	var ticket hookTicket
	if channelID != id.DMChannelID {
		if b.Recipient == "" {
			return fail("stream needs recipient outside your DM with the app: the user id of the person the work is for, which Slack requires in a channel")
		}
		options = append(options, slack.MsgOptionRecipientTeamID(id.TeamID), slack.MsgOptionRecipientUserID(b.Recipient))
		w := write{action: approvePost, named: b.ChannelID, channelID: channelID, threadTS: b.ThreadTS, text: streamWords(b.Plan, b.Tasks)}
		spent, replay, err := a.authorize(hc, api, id, b.ticketSpend, w)
		if err != nil {
			return fail(err.Error())
		}
		if replay != nil {
			res := replay.result(true)
			res.ThreadTS = b.ThreadTS
			return ok(streamResult{postResult: res})
		}
		if err := a.claimReceipt(hc.Ctx, hc.DB, spent, w); err != nil {
			return fail(err.Error())
		}
		ticket = spent
	}
	var ts string
	err := a.writeAndRetry(hc.Ctx, api, id, channelID, func() (err error) {
		_, ts, err = api.StartStreamContext(hc.Ctx, channelID, options...)
		return err
	})
	if err != nil {
		return fail(a.slackFailed(hc, id, b.ticketSpend, ticket, actionWrite, err))
	}
	a.wrote(hc.Ctx, channelID, b.ThreadTS)
	if ticket.once() {
		if err := a.saveReceipt(hc.Ctx, hc.DB, receipt{TicketID: ticket.ID, ChannelID: channelID, TS: ts}); err != nil {
			return fail(fmt.Sprintf("started stream %s but could not record its receipt, so do not start it again: %v", ts, err))
		}
	}
	return ok(streamResult{
		postResult: postResult{ChannelID: channelID, TS: ts, ThreadTS: b.ThreadTS, TicketID: ticket.ID, Posted: true},
		LineError:  setWorkingLine(hc.Ctx, api, id, channelID, b.ThreadTS, workingStatus, inProgress(b.Tasks)),
	})
}

func (a *app) handleStream(hc daemon.HandlerCtx) daemon.Reply {
	b, err := decodeBody(hc)
	if err != nil {
		return fail(err.Error())
	}
	if b.ChannelID == "" || b.ThreadTS == "" {
		return fail("stream needs channel_id and thread_ts, plus ts to continue or stop a stream")
	}
	if err := checkAgent(b.Agent); err != nil {
		return fail(err.Error())
	}
	id, err := loadIdentity(hc.Ctx, hc.DB)
	if err != nil {
		return fail(err.Error())
	}
	id.Agent = b.Agent
	api := a.newAPI(id.Token)
	channelID, err := conversationID(hc.Ctx, api, id, b.ChannelID)
	if err != nil {
		return fail(err.Error())
	}
	var chunks []slack.MsgOption
	if changed := streamChunks(b.Plan, b.Tasks); len(changed) > 0 {
		chunks = append(chunks, slack.MsgOptionChunks(changed...))
	}
	if b.TS == "" {
		return a.startStream(hc, api, id, channelID, b, chunks)
	}
	res := streamResult{postResult: postResult{ChannelID: channelID, TS: b.TS, ThreadTS: b.ThreadTS}}
	if b.Stop {
		if _, _, err := api.StopStreamContext(hc.Ctx, channelID, b.TS, chunks...); err != nil {
			return fail(err.Error())
		}
		res.LineError = setWorkingLine(hc.Ctx, api, id, channelID, b.ThreadTS, "", nil)
		return ok(res)
	}
	if _, _, err := api.AppendStreamContext(hc.Ctx, channelID, b.TS, chunks...); err != nil {
		return fail(err.Error())
	}
	res.LineError = setWorkingLine(hc.Ctx, api, id, channelID, b.ThreadTS, workingStatus, inProgress(b.Tasks))
	return ok(res)
}

func pickupCmd(d cmd.Deps) *cobra.Command {
	var (
		f         messageFlags
		done      bool
		state     string
		agentName string
	)
	c := &cobra.Command{
		Use:   "pickup",
		Short: "Add eyes to a message and show Slack's working line under its thread; --done takes both away",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			body := f.body()
			body["done"] = done
			body["state"] = state
			body["agent"] = agentName
			return runOp(c, d, f.sessionFlags, opPickup, body)
		},
	}
	f.bind(c)
	c.Flags().StringVar(&f.ts, "ts", "", "the message you are looking at")
	c.Flags().BoolVar(&done, "done", false, "take the eyes and the working line away")
	c.Flags().StringVar(&state, "state", "", "what the working line says you are doing now, such as \"Waiting for a subagent…\"")
	bindAgent(c, &agentName)
	return c
}

func streamCmd(d cmd.Deps) *cobra.Command {
	var (
		f         messageFlags
		recipient string
		plan      string
		taskFlags []string
		stop      bool
		agentName string
		spend     ticketSpend
	)
	c := &cobra.Command{
		Use:   "stream",
		Short: "Start, update, or stop the separate working steps in a thread, keeping the working line up",
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
			body["recipient"] = recipient
			body["plan"] = plan
			body["tasks"] = tasks
			body["stop"] = stop
			body["agent"] = agentName
			spend.fill(body)
			return runOp(c, d, f.sessionFlags, opStream, body)
		},
	}
	f.bind(c)
	c.Flags().StringVar(&f.ts, "ts", "", "the stream's message ts, to update or stop it; omit it to start one in --thread")
	c.Flags().StringVar(&recipient, "recipient", "", "outside your DM with the app, the user id of the person the work is for")
	c.Flags().StringVar(&plan, "plan", "", "the title above the steps")
	c.Flags().StringArrayVar(&taskFlags, "task", nil, "a task as id|status|title or id|status|title|details, with status pending, in_progress, complete, or error (repeatable)")
	c.Flags().BoolVar(&stop, "stop", false, "end the stream")
	bindAgent(c, &agentName)
	bindTicket(c, &spend)
	return c
}
