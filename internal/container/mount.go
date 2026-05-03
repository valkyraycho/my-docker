//go:build linux

package container

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// setupRoot performs pivot_root to make rootfs the container's filesystem root.
// The sequence is: (1) re-mount / as MS_PRIVATE so host mount events don't
// propagate into the namespace; (2) bind-mount rootfs onto itself so it becomes
// a mount point (pivot_root requires the new root to be a mount point); (3) call
// pivot_root, which atomically swaps the root and stashes the old root under
// .old_root; (4) chdir to "/" so the working directory is valid in the new root;
// (5) lazy-unmount .old_root so the host filesystem is no longer reachable.
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

// setupMounts populates the essential virtual filesystems inside the new root.
// /proc exposes per-process kernel state (required by many tools like ps and top).
// /dev is a fresh tmpfs so we control exactly which device nodes exist.
// /sys exposes kernel subsystem state (cgroup info, network stats, etc.).
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
	if err := createDevNodes(); err != nil {
		return fmt.Errorf("create /dev nodes: %w", err)
	}
	return nil
}

// createDevNodes populates the minimal set of character devices that well-behaved
// processes expect (null, zero, full, random, urandom, tty). Because /dev is a
// fresh tmpfs, none of these exist until we mknod them explicitly.
func createDevNodes() error {
	nodes := []struct {
		path  string
		mode  uint32
		major uint32
		minor uint32
	}{
		{"/dev/null", unix.S_IFCHR | 0666, 1, 3},
		{"/dev/zero", unix.S_IFCHR | 0666, 1, 5},
		{"/dev/full", unix.S_IFCHR | 0666, 1, 7},
		{"/dev/random", unix.S_IFCHR | 0666, 1, 8},
		{"/dev/urandom", unix.S_IFCHR | 0666, 1, 9},
		{"/dev/tty", unix.S_IFCHR | 0666, 5, 0},
	}

	for _, n := range nodes {
		dev := unix.Mkdev(n.major, n.minor)
		if err := unix.Mknod(n.path, n.mode, int(dev)); err != nil {
			return fmt.Errorf("mknod %s: %w", n.path, err)
		}
	}
	return nil
}
