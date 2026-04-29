//go:build linux

package main

import (
	"github.com/spf13/cobra"
	"github.com/valkyraycho/my-docker/internal/container"
)

var initCmd = &cobra.Command{
	Use:    "init <rootfs> <cmd> [args...]",
	Short:  "Container entrypoint (internal use only)",
	Hidden: true,
	Args:   cobra.MinimumNArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		return container.Init(args[0], args[1:])
	},
}
