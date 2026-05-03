//go:build linux

package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

// killCmd implements "mydocker kill". Unlike stop, there is no grace
// period — the daemon SIGKILLs the container's init immediately.
// Useful when a process is unresponsive to SIGTERM.
var killCmd = &cobra.Command{
	Use:   "kill <id>",
	Short: "Kill a running container (SIGKILL, no grace period)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cli, err := getClient()
		if err != nil {
			return err
		}
		if err := cli.ContainerKill(cmd.Context(), args[0]); err != nil {
			return fmt.Errorf("kill: %w", err)
		}
		fmt.Println(args[0])
		return nil
	},
}
