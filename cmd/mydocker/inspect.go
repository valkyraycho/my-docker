//go:build linux

package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// inspectCmd implements "mydocker inspect". It asks the daemon for the
// container's full state via GET /containers/{id}/json and pretty-prints
// the response as indented JSON — same shape `docker inspect` produces.
var inspectCmd = &cobra.Command{
	Use:   "inspect <id>",
	Short: "Display detailed information about a container",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cli, err := getClient()
		if err != nil {
			return err
		}

		info, err := cli.ContainerInspect(cmd.Context(), args[0])
		if err != nil {
			return fmt.Errorf("inspect: %w", err)
		}

		b, err := json.MarshalIndent(info, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal state: %w", err)
		}

		fmt.Fprintln(os.Stdout, string(b))
		return nil
	},
}
