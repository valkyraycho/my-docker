//go:build linux

// Package main is the CLI entry point for the mydocker client.
// It wires up all subcommands under the root cobra command and owns
// top-level error printing so subcommands don't double-report failures.
package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"

	"github.com/spf13/cobra"
)

// rootCmd is the top-level cobra command ("mydocker"). All subcommands are
// registered as children in main().
var rootCmd = &cobra.Command{
	Use:          "mydocker",
	Short:        "A minimal container runtime, for learning",
	SilenceUsage: true,
}

func main() {
	rootCmd.AddCommand(runCmd, initCmd, pullCmd, psCmd, logsCmd, stopCmd, rmCmd, inspectCmd, versionCmd)

	// Let main() own all error printing. Each subcommand silences cobra's
	// default printer so we avoid double-reporting (especially for run,
	// where the child process has already printed its own error).
	for _, c := range rootCmd.Commands() {
		c.SilenceErrors = true
	}

	err := rootCmd.Execute()
	if err == nil {
		return
	}

	// ExitError = container exited non-zero. The child has already printed
	// any real error; we only need to propagate the exit code silently.
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		os.Exit(ee.ExitCode())
	}

	// Any other error is a CLI-layer failure we own printing for.
	fmt.Fprintln(os.Stderr, "Error:", err)
	os.Exit(1)
}

func init() {
	rootCmd.PersistentFlags().StringVarP(&hostFlag, "host", "H", "", "Daemon socket to connect to (unix://path or env MYDOCKER_HOST)")
}
