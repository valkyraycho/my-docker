//go:build linux

package main

import (
	"os"

	"github.com/spf13/cobra"
	"github.com/valkyraycho/my-docker/internal/container"
)

// psShowAll is set by the -a flag; when true, stopped containers are included.
var psShowAll bool

// psCmd implements "mydocker ps". By default only running containers are shown;
// pass -a to include stopped containers.
var psCmd = &cobra.Command{
	Use:   "ps",
	Short: "List containers",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return container.Ps(os.Stdout, psShowAll)
	},
}

func init() {
	psCmd.Flags().BoolVarP(&psShowAll, "all", "a", false, "show all containers (default: running only)")
}
