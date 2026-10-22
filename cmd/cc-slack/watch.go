package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
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
	caughtUpEvent = "event: caught-up"
	provenEvent   = `{"type":"channel.proven"}`
	provenPoll    = 2 * time.Second
)

var watchedTypes = map[string]bool{
	eventMessage: true, eventAction: true, eventWatchError: true, eventOwnerStopped: true, agent.EventRelay: true,
}

func channelEvent(typ string) bool { return watchedTypes[typ] || typ == daemon.ProbeEventType }

func watchCmd(d cmd.Deps) *cobra.Command {
	var (
		f           sessionFlags
		once        bool
		lifecycle   bool
		untilProven bool
	)
	c := &cobra.Command{
		Use:   "watch",
		Short: "Stream this session's Slack events as line-delimited JSON (one event per line)",
		Long: "watch prints one JSON event per line for each slack.message, slack.watch_error, and\n" +
			"agent.relay the session receives after watch starts. It is meant to run under a Claude\n" +
			"Code Monitor, so it never replays earlier events and leaves agent lifecycle events out;\n" +
			"--lifecycle prints every event for debugging. With --once it exits after the first\n" +
			"printed event. With --until-proven it prints " + provenEvent + " and exits once this\n" +
			"window has run channel-ack, so channel tags carry the session from then on.",
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
			head, err := streamHead(ctx, port, subjectID)
			if err != nil {
				return err
			}
			if err := consume.SeedCursor(d.Paths, subjectID, watchConsumer, consumer); err != nil {
				return err
			}
			src := consume.StreamSource{
				Port: port, SubjectID: subjectID, Consumer: consumer, ClaudePID: d.ClaudePID(),
				ExcludeOrigin: event.OriginAgent,
				Paths:         d.Paths, WindowAlive: d.WindowAlive,
				Refresh: func(ctx context.Context) (int, error) {
					_, port, err := resolveSubjectPort(ctx, d, f.session, scope, consumer)
					return port, err
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
			err = consume.ConsumeEvents(streamCtx, src, watchHandler(c.OutOrStdout(), head, lifecycle, once, d.TerminalEvent))
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

func watchHandler(out io.Writer, head int64, lifecycle, once bool, terminal func(string) bool) consume.EventHandler {
	return func(seq int64, data string) (bool, error) {
		typ := eventType(data)
		if seq <= head || (!lifecycle && !watchedTypes[typ]) {
			return false, nil
		}
		if _, err := fmt.Fprintln(out, data); err != nil {
			return false, err
		}
		return once || terminal(typ), nil
	}
}

func eventType(data string) string {
	var e struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal([]byte(data), &e)
	return e.Type
}

func streamHead(ctx context.Context, port int, subjectID string) (int64, error) {
	u := fmt.Sprintf("http://127.0.0.1:%d/events?session=%s&exclude_origin=%s",
		port, url.QueryEscape(subjectID), url.QueryEscape(event.OriginAgent))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, http.NoBody)
	if err != nil {
		return 0, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("read the event log head of %s: %w", subjectID, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("read the event log head of %s: events endpoint returned %d", subjectID, resp.StatusCode)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	caughtUp := false
	for sc.Scan() {
		line := sc.Text()
		if line == caughtUpEvent {
			caughtUp = true
			continue
		}
		if data, ok := strings.CutPrefix(line, "data: "); ok && caughtUp {
			var marker struct {
				Seq int64 `json:"seq"`
			}
			if err := json.Unmarshal([]byte(data), &marker); err != nil {
				return 0, fmt.Errorf("parse the caught-up marker of %s: %w", subjectID, err)
			}
			return marker.Seq, nil
		}
	}
	if err := sc.Err(); err != nil {
		return 0, fmt.Errorf("event stream of %s closed before it caught up: %w", subjectID, err)
	}
	return 0, fmt.Errorf("event stream of %s closed before it caught up", subjectID)
}
