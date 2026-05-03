//go:build linux

package main

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"github.com/valkyraycho/my-docker/internal/container"
)

// stopTimeout is set by the -t flag; it controls how long to wait for a
// graceful SIGTERM before escalating to SIGKILL.
var stopTimeout time.Duration

// stopCmd implements "mydocker stop". It sends SIGTERM to the container and
// waits up to -t (default container.DefaultStopTimeout) before sending SIGKILL.
var stopCmd = &cobra.Command{
	Use:   "stop [flags] <id>",
	Short: "Stop a running container",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := container.Stop(args[0], stopTimeout); err != nil {
			return fmt.Errorf("stop: %w", err)
		}
		fmt.Println(args[0])
		return nil
	},
}

func init() {
	stopCmd.Flags().DurationVarP(&stopTimeout, "time", "t", container.DefaultStopTimeout, "timeout before sending SIGKILL")
}
