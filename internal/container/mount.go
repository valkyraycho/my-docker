//go:build linux

package container

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func setupRoot(rootfs string) error {
	if err := unix.Mount("", "/", "", unix.MS_PRIVATE|unix.MS_REC, ""); err != nil {
		return fmt.Errorf("make / private: %w", err)
	}

	if err := unix.Mount(rootfs, rootfs, "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
		return fmt.Errorf("bind-mount rootfs: %w", err)
	}

	oldRoot := filepath.Join(rootfs, ".old_root")

	if err := os.MkdirAll(oldRoot, 0755); err != nil {
		return fmt.Errorf("mkdir old_root: %w", err)
	}

	if err := unix.PivotRoot(rootfs, oldRoot); err != nil {
		return fmt.Errorf("pivot_root: %w", err)
	}

	if err := os.Chdir("/"); err != nil {
		return fmt.Errorf("chdir /: %w", err)
	}

	if err := unix.Unmount("/.old_root", unix.MNT_DETACH); err != nil {
		return fmt.Errorf("unmount old_root: %w", err)
	}

	if err := os.RemoveAll("/.old_root"); err != nil {
		return fmt.Errorf("remove old_root: %w", err)
	}

	return nil
}

func setupMounts() error {
	if err := unix.Mount("proc", "/proc", "proc", 0, ""); err != nil {
		return fmt.Errorf("mount /proc: %w", err)
	}
	if err := unix.Mount("tmpfs", "/dev", "tmpfs", 0, ""); err != nil {
		return fmt.Errorf("mount /dev: %w", err)
	}
	if err := unix.Mount("sysfs", "/sys", "sysfs", 0, ""); err != nil {
		return fmt.Errorf("mount /sys: %w", err)
	}
	return nil
}
