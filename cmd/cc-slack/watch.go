package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-interact/agent"
	"github.com/yasyf/cc-interact/cmd"
	"github.com/yasyf/cc-interact/consume"
	"github.com/yasyf/cc-interact/daemon"
	"github.com/yasyf/cc-interact/event"
)

const (
	watchConsumer = "watch"
	provenEvent   = `{"type":"channel.proven"}`
	provenPoll    = 2 * time.Second
	monitorEvent  = 500
	cutField      = "cut"
)

var cutFields = map[string]string{eventMessage: "text", eventAction: "label", eventWatchError: "error", eventOwnerStopped: "watches"}

var watchedTypes = map[string]bool{
	eventMessage: true, eventStreamed: true, eventAction: true, eventWatchError: true, eventOwnerStopped: true, agent.EventRelay: true,
}

func channelEvent(typ string) bool {
	return (watchedTypes[typ] && typ != eventStreamed) || typ == daemon.ProbeEventType
}

func watchCmd(d cmd.Deps) *cobra.Command {
	var (
		f            sessionFlags
		once         bool
		lifecycle    bool
		untilProven  bool
		onlyDelivery bool
		whole        bool
	)
	c := &cobra.Command{
		Use:   "watch",
		Short: "Stream this session's Slack events as line-delimited JSON (one event per line)",
		Long: "watch prints one JSON event per line for each slack.message, slack.watch_error, and\n" +
			"agent.relay the session receives after watch starts. It is meant to run under a Claude\n" +
			"Code Monitor, so it never replays earlier events and leaves agent lifecycle events out;\n" +
			"--lifecycle prints every event for debugging. With --once it exits after the first\n" +
			"printed event. With --until-proven it prints " + provenEvent + " and exits once this\n" +
			"window has run channel-ack, so channel tags carry the session from then on. With\n" +
			"--only-delivery this stream alone carries the session's Slack messages while it runs:\n" +
			"no channel tag and no directive for them, until the process exits.\n\n" +
			"A Monitor event holds 500 characters, so a longer line drops a card's capture and options,\n" +
			"then its question and the thread owner's title, then the permalink, and last cuts the\n" +
			"line's own words and sets cut. A tap's id is never cut. --whole prints every line as the\n" +
			"daemon sent it, for a program reading the stream rather than a Monitor.",
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			ctx := c.Context()
			scope := cwdOr(f.cwd)
			consumer := fmt.Sprintf("%s-%d", watchConsumer, os.Getpid())
			var (
				subjectID string
				port      int
			)
			if err := acrossRestart(awaitRestart, func(ctx context.Context) error {
				if err := d.EnsureCurrent(ctx); err != nil {
					return err
				}
				var err error
				subjectID, port, err = resolveSubjectPort(ctx, d, f.session, scope, consumer)
				return err
			})(ctx); err != nil {
				return err
			}
			head, err := consume.StreamHead(ctx, port, subjectID, event.OriginAgent)
			if err != nil {
				return err
			}
			if err := consume.SeedCursor(d.Paths, subjectID, watchConsumer, consumer); err != nil {
				return err
			}
			holdDelivery := func() error {
				if !onlyDelivery {
					return nil
				}
				_, err := callOp(c, d, f, opHoldDelivery, map[string]any{"stream_pid": os.Getpid()})
				return err
			}
			if err := holdDelivery(); err != nil {
				return err
			}
			src := consume.StreamSource{
				Port: port, SubjectID: subjectID, Consumer: consumer, ClaudePID: d.ClaudePID(),
				ExcludeOrigin: event.OriginAgent,
				Paths:         d.Paths, WindowAlive: d.WindowAlive,
				Refresh: func(ctx context.Context) (int, error) {
					_, port, err := resolveSubjectPort(ctx, d, f.session, scope, consumer)
					if err != nil {
						return port, err
					}
					return port, holdDelivery()
				},
			}
			streamCtx, stop := context.WithCancel(ctx)
			defer stop()
			var proven atomic.Bool
			if untilProven {
				go func() {
					if waitProven(streamCtx, d, f.session, scope) {
						proven.Store(true)
						stop()
					}
				}()
			}
			err = consume.ConsumeEvents(streamCtx, src, watchHandler(c.OutOrStdout(), head, lifecycle, once, whole, d.TerminalEvent))
			if proven.Load() {
				_, err = fmt.Fprintln(c.OutOrStdout(), provenEvent)
			}
			return err
		},
	}
	f.bind(c)
	c.Flags().BoolVar(&once, "once", false, "exit after the first printed event")
	c.Flags().BoolVar(&lifecycle, "lifecycle", false, "also print agent lifecycle events, for debugging")
	c.Flags().BoolVar(&untilProven, "until-proven", false, "exit once this window's channel delivery is proven")
	c.Flags().BoolVar(&onlyDelivery, "only-delivery", false, "while this runs, deliver the session's Slack messages here alone, with no channel tag or directive")
	c.Flags().BoolVar(&whole, "whole", false, "print every line whole, past the 500 characters a Monitor event holds")
	return c
}

func waitProven(ctx context.Context, d cmd.Deps, session, scope string) bool {
	t := time.NewTicker(provenPoll)
	defer t.Stop()
	for !channelProven(ctx, d, session, scope) {
		select {
		case <-ctx.Done():
			return false
		case <-t.C:
		}
	}
	return true
}

func channelProven(ctx context.Context, d cmd.Deps, session, scope string) bool {
	client, err := d.NewClient(ctx)
	if err != nil {
		return false
	}
	defer func() { _ = client.Close() }()
	reply, err := client.Do(ctx, daemon.Envelope{Op: daemon.OpStatus, Session: session, ClaudePID: d.ClaudePID(), Scope: scope})
	if err != nil || !reply.OK {
		return false
	}
	var body daemon.StatusBody
	return json.Unmarshal(reply.Body, &body) == nil && body.Proven
}

func watchHandler(out io.Writer, head int64, lifecycle, once, whole bool, terminal func(string) bool) consume.EventHandler {
	return func(seq int64, data string) (bool, error) {
		typ := eventType(data)
		if seq <= head || (!lifecycle && !watchedTypes[typ]) {
			return false, nil
		}
		if typ == eventStreamed {
			data = strings.Replace(data, `"type":"`+eventStreamed+`"`, `"type":"`+eventMessage+`"`, 1)
		}
		if !whole {
			fit, err := fitted(data)
			if err != nil {
				return false, err
			}
			data = fit
		}
		if _, err := fmt.Fprintln(out, data); err != nil {
			return false, err
		}
		return once || terminal(typ), nil
	}
}

func fitted(data string) (string, error) {
	if len(data) <= monitorEvent {
		return data, nil
	}
	var line map[string]any
	if err := json.Unmarshal([]byte(data), &line); err != nil {
		return "", fmt.Errorf("fit watch line %q: %w", data, err)
	}
	var encoded []byte
	for _, drop := range []func(map[string]any){withoutCardPicks, withoutCardWords, withoutPermalink} {
		drop(line)
		var err error
		if encoded, err = json.Marshal(line); err != nil || len(encoded) <= monitorEvent {
			return string(encoded), err
		}
	}
	typ, _ := line["type"].(string)
	field, cuttable := cutFields[typ]
	if !cuttable {
		return string(encoded), nil
	}
	line[cutField] = true
	switch words := line[field].(type) {
	case []any:
		return withoutLastWatches(line, field, words)
	case string:
		said := []rune(words)
		line[field] = string(said[:max(0, len(said)-(len(encoded)+len(`,"cut":true`)-monitorEvent))])
		encoded, err := json.Marshal(line)
		return string(encoded), err
	default:
		return "", fmt.Errorf("fit watch line %q: %s is %T", data, field, words)
	}
}

func withoutLastWatches(line map[string]any, field string, watches []any) (string, error) {
	for {
		encoded, err := json.Marshal(line)
		if err != nil || len(encoded) <= monitorEvent || len(watches) == 0 {
			return string(encoded), err
		}
		watches = watches[:len(watches)-1]
		line[field] = watches
	}
}

func withoutCardPicks(line map[string]any) {
	delete(line, "capture")
	delete(line, "options")
}

func withoutCardWords(line map[string]any) {
	delete(line, "question")
	if owner, ok := line["thread_owner"].(map[string]any); ok {
		delete(owner, "title")
	}
}

func withoutPermalink(line map[string]any) {
	delete(line, "permalink")
}

func eventType(data string) string {
	var e struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal([]byte(data), &e)
	return e.Type
}
