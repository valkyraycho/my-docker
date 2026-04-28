//go:build linux

package volume

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

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
