//go:build linux

// Package overlay manages OverlayFS mounts for container filesystems.
// Each container gets a stack of read-only image layers (lowerdir) combined
// with a writable upperdir; all changes go to upper so the image layers are
// never modified. A workdir is required by the kernel for internal OverlayFS
// bookkeeping. The merged view is what the container sees as its root.
package overlay

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

const (
	root = "/var/lib/mydocker"

	layersDir = "/var/lib/mydocker/layers"

	containersDir = "/var/lib/mydocker/containers"
)

// EnsureRoot creates the on-disk directory layout under /var/lib/mydocker and
// mounts a tmpfs over the containers directory. Tmpfs is used so that overlay
// upper/work/merged dirs are automatically cleaned up on reboot, preventing
// stale mounts from accumulating across daemon restarts.
func EnsureRoot() error {
	for _, d := range []string{root, layersDir} {
		if err := os.MkdirAll(d, 0755); err != nil {
			return fmt.Errorf("mkdir %s: %w", d, err)
		}
	}

	if err := os.MkdirAll(containersDir, 0755); err != nil {
		return fmt.Errorf("mkdir %s: %w", containersDir, err)
	}

	mounted, err := isTmpfs(containersDir)
	if err != nil {
		return fmt.Errorf("check mount: %w", err)
	}

	if !mounted {
		if err := unix.Mount("tmpfs", containersDir, "tmpfs", 0, ""); err != nil {
			return fmt.Errorf("mount tmpfs on %s: %w", root, err)
		}
	}

	return nil
}

// isTmpfs reports whether path is already mounted as a tmpfs by parsing
// /proc/mounts, avoiding a redundant mount call on daemon restart.
func isTmpfs(path string) (bool, error) {
	f, err := os.Open("/proc/mounts")
	if err != nil {
		return false, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 3 {
			continue
		}

		target := fields[1]
		fstype := fields[2]
		if target == path && fstype == "tmpfs" {
			return true, nil
		}
	}
	return false, scanner.Err()

}

// Mount assembles an OverlayFS for containerID from the given image layer
// names. Layers are ordered so the first name (top of the Docker image stack)
// maps to the highest-priority lowerdir. Returns the path to the merged
// directory, which becomes the container's rootfs.
func Mount(containerID string, layerNames []string) (string, error) {
	for _, name := range layerNames {
		layer := filepath.Join(layersDir, name)
		info, err := os.Stat(layer)
		switch {
		case errors.Is(err, os.ErrNotExist):
			return "", fmt.Errorf("layer %q not found in %s", name, layersDir)
		case err != nil:
			return "", fmt.Errorf("check layer %q: %w", name, err)
		case !info.IsDir():
			return "", fmt.Errorf("layer %q is not a directory", name)
		}
	}

	paths := make([]string, len(layerNames))
	for i, name := range layerNames {
		paths[len(layerNames)-1-i] = filepath.Join(layersDir, name)
	}
	lowerdir := strings.Join(paths, ":")

	containerDir := filepath.Join(containersDir, containerID)
	upperPath := filepath.Join(containerDir, "upper")
	workPath := filepath.Join(containerDir, "work")
	mergedPath := filepath.Join(containerDir, "merged")

	for _, path := range []string{upperPath, workPath, mergedPath} {
		if err := os.MkdirAll(path, 0755); err != nil {
			return "", fmt.Errorf("create %s dir: %w", path, err)
		}
	}

	options := fmt.Sprintf(
		"lowerdir=%s,upperdir=%s,workdir=%s",
		lowerdir,
		upperPath,
		workPath,
	)

	if err := unix.Mount("overlay", mergedPath, "overlay", 0, options); err != nil {
		return "", fmt.Errorf("mount overlay: %w", err)
	}

	return mergedPath, nil
}

// Unmount lazily detaches the overlay merged mount for containerID.
// MNT_DETACH allows unmounting even if files inside are still open,
// which is safe here because the container process has already exited.
func Unmount(containerID string) error {
	containerDir := filepath.Join(containersDir, containerID)
	mergedPath := filepath.Join(containerDir, "merged")
	if err := unix.Unmount(mergedPath, unix.MNT_DETACH); err != nil {
		return fmt.Errorf("unmount overlay: %w", err)
	}
	return nil
}

// MergedPath returns the path to the overlay merged directory for containerID.
// Other packages use this as the rootfs path when mounting volumes or
// performing pivot_root.
func MergedPath(containerID string) string {
	return filepath.Join(containersDir, containerID, "merged")
}
