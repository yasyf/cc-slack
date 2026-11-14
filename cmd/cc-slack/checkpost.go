package main

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/yasyf/cc-interact/version"
)

const pluginRootEnv = "BINRUN_PLUGIN_ROOT"

var checkPostScript = filepath.Join("skills", "slack", "scripts", "check-post")

var releaseDir = regexp.MustCompile(`^\d+(\.\d+)*$`)

const paragraphSentences = 3

var (
	sentenceEnd = regexp.MustCompile(`[.!?][*_~]*(?:\s|$)`)
	listItem    = regexp.MustCompile(`^\s*(?:[-*•◦]|\d+[.)])\s`)
	codeSpan    = regexp.MustCompile("`[^`\n]+`")
	quoted      = regexp.MustCompile(`"[^"\n]*"|“[^”\n]*”`)
	blockQuote  = regexp.MustCompile(`^\s*&gt;`)
)

type exitStatus int

func (s exitStatus) Error() string { return "exit status " + strconv.Itoa(int(s)) }

func checkPostCmd() *cobra.Command {
	var asked []string
	c := &cobra.Command{
		Use:   "check-post <file|->",
		Short: "Check a Slack post's text with the plugin's check-post before sending it",
		Long: `Run the plugin's bundled check-post on a post's text, read from a file or
from stdin with -, then check that no paragraph of prose, as cc-slack draws the
post, holds more than three sentences, where a table, heading, picture, chart or
code block is never prose, that a line saying something waits on the reader
links the open card it means, and that no link to a message in your DM with the
app shows as a raw URL, bare or labeled with its own URL, where words naming it
would show the words and Slack would draw the message under them, and that no
one line links more than five Slack messages, since Slack draws five a message
and cc-slack sends a longer post to your DM as pages cut between lines. It
prints each finding with its fix and exits 1 when there is one, 0 when the text
is clean, and 2 on a usage error, as the script does. Each --asked is a message
of the reader's the post answers, word for word, and a multi-agent term the
reader used there, such as the drive, passes, as does an internal id such as a
run id when that message names it or asks for ids. It runs the check-post of the
newest installed plugin, which a session's PATH may not hold.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			root, err := pluginRoot()
			if err != nil {
				return fmt.Errorf("check-post: %w", err)
			}
			text, err := postText(c, args)
			if err != nil {
				return fmt.Errorf("check-post: %w", err)
			}
			script := []string{"-"}
			for _, message := range asked {
				script = append(script, "--asked="+message)
			}
			check := exec.CommandContext(c.Context(), filepath.Join(root, checkPostScript), script...) //nolint:gosec // G204: the plugin's own check-post script, never a shell
			check.Stdin, check.Stdout, check.Stderr = strings.NewReader(text), c.OutOrStdout(), c.ErrOrStderr()
			status := 0
			if err := check.Run(); err != nil {
				exited, ok := errors.AsType[*exec.ExitError](err)
				if !ok {
					return err
				}
				status = exited.ExitCode()
			}
			var findings []fmt.Stringer
			if said, found := waitsWithoutCard(text, dmCard, len(linkedPRRefs(text)) > 0); found {
				findings = append(findings, said)
			}
			for _, raw := range rawCardLinks(text, dmCard) {
				findings = append(findings, raw)
			}
			if crowded, found := crowdedLineIn(text, dmCard); found {
				findings = append(findings, crowded)
			}
			findings = append(findings, wallsIn(text)...)
			for _, finding := range findings {
				if _, err := fmt.Fprintln(c.OutOrStdout(), finding); err != nil {
					return err
				}
				status = max(status, 1)
			}
			if status != 0 {
				return exitStatus(status)
			}
			return nil
		},
	}
	c.Flags().StringArrayVar(&asked, "asked", nil, "a message of the reader's that the post answers, word for word; repeat for each")
	return c
}

func postText(c *cobra.Command, args []string) (string, error) {
	if len(args) == 0 || args[0] == "-" {
		text, err := io.ReadAll(c.InOrStdin())
		return string(text), err
	}
	text, err := os.ReadFile(args[0])
	return string(text), err
}

func pluginRoot() (string, error) {
	if root := os.Getenv(pluginRootEnv); root != "" {
		if _, err := os.Stat(filepath.Join(root, checkPostScript)); err == nil { //nolint:gosec // G703: the plugin root its launcher exported, checked for this plugin's own script
			return root, nil
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	versions := filepath.Join(cmp.Or(os.Getenv("CLAUDE_CONFIG_DIR"), filepath.Join(home, ".claude")), "plugins", "cache", channelPlugin.Marketplace, channelPlugin.Name)
	entries, err := os.ReadDir(versions)
	if err != nil {
		return "", fmt.Errorf("no %s plugin is installed: %w", channelPlugin.Name, err)
	}
	newest := ""
	for _, e := range entries {
		if _, err := os.Stat(filepath.Join(versions, e.Name(), ".orphaned_at")); !e.IsDir() || !releaseDir.MatchString(e.Name()) || err == nil { //nolint:gosec // G703: a version directory of this plugin in the user's own Claude config dir
			continue
		}
		if newest == "" || version.Newer(e.Name(), newest) {
			newest = e.Name()
		}
	}
	if newest == "" {
		return "", fmt.Errorf("no %s plugin version under %s is in use", channelPlugin.Name, versions)
	}
	return filepath.Join(versions, newest), nil
}

type wallOfText struct {
	line      int
	opening   string
	sentences int
}

func (w wallOfText) String() string {
	return fmt.Sprintf("%d:1 wall-of-text %q: %d sentences in one paragraph; keep each paragraph to %d sentences or fewer, and put steps and settings in a bulleted list", w.line, w.opening, w.sentences, paragraphSentences)
}

func wallsIn(text string) []fmt.Stringer {
	lines := strings.Split(text, "\n")
	var walls []fmt.Stringer
	for _, piece := range bodyPieces(text) {
		if piece.table != nil || piece.picture != nil || strings.HasPrefix(piece.text, "```") {
			continue
		}
		for at, line := range strings.Split(piece.text, "\n") {
			if listItem.MatchString(line) || blockQuote.MatchString(line) {
				continue
			}
			sentences := len(sentenceEnd.FindAllString(quoted.ReplaceAllString(codeSpan.ReplaceAllString(slackToken.ReplaceAllString(line, " "), " "), " "), -1))
			if sentences > paragraphSentences {
				opening := strings.Fields(lines[piece.line+at-1])
				walls = append(walls, wallOfText{piece.line + at, strings.Join(opening[:min(len(opening), 6)], " "), sentences})
			}
		}
	}
	return walls
}
