//go:build linux

package main

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"
)

// stopTimeout is set by the -t flag; it controls how long the daemon
// waits for a graceful SIGTERM before escalating to SIGKILL.
var stopTimeout time.Duration

// stopCmd implements "mydocker stop". After the M9 daemon split it
// talks to mydockerd over the UNIX socket — the daemon owns the
// signal escalation and state transitions.
var stopCmd = &cobra.Command{
	Use:   "stop [flags] <id>",
	Short: "Stop a running container",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cli, err := getClient()
		if err != nil {
			return err
		}
		if err := cli.ContainerStop(cmd.Context(), args[0], stopTimeout); err != nil {
			return fmt.Errorf("stop: %w", err)
		}
		fmt.Println(args[0])
		return nil
	},
}

func init() {
	stopCmd.Flags().DurationVarP(&stopTimeout, "time", "t", 10*time.Second,
		"timeout before sending SIGKILL")
}
