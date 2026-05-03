//go:build linux

package volume

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// Mount bind-mounts the volume described by spec into the container's rootfs.
// For Named volumes the data directory is created first via EnsureNamed. Read-
// only mounts require a two-step syscall: bind-mount first (which is always
// read-write), then remount with MS_RDONLY — the kernel enforces this order.
func Mount(spec *Spec, rootfs string) error {
	var source string
	switch spec.Kind {
	case Bind:
		source = spec.Source
	case Named:
		var err error
		source, err = EnsureNamed(spec.Source)
		if err != nil {
			return fmt.Errorf("ensure volume: %w", err)
		}
	}

	target := filepath.Join(rootfs, spec.Target)
	if err := os.MkdirAll(target, 0755); err != nil {
		return fmt.Errorf("mkdir %s: %w", target, err)
	}

	if err := unix.Mount(source, target, "", unix.MS_BIND, ""); err != nil {
		return fmt.Errorf("mount %s to %s: %w", source, target, err)
	}
	if spec.ReadOnly {
		if err := unix.Mount("", target, "", unix.MS_BIND|unix.MS_REMOUNT|unix.MS_RDONLY, ""); err != nil {
			_ = unix.Unmount(target, unix.MNT_DETACH)
			return fmt.Errorf("remount %s for readonly: %w", target, err)
		}
	}

	return nil
}

// Unmount lazily detaches the volume bind mount from the container's rootfs.
// ENOENT is treated as success so cleanup is idempotent when called after a
// partial setup failure.
func Unmount(spec *Spec, rootfs string) error {
	target := filepath.Join(rootfs, spec.Target)

	if err := unix.Unmount(target, unix.MNT_DETACH); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return fmt.Errorf("unmount %s: %w", target, err)
	}
	return nil
}
