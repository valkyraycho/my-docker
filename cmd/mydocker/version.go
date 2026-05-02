package main

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/valkyraycho/my-docker/internal/client"
)

const clientSocketPath = "/var/run/mydocker.sock"

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Show the mydockerd daemon version and capabilities",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		c := client.New(clientSocketPath)
		pingResult, err := c.Ping(cmd.Context())
		if err != nil {
			return fmt.Errorf("contact mydockerd: %w", err)
		}

		fmt.Printf("API version: %s\n", pingResult.APIVersion)
		fmt.Printf("OSType: %s\n", pingResult.OSType)
		if pingResult.BuilderVersion != "" {
			fmt.Printf("Builder version: %s\n", pingResult.BuilderVersion)
		}
		return nil
	},
}
