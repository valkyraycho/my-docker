//go:build linux

package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/valkyraycho/my-docker/internal/state"
)

var inspectCmd = &cobra.Command{
	Use:   "inspect <id>",
	Short: "Display detailed information about a container",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := state.Find(args[0])
		if err != nil {
			return err
		}

		b, err := json.MarshalIndent(c, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal state: %w", err)
		}

		fmt.Fprintln(os.Stdout, string(b))
		return nil
	},
}
