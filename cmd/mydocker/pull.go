//go:build linux

package main

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/valkyraycho/my-docker/internal/image"
	"github.com/valkyraycho/my-docker/internal/registry"
)

var pullCmd = &cobra.Command{
	Use:   "pull <image>[:<tag>]",
	Short: "Pull an image from a registry",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ref := args[0]
		client := registry.New(image.DefaultRegistry)
		store := image.New()
		if err := store.Pull(client, ref); err != nil {
			return fmt.Errorf("pull: %w", err)
		}
		fmt.Printf("pulled %s\n", ref)
		return nil
	},
}
