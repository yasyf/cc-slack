package main

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/slack-go/slack"
	"github.com/spf13/cobra"

	"github.com/yasyf/cc-interact/cmd"
	"github.com/yasyf/cc-interact/daemon"
)

const (
	opApplyRead   daemon.Op = "apply-read"
	opApplyChange daemon.Op = "apply-change"

	verdictDelete  = "delete"
	verdictRewrite = "rewrite"
	verdictKeep    = "keep"

	outcomeOwn         = "own"
	outcomeDeleted     = "deleted"
	outcomeRewritten   = "rewritten"
	outcomeRefused     = "refused"
	outcomeGone        = "gone"
	outcomeRateLimited = "ratelimited"

	applyPace = 1500 * time.Millisecond
)

var (
	errMessageGone = errors.New("message_not_found")
	planVerdicts   = []string{verdictDelete, verdictRewrite, verdictKeep}
)

type applyResult struct {
	Outcome    string          `json:"outcome"`
	Reason     string          `json:"reason,omitempty"`
	RetryAfter int             `json:"retry_after,omitempty"`
	Message    json.RawMessage `json:"message,omitempty"`
}

func (a *app) handleApplyRead(hc daemon.HandlerCtx) daemon.Reply {
	b, err := decodeBody(hc)
	if err != nil {
		return fail(err.Error())
	}
	if b.ChannelID == "" || b.TS == "" {
		return fail("apply-read needs channel_id and ts")
	}
	id, err := loadIdentity(hc.Ctx, hc.DB)
	if err != nil {
		return fail(err.Error())
	}
	res, err := a.readOwn(withoutRateWait(hc.Ctx), a.newAPI(id.Token), id, b)
	if err != nil {
		return fail(fmt.Sprintf("read message %s: %v", b.TS, err))
	}
	return ok(res)
}

func (a *app) handleApplyChange(hc daemon.HandlerCtx) daemon.Reply {
	b, err := decodeBody(hc)
	if err != nil {
		return fail(err.Error())
	}
	if err := checkVerdict(b); err != nil {
		return fail(err.Error())
	}
	act := actionDelete
	if b.Verdict == verdictRewrite {
		act = actionEdit
	}
	id, err := loadIdentity(hc.Ctx, hc.DB)
	if err != nil {
		return fail(fallback(act, err))
	}
	ctx, api := withoutRateWait(hc.Ctx), a.newAPI(id.Token)
	own, err := a.checkOwn(ctx, api, id, b)
	if err != nil {
		return fail(fmt.Sprintf("read message %s: %v", b.TS, err))
	}
	if own.Outcome != outcomeOwn {
		return ok(own)
	}
	res, err := a.change(ctx, api, id, hc.Env.Scope, b)
	if err != nil {
		return fail(fallback(act, err))
	}
	return ok(res)
}

func checkVerdict(b opBody) error {
	switch {
	case b.ChannelID == "" || b.TS == "":
		return errors.New("apply-change needs channel_id and ts")
	case b.Verdict == verdictDelete && b.Text != "":
		return errors.New("a delete takes no text")
	case b.Verdict == verdictRewrite && b.Text == "":
		return errors.New("a rewrite needs text")
	case b.Verdict != verdictDelete && b.Verdict != verdictRewrite:
		return fmt.Errorf("apply-change takes verdict delete or rewrite, not %q", b.Verdict)
	}
	return nil
}

func ownedKey(channelID, ts string) string { return channelID + "/" + ts }

func (a *app) checkOwn(ctx context.Context, api slackAPI, id identity, b opBody) (applyResult, error) {
	if _, read := a.owned.Load(ownedKey(b.ChannelID, b.TS)); read && b.ChannelID == id.DMChannelID {
		return applyResult{Outcome: outcomeOwn}, nil
	}
	return a.readOwn(ctx, api, id, b)
}

func (a *app) readOwn(ctx context.Context, api slackAPI, id identity, b opBody) (applyResult, error) {
	if b.ChannelID != id.DMChannelID {
		return applyResult{Outcome: outcomeRefused, Reason: fmt.Sprintf("%s is not your DM with the bot, %s", b.ChannelID, id.DMChannelID)}, nil
	}
	raw, err := api.RawMessage(ctx, b.ChannelID, cmp.Or(b.ThreadTS, b.TS), b.TS)
	if res, settled := settledBySlack(err); settled {
		return res, nil
	}
	if err != nil {
		return applyResult{}, err
	}
	var m slack.Message
	if err := json.Unmarshal(raw, &m); err != nil {
		return applyResult{}, err
	}
	if m.SubType == "tombstone" {
		return applyResult{Outcome: outcomeGone, Reason: "only its thread's deleted-message placeholder is left"}, nil
	}
	if !id.botPosted(m) {
		return applyResult{Outcome: outcomeRefused, Reason: fmt.Sprintf("the bot did not post it; %s did", cmp.Or(m.User, m.BotID, m.Username))}, nil
	}
	a.owned.Store(ownedKey(b.ChannelID, b.TS), true)
	return applyResult{Outcome: outcomeOwn, Message: raw}, nil
}

func settledBySlack(err error) (applyResult, bool) {
	if err == nil {
		return applyResult{}, false
	}
	if rate, limited := errors.AsType[*slack.RateLimitedError](err); limited {
		return applyResult{Outcome: outcomeRateLimited, RetryAfter: int(math.Ceil(rate.RetryAfter.Seconds()))}, true
	}
	if code := errorCode(err); code == "message_not_found" || code == "thread_not_found" {
		return applyResult{Outcome: outcomeGone, Reason: code}, true
	}
	return applyResult{}, false
}

func (a *app) change(ctx context.Context, api slackAPI, id identity, scope string, b opBody) (applyResult, error) {
	err := a.changeMessage(ctx, api, id, scope, b)
	if res, settled := settledBySlack(err); settled {
		return res, nil
	}
	if err != nil {
		return applyResult{}, err
	}
	a.wrote(ctx, b.ChannelID, cmp.Or(b.ThreadTS, b.TS))
	if b.Verdict == verdictRewrite {
		return applyResult{Outcome: outcomeRewritten}, nil
	}
	a.owned.Delete(ownedKey(b.ChannelID, b.TS))
	if err := a.owed.forget(ctx, b.TS); err != nil {
		a.poller.log.Printf("%v", err)
	}
	return applyResult{Outcome: outcomeDeleted}, nil
}

func (a *app) changeMessage(ctx context.Context, api slackAPI, id identity, scope string, b opBody) error {
	if b.Verdict == verdictDelete {
		_, _, err := api.DeleteMessageContext(ctx, b.ChannelID, b.TS)
		return err
	}
	texts, _ := a.linkPRs(ctx, id, scope, withoutHidden(b.Text))
	return a.cards.editByHand(b.ChannelID, b.TS, func() error {
		return updateMessage(ctx, api, b.ChannelID, b.TS, texts[0], nil)
	})
}

type planEntry struct {
	TS        string `json:"ts"`
	ThreadTS  string `json:"thread_ts"`
	Permalink string `json:"permalink"`
	Verdict   string `json:"verdict"`
	NewText   string `json:"new_text"`
	channelID string
}

type planFiles struct {
	archive string
	done    string
}

func planFilesFor(plan string) planFiles {
	base := strings.TrimSuffix(plan, filepath.Ext(plan))
	return planFiles{archive: base + ".archive.jsonl", done: base + ".done.jsonl"}
}

func loadPlan(path string) ([]planEntry, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var entries []planEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, fmt.Errorf("plan %s is not a JSON list of entries: %w", path, err)
	}
	for i := range entries {
		e := &entries[i]
		ref, err := parsePermalink(e.Permalink)
		if err != nil {
			return nil, fmt.Errorf("plan entry %d: %w", i, err)
		}
		e.channelID = ref.channelID
		switch {
		case e.TS == "":
			return nil, fmt.Errorf("plan entry %d has no ts", i)
		case !slices.Contains(planVerdicts, e.Verdict):
			return nil, fmt.Errorf("plan entry %s: verdict %q is not delete, rewrite, or keep", e.TS, e.Verdict)
		case e.Verdict == verdictRewrite && e.NewText == "":
			return nil, fmt.Errorf("plan entry %s is a rewrite with no new_text", e.TS)
		case e.channelID != entries[0].channelID:
			return nil, fmt.Errorf("plan names two channels, %s and %s; a plan covers one DM", entries[0].channelID, e.channelID)
		}
	}
	return entries, nil
}

type doneEntry struct {
	TS      string `json:"ts"`
	Outcome string `json:"outcome"`
	Reason  string `json:"reason,omitempty"`
	At      string `json:"at"`
}

func readDone(path string) (map[string]bool, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]bool{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	done := map[string]bool{}
	lines := bufio.NewScanner(f)
	for lines.Scan() {
		var e doneEntry
		if err := json.Unmarshal(lines.Bytes(), &e); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		done[e.TS] = true
	}
	return done, lines.Err()
}

type archivedMessage struct {
	ChannelID string          `json:"channel_id"`
	Verdict   string          `json:"verdict"`
	NewText   string          `json:"new_text,omitempty"`
	Message   json.RawMessage `json:"message"`
}

type jsonLines struct{ f *os.File }

func appendJSONLines(path string) (*jsonLines, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	return &jsonLines{f}, nil
}

func (l *jsonLines) add(v any) error {
	line, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if _, err := l.f.Write(append(line, '\n')); err != nil {
		return err
	}
	return l.f.Sync()
}

type applyCall func(ctx context.Context, op daemon.Op, body map[string]any) (applyResult, error)

func applyVia(d cmd.Deps, session, scope string) applyCall {
	return func(ctx context.Context, op daemon.Op, body map[string]any) (applyResult, error) {
		raw, err := json.Marshal(body)
		if err != nil {
			return applyResult{}, err
		}
		reply, err := invoke(ctx, d, session, scope, op, raw)
		if err != nil {
			return applyResult{}, err
		}
		var res applyResult
		err = json.Unmarshal(reply.Body, &res)
		return res, err
	}
}

type applyTally struct {
	deleted, rewritten, skipped, refused, failed int
}

func (t applyTally) summary(dry bool) string {
	if dry {
		return fmt.Sprintf("dry run: would delete %d, would rewrite %d, skipped %d, refused %d, failed %d", t.deleted, t.rewritten, t.skipped, t.refused, t.failed)
	}
	return fmt.Sprintf("deleted %d, rewritten %d, skipped %d, refused %d, failed %d", t.deleted, t.rewritten, t.skipped, t.refused, t.failed)
}

type planRun struct {
	entries []planEntry
	done    map[string]bool
	dry     bool
	call    applyCall
	archive *jsonLines
	doneLog *jsonLines
	log     io.Writer
	pace    time.Duration
	sleep   func(context.Context, time.Duration) error
	now     func() time.Time
}

func openPlanRun(plan string, dry bool, call applyCall, log io.Writer) (*planRun, error) {
	entries, err := loadPlan(plan)
	if err != nil {
		return nil, err
	}
	files := planFilesFor(plan)
	done, err := readDone(files.done)
	if err != nil {
		return nil, err
	}
	r := &planRun{entries: entries, done: done, dry: dry, call: call, log: log, pace: applyPace, sleep: pause, now: time.Now}
	if dry {
		return r, nil
	}
	if r.archive, err = appendJSONLines(files.archive); err != nil {
		return nil, err
	}
	if r.doneLog, err = appendJSONLines(files.done); err != nil {
		return nil, errors.Join(err, r.archive.f.Close())
	}
	_, err = fmt.Fprintf(log, "archiving each message to %s before changing it, and logging each entry done to %s\n", files.archive, files.done)
	return r, err
}

func (r *planRun) close() {
	for _, l := range []*jsonLines{r.archive, r.doneLog} {
		if l != nil {
			_ = l.f.Close()
		}
	}
}

func (r *planRun) run(ctx context.Context) (applyTally, error) {
	var t applyTally
	var started time.Time
	for _, e := range r.entries {
		if e.Verdict == verdictKeep || r.done[e.TS] {
			t.skipped++
			continue
		}
		if wait := r.pace - r.now().Sub(started); !started.IsZero() && wait > 0 {
			if err := r.sleep(ctx, wait); err != nil {
				return t, err
			}
		}
		started = r.now()
		res, err := r.apply(ctx, e)
		if ctx.Err() != nil {
			return t, ctx.Err()
		}
		if err != nil {
			t.failed++
			if _, err := fmt.Fprintf(r.log, "failed %s: %v\n", e.TS, err); err != nil {
				return t, err
			}
			continue
		}
		if err := r.record(&t, e, res); err != nil {
			return t, err
		}
	}
	return t, nil
}

func (r *planRun) record(t *applyTally, e planEntry, res applyResult) error {
	var line string
	switch res.Outcome {
	case outcomeOwn:
		line = "would " + e.Verdict + " " + e.TS
		if e.Verdict == verdictDelete {
			t.deleted++
		} else {
			t.rewritten++
		}
	case outcomeDeleted:
		t.deleted++
		line = "deleted " + e.TS
	case outcomeRewritten:
		t.rewritten++
		line = "rewrote " + e.TS
	case outcomeRefused:
		t.refused++
		line = "refused " + e.TS + ": " + res.Reason
	case outcomeGone:
		t.skipped++
		line = "already gone " + e.TS + ": " + res.Reason
	default:
		return fmt.Errorf("entry %s: the daemon answered outcome %q", e.TS, res.Outcome)
	}
	if _, err := fmt.Fprintln(r.log, line); err != nil {
		return err
	}
	if r.dry {
		return nil
	}
	return r.doneLog.add(doneEntry{TS: e.TS, Outcome: res.Outcome, Reason: res.Reason, At: r.now().UTC().Format(time.RFC3339)})
}

func (r *planRun) apply(ctx context.Context, e planEntry) (applyResult, error) {
	body := map[string]any{"channel_id": e.channelID, "thread_ts": e.ThreadTS, "ts": e.TS}
	read, err := r.patiently(ctx, opApplyRead, body)
	if err != nil || read.Outcome != outcomeOwn || r.dry {
		return read, err
	}
	if err := r.archive.add(archivedMessage{ChannelID: e.channelID, Verdict: e.Verdict, NewText: e.NewText, Message: read.Message}); err != nil {
		return applyResult{}, fmt.Errorf("archive the message before changing it: %w", err)
	}
	body["verdict"] = e.Verdict
	if e.Verdict == verdictRewrite {
		body["text"] = e.NewText
	}
	return r.patiently(ctx, opApplyChange, body)
}

func (r *planRun) patiently(ctx context.Context, op daemon.Op, body map[string]any) (applyResult, error) {
	for {
		res, err := r.call(ctx, op, body)
		if err != nil || res.Outcome != outcomeRateLimited {
			return res, err
		}
		wait := max(time.Duration(res.RetryAfter)*time.Second, r.pace)
		if _, err := fmt.Fprintf(r.log, "Slack limits %s; waiting %s as it asks\n", op, wait); err != nil {
			return applyResult{}, err
		}
		if err := r.sleep(ctx, wait); err != nil {
			return applyResult{}, err
		}
	}
}

func applyCmd(d cmd.Deps) *cobra.Command {
	var (
		f      sessionFlags
		plan   string
		dryRun bool
	)
	c := &cobra.Command{
		Use:   "apply",
		Short: "Delete or rewrite the bot's own posts in your DM with it, entry by entry from a plan file",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			r, err := openPlanRun(plan, dryRun, applyVia(d, f.session, cwdOr(f.cwd)), c.ErrOrStderr())
			if err != nil {
				return err
			}
			defer r.close()
			tally, err := r.run(c.Context())
			if _, werr := fmt.Fprintln(c.OutOrStdout(), tally.summary(dryRun)); werr != nil {
				return errors.Join(err, werr)
			}
			if err != nil {
				return err
			}
			if tally.failed > 0 {
				return fmt.Errorf("%d entries failed; run the same command again to retry them", tally.failed)
			}
			return nil
		},
	}
	f.bind(c)
	c.Flags().StringVar(&plan, "plan", "", "JSON list of entries, each with ts, thread_ts, permalink, verdict (delete, rewrite, or keep), and new_text for a rewrite, all in your DM with the bot")
	c.Flags().BoolVar(&dryRun, "dry-run", false, "read and check every entry and print what it would do, changing nothing")
	if err := c.MarkFlagRequired("plan"); err != nil {
		panic(err)
	}
	return c
}
