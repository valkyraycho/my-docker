//go:build linux

package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/valkyraycho/my-docker/internal/container"
)

var logsCmd = &cobra.Command{
	Use:   "logs <id>",
	Short: "Fetch the logs of a container",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := container.Logs(os.Stdout, args[0]); err != nil {
			return fmt.Errorf("logs: %w", err)
		}
		return nil
	},
}
