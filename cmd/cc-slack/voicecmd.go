package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	spawnllm "github.com/yasyf/spawnllm/go"
)

func voiceCmd() *cobra.Command {
	var req voiceRequest
	var last int
	var here, all bool
	c := &cobra.Command{
		Use:   "voice [file|-]",
		Short: "Show what the daemon's voice sender does to a post: the text as written and as it would send",
		Long: `With a post's text, from a file or stdin with -, ask the running daemon's
voice sender to format it and run it by Jev against the --rules page, without
posting, and print the text before and after with each stage's milliseconds.
With --last, print the before and after of the newest posts it sent, from
voice.jsonl in the state directory, which also holds each post's timings;
--all adds the rows that were never posts: dry runs, refusals, failed sends,
settle and tap rows.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			if last > 0 {
				outs, err := readVoiceLog(filepath.Join(appPaths().StateDir(), voiceLogName), last, all)
				if err != nil {
					return err
				}
				for _, out := range outs {
					if err := printVoice(c.OutOrStdout(), out); err != nil {
						return err
					}
				}
				return nil
			}
			text, err := postText(c, args)
			if err != nil {
				return err
			}
			req.Text, req.DryRun = text, true
			ask := askVoice
			if here {
				ask = func(r voiceRequest) (voiceOutcome, error) { return voiceHere(c.Context(), r) }
			}
			out, err := ask(req)
			if err != nil {
				return err
			}
			return printVoice(c.OutOrStdout(), out)
		},
	}
	c.Flags().StringVar(&req.Rules, "rules", "", "the role page Jev reads the post against")
	c.Flags().StringVar(&req.ChannelID, "channel", "", "the operator channel the post would go to")
	c.Flags().StringVar(&req.Asked, "asked", "", "his message the post answers")
	c.Flags().StringVar(&req.Lane, "lane", "", "the lane posting")
	c.Flags().BoolVar(&here, "here", false, "run the sender in this process, not the daemon, to try a rules page before it installs")
	c.Flags().IntVar(&last, "last", 0, "print the newest N posts the sender handled")
	c.Flags().BoolVar(&all, "all", false, "with --last, also print rows that were never posts: dry runs, refusals, failed sends, settle and tap rows")
	return c
}

func askVoice(req voiceRequest) (voiceOutcome, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return voiceOutcome{}, err
	}
	r, err := http.NewRequest(http.MethodPost, "http://"+toolsAddr+voicePath, bytes.NewReader(body)) //nolint:noctx // a CLI call bounded by the client's timeout
	if err != nil {
		return voiceOutcome{}, err
	}
	r.Header.Set(toolsPIDHeader, strconv.Itoa(claudePID()))
	resp, err := (&http.Client{Timeout: voiceJevLate + voicePostLimit}).Do(r)
	if err != nil {
		return voiceOutcome{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return voiceOutcome{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return voiceOutcome{}, fmt.Errorf("voice: %s: %s", resp.Status, bytes.TrimSpace(raw))
	}
	var out voiceOutcome
	err = json.Unmarshal(raw, &out)
	return out, err
}

func printVoice(w io.Writer, out voiceOutcome) error {
	var b strings.Builder
	fmt.Fprintf(&b, "── %s %s %s · jev %s · format %dms jev %dms post %dms total %dms\n", out.At.Local().Format(time.Kitchen), out.Lane, out.ID, out.Jev, out.FormatMS, out.JevMS, out.PostMS, out.TotalMS)
	if out.Dropped != "" || out.Error != "" {
		fmt.Fprintf(&b, "not sent: %s%s\n", out.Dropped, out.Error)
	}
	fmt.Fprintf(&b, "BEFORE\n%s\n\nAFTER\n%s\n", out.Before, out.After)
	if len(out.Cut) > 0 || out.Repeat > 0 {
		fmt.Fprintf(&b, "(cut %v, repeat %.2f)\n", out.Cut, out.Repeat)
	}
	if len(out.Scores) > 0 {
		fmt.Fprintf(&b, "jev cut scores %v\n", out.Scores)
	}
	if out.Permalink != "" {
		fmt.Fprintln(&b, out.Permalink)
	}
	b.WriteString("\n")
	_, err := io.WriteString(w, b.String())
	return err
}

func voiceHere(ctx context.Context, req voiceRequest) (voiceOutcome, error) {
	v := &voiceSender{decide: spawnllm.Decide, rules: map[string]voiceRules{}}
	rules, err := v.rulesAt(req.Rules)
	if err != nil {
		return voiceOutcome{}, err
	}
	began := time.Now()
	out := voiceOutcome{At: began, Lane: req.Lane, Before: req.Text, DryRun: true}
	out.Formatted = voiceFormat(req.Text)
	out.FormatMS = time.Since(began).Milliseconds()
	if out.Dropped = voiceMechanical(out.Formatted); out.Dropped != "" {
		return out, nil
	}
	units := voiceUnits(out.Formatted)
	jev := time.Now()
	e := v.ask(ctx, rules, req.Asked, "", units)
	out.JevMS, out.Jev = time.Since(jev).Milliseconds(), cmp.Or(e.note, "answered")
	out.After, out.Cut, out.Scores = voiceApplied(units, e), e.cut, e.scores
	out.TotalMS = time.Since(began).Milliseconds()
	return out, nil
}
