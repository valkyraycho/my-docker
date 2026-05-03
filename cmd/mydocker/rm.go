//go:build linux

package main

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/valkyraycho/my-docker/internal/container"
)

// rmForce is set by the -f flag; when true, a running container is stopped
// before removal.
var rmForce bool

// rmCmd implements "mydocker rm". With -f it stops a running container before
// removing it; without -f it returns an error if the container is still running.
var rmCmd = &cobra.Command{
	Use:   "rm [flags] <id>",
	Short: "Remove a container",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := container.Rm(args[0], rmForce); err != nil {
			return fmt.Errorf("rm: %w", err)
		}
		fmt.Println(args[0])
		return nil
	},
}

func init() {
	rmCmd.Flags().BoolVarP(&rmForce, "force", "f", false, "force removal (stops if running)")
}
