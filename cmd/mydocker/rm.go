//go:build linux

package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

// rmForce is set by the -f flag; when true, the daemon stops a running
// container before removing it (matches Docker's `docker rm -f`).
var rmForce bool

// rmCmd implements "mydocker rm" as a thin wrapper around the daemon's
// DELETE /containers/{id} endpoint. Without -f, the daemon returns
// 409 Conflict for a running container; with -f it stops-then-removes
// atomically.
var rmCmd = &cobra.Command{
	Use:   "rm [flags] <id>",
	Short: "Remove a container",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cli, err := getClient()
		if err != nil {
			return err
		}
		if err := cli.ContainerRemove(cmd.Context(), args[0], rmForce); err != nil {
			return fmt.Errorf("rm: %w", err)
		}
		fmt.Println(args[0])
		return nil
	},
}

func init() {
	rmCmd.Flags().BoolVarP(&rmForce, "force", "f", false, "force removal (stops if running)")
}
