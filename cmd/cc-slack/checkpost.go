package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"

	"github.com/spf13/cobra"
)

const pluginRootEnv = "BINRUN_PLUGIN_ROOT"

type exitStatus int

func (s exitStatus) Error() string { return "exit status " + strconv.Itoa(int(s)) }

func checkPostCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "check-post <file|->",
		Short: "Check a Slack post's text with the plugin's check-post before sending it",
		Long: `Run the plugin's bundled check-post on a post's text, read from a file or
from stdin with -. It prints each finding with its fix and exits 1 when there is
one, 0 when the text is clean, and 2 on a usage error, as the script does. It
runs the check-post of the newest installed plugin, which a session's PATH may
not hold.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			root := os.Getenv(pluginRootEnv)
			if root == "" {
				return errors.New("check-post: " + pluginRootEnv + " is unset, so the plugin's check-post cannot be found; run cc-slack through its plugin's bin/cc-slack")
			}
			check := exec.CommandContext(c.Context(), filepath.Join(root, "skills", "slack", "scripts", "check-post"), args...) //nolint:gosec // G204: the plugin's own check-post script, never a shell
			check.Stdin, check.Stdout, check.Stderr = c.InOrStdin(), c.OutOrStdout(), c.ErrOrStderr()
			err := check.Run()
			if exited, ok := errors.AsType[*exec.ExitError](err); ok {
				return exitStatus(exited.ExitCode())
			}
			return err
		},
	}
}
