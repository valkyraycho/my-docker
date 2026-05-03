//go:build linux

package volume

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const volumesDir = "/var/lib/mydocker/volumes"

// EnsureNamed creates the data directory for a named volume if it doesn't
// already exist, then returns its path. Idempotent: safe to call on every run.
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

// NamedPath returns the on-disk data path for a named volume.
// The "_data" suffix mirrors Docker's own volume layout under
// /var/lib/docker/volumes/<name>/_data.
func NamedPath(name string) string {
	return filepath.Join(volumesDir, name, "_data")
}
