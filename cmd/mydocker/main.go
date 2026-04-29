//go:build linux

package main

import (
	"errors"
	"fmt"
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
