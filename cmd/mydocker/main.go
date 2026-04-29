//go:build linux

package main

import (
	"errors"
	"os"
	"os/exec"

	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:          "mydocker",
	Short:        "A minimal container runtime, for learning",
	SilenceUsage: true,
}

func main() {
	rootCmd.AddCommand(runCmd, initCmd, pullCmd, psCmd, logsCmd, stopCmd, rmCmd, inspectCmd)
	err := rootCmd.Execute()
	if err == nil {
		return
	}

	var ee *exec.ExitError
	if errors.As(err, &ee) {
		os.Exit(ee.ExitCode())
	}
	os.Exit(1)
}
