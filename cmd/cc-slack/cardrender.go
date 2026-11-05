package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/slack-go/slack"
	"github.com/spf13/cobra"
)

type renderedCard struct {
	Text   string          `json:"text"`
	Blocks []slack.Block   `json:"blocks"`
	Fold   [][]slack.Block `json:"fold,omitempty"`

	Entities []slack.WorkObjectEntity `json:"entities,omitempty"`
}

func cardCmd() *cobra.Command {
	c := &cobra.Command{Use: "card", Short: "Read a status card without posting it"}
	c.AddCommand(cardRenderCmd())
	return c
}

func cardRenderCmd() *cobra.Command {
	var (
		f        sessionFlags
		path     string
		mirrored bool
	)
	c := &cobra.Command{
		Use:   "render",
		Short: "Print the Block Kit a status card draws, reaching neither Slack nor the daemon",
		Long: `Print the message a status card draws as JSON: its notification text, the
blocks of the card itself, under fold the pages Explain more posts, and under
entities the Work Objects its artifacts post. It refuses what dm-status refuses
for that card. It reaches neither Slack nor the daemon, so PR and link previews
and an artifact's uploaded picture, which need them, stay out of the output.`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			dir := cwdOr(f.cwd)
			abs := path
			if !filepath.IsAbs(abs) {
				abs = filepath.Join(dir, path)
			}
			raw, err := os.ReadFile(abs)
			if err != nil {
				return fmt.Errorf("read --card: %w", err)
			}
			rendered, err := renderCard(c.Context(), raw, f.session, dir, mirrored, time.Now())
			if err != nil {
				return err
			}
			out := json.NewEncoder(c.OutOrStdout())
			out.SetIndent("", "  ")
			out.SetEscapeHTML(false)
			return out.Encode(rendered)
		},
	}
	f.bind(c)
	c.Flags().StringVar(&path, "card", "", "file holding the status card as JSON")
	c.Flags().BoolVar(&mirrored, "mirrored", false, "check it as a waiting card cc-present sends, as dm-status --mirrored does")
	_ = c.MarkFlagRequired("card")
	return c
}

func renderCard(ctx context.Context, raw []byte, session, dir string, mirrored bool, now time.Time) (renderedCard, error) {
	var card statusCard
	if err := json.Unmarshal(raw, &card); err != nil {
		return renderedCard{}, fmt.Errorf("--card takes a JSON status card: %w", err)
	}
	card.fill("")
	path, required, err := configPath("", dir)
	if err != nil {
		return renderedCard{}, err
	}
	cfg, err := loadConfig(path, required)
	if err != nil {
		return renderedCard{}, err
	}
	check := card.check
	if mirrored {
		check = card.checkMirrored
	}
	if err := check(now, false, cfg.Links.Tailnet); err != nil {
		return renderedCard{}, err
	}
	blocks, fold := card.layout(cardView{tag: gitCardTag(ctx, session, dir), at: now, now: now})
	return renderedCard{Text: card.text(), Blocks: blocks, Fold: fold, Entities: card.workObjects()}, nil
}
