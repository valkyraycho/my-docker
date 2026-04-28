//go:build linux

package volume

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const volumesDir = "/var/lib/mydocker/volumes"

func EnsureNamed(name string) (string, error) {
	if strings.HasPrefix(name, ".") {
		return "", fmt.Errorf("volume name %q cannot start with '.'", name)
	}
	dataDir := NamedPath(name)

	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", dataDir, err)
	}
	return dataDir, nil
}

func NamedPath(name string) string {
	return filepath.Join(volumesDir, name, "_data")
}
