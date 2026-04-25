package image

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/valkyraycho/my-docker/internal/registry"
)

var ErrImageNotFound = errors.New("image not found")

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
