//go:build linux

package main

import (
	"github.com/spf13/cobra"
	"github.com/valkyraycho/my-docker/internal/container"
)

// initCmd is the re-exec trampoline that runs *inside* the new namespaces after
// the parent process called unshare. It is hidden from the help text because
// users never call it directly — "mydocker run" re-execs the binary with
// "init" as the first argument so that container.Init runs with the correct
// PID, mount, and UTS namespaces already in place. It performs pivot_root to
// switch the filesystem root, then execs the user-supplied command as PID 1.
var initCmd = &cobra.Command{
	Use:                "init <rootfs> <cmd> [args...]",
	Short:              "Container entrypoint (internal use only)",
	Hidden:             true,
	Args:               cobra.MinimumNArgs(2),
	DisableFlagParsing: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		return container.Init(args[0], args[1:])
	},
}
