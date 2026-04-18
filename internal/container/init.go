//go:build linux

package container

import (
	"fmt"
	"os"
	"os/exec"

	"golang.org/x/sys/unix"
)

func Init(args []string) error {
	if err := unix.Sethostname([]byte("my-docker")); err != nil {
		return fmt.Errorf("sethostname: %w", err)
	}

	binary, err := exec.LookPath(args[0])
	if err != nil {
		return fmt.Errorf("lookpath %q: %w", args[0], err)
	}

	return unix.Exec(binary, args, os.Environ())
}
