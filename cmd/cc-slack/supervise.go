package main

import "github.com/spf13/cobra"

func superviseCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "supervise",
		Short: "Supervise the cc-slack daemon in the foreground on a linux host with no launchd, as its service-manager entry",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return runSupervisor(c.Context())
		},
	}
}
