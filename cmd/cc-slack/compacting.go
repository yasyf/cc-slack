package main

import (
	"slices"

	"github.com/slack-go/slack"
	"github.com/spf13/cobra"

	"github.com/yasyf/cc-interact/agent"
	"github.com/yasyf/cc-interact/cmd"
	"github.com/yasyf/cc-interact/daemon"
)

const opPreCompact daemon.Op = "pre-compact"

const (
	compactionAck          = "Got it, back in a minute."
	compactionAckEventType = "cc_slack_compaction_ack"
)

type preCompactResult struct {
	Compacting string `json:"compacting"`
}

func (p *poller) startCompacting(subjectID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.compact[subjectID] = true
}

func (p *poller) endCompacting(subjectID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	was := p.compact[subjectID]
	delete(p.compact, subjectID)
	return was
}

func (p *poller) compacting(watches []watch) []watch {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.DeleteFunc(slices.Clone(watches), func(w watch) bool { return w.Owner != agent.TopLevel || !p.compact[w.SubjectID] })
}

func compactionAckPost(m slack.Message) bool {
	return m.Metadata.EventType == compactionAckEventType
}

func (a *app) handlePreCompact(hc daemon.HandlerCtx) daemon.Reply {
	sub, found, err := hc.Subjects.Find(hc.Ctx, hc.Window, hc.Scope)
	if err != nil {
		return fail(err.Error())
	}
	if !found {
		return ok(preCompactResult{})
	}
	a.poller.startCompacting(sub.ID)
	a.poller.log.Printf("session %s is compacting: its operator's DM messages get an ack until it starts again", hc.Env.Session)
	return ok(preCompactResult{Compacting: sub.ID})
}

func preCompactCmd(d cmd.Deps) *cobra.Command {
	return &cobra.Command{
		Use:    "pre-compact",
		Hidden: true,
		Short:  "PreCompact hook: until this window starts again, the daemon acks each operator DM message its watches receive",
		Args:   cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			in, err := readHookInput(c)
			if err != nil {
				return err
			}
			_, err = invoke(c.Context(), d, in.SessionID, cwdOr(in.Cwd), opPreCompact, nil)
			return err
		},
	}
}
