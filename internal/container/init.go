//go:build linux

package container

import (
	"fmt"
	"os"
	"os/exec"

	"golang.org/x/sys/unix"
)

func Init(rootfs string, args []string) error {
	if err := unix.Sethostname([]byte("my-docker")); err != nil {
		return fmt.Errorf("sethostname: %w", err)
	}

	if err := setupRoot(rootfs); err != nil {
		return fmt.Errorf("setup root: %w", err)
	}

	if err := setupMounts(); err != nil {
		return fmt.Errorf("setup mounts: %w", err)
	}

	binary, err := exec.LookPath(args[0])
	if err != nil {
		return fmt.Errorf("lookpath %q: %w", args[0], err)
	}

	return unix.Exec(binary, args, os.Environ())
}
