package image

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/valkyraycho/my-docker/internal/registry"
)

// ErrImageNotFound is returned by Resolve when the image has not been pulled.
var ErrImageNotFound = errors.New("image not found")

// Resolve looks up a pulled image by its ref (e.g. "alpine:3.19") and returns
// the ordered list of extracted layer directory paths suitable for passing to
// overlay mount. Layers are returned top-first so the caller can use them
// directly as OverlayFS lowerdir entries.
func (s *Store) Resolve(ref string) ([]string, error) {
	repo, tag := parseRef(ref)

	imageDir := s.ImageDir(repo, tag)
	manifestBytes, err := os.ReadFile(filepath.Join(imageDir, "manifest.json"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("image %q not found: %w", ref, ErrImageNotFound)
		}
		return nil, fmt.Errorf("read manifest for %s: %w", ref, err)
	}

	var manifest registry.Manifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return nil, fmt.Errorf("unmarshal manifest: %w", err)
	}

	result := make([]string, len(manifest.Layers))
	for i, layer := range manifest.Layers {
		result[len(manifest.Layers)-i-1] = digestPath(layer.Digest)
	}

	return result, nil
}
