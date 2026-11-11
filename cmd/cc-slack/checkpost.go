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

type exitStatus int

func (s exitStatus) Error() string { return "exit status " + strconv.Itoa(int(s)) }

func checkPostCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "check-post <file|->",
		Short: "Check a Slack post's text with the plugin's check-post before sending it",
		Long: `Run the plugin's bundled check-post on a post's text, read from a file or
from stdin with -, then check that a line saying something waits on the reader
links the open card it means, and that no link to a message in your DM with the
app is wrapped as <url|url> where the bare link would unfurl. It prints each finding with its fix and exits 1
when there is one, 0 when the text is clean, and 2 on a usage error, as the
script does. It runs the check-post of the newest installed plugin, which a
session's PATH may not hold.`,
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
			check := exec.CommandContext(c.Context(), filepath.Join(root, checkPostScript), "-") //nolint:gosec // G204: the plugin's own check-post script, never a shell
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
			if said, found := waitsWithoutCard(text, dmCard); found && len(linkedPRRefs(text)) == 0 {
				findings = append(findings, said)
			}
			for _, wrapped := range wrappedCardLinks(text, dmLinkAsLabel) {
				findings = append(findings, wrapped)
			}
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
