//go:build linux

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

func Unmount(containerID string) error {
	containerDir := filepath.Join(containersDir, containerID)
	mergedPath := filepath.Join(containerDir, "merged")
	if err := unix.Unmount(mergedPath, unix.MNT_DETACH); err != nil {
		return fmt.Errorf("unmount overlay: %w", err)
	}
	return nil
}

func MergedPath(containerID string) string {
	return filepath.Join(containersDir, containerID, "merged")
}
