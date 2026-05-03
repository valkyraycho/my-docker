// Package image implements image pulling, content-addressable layer storage,
// and image ref resolution for mydocker. Pulled blobs are stored under
// /var/lib/mydocker/blobs, extracted layers under /var/lib/mydocker/layers,
// and image metadata (manifest + config) under /var/lib/mydocker/images.
package image

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	root      = "/var/lib/mydocker"
	blobsDir  = root + "/blobs"
	layersDir = root + "/layers"
	imagesDir = root + "/images"
)

// Store is the content-addressable image store. It is a zero-value struct;
// use New to obtain one. All paths are derived from the package-level constants.
type Store struct{}

// New returns a new Store. No I/O is performed; call EnsureDirs before writing.
func New() *Store {
	return &Store{}
}

// EnsureDirs creates the blobs, layers, and images root directories if they
// do not already exist. Must be called before any write operation.
func (s *Store) EnsureDirs() error {
	for _, d := range []string{blobsDir, layersDir, imagesDir} {
		if err := os.MkdirAll(d, 0755); err != nil {
			return fmt.Errorf("create dir %s: %w", d, err)
		}
	}
	return nil
}

func digestPath(digest string) string {
	return strings.ReplaceAll(digest, ":", "_")
}

func imageKey(repo, tag string) string {
	repo = strings.ReplaceAll(repo, "/", "_")
	return repo + "_" + tag
}

// BlobPath returns the on-disk path for the raw compressed blob identified by
// digest (e.g. "sha256:abc123").
func (s *Store) BlobPath(digest string) string {
	return filepath.Join(blobsDir, digestPath(digest), "data")
}

// HasBlob reports whether the blob for the given digest is already cached.
func (s *Store) HasBlob(digest string) bool {
	_, err := os.Stat(s.BlobPath(digest))
	return err == nil
}

// LayerPath returns the on-disk directory where a layer's tar contents are
// extracted, keyed by digest.
func (s *Store) LayerPath(digest string) string {
	return filepath.Join(layersDir, digestPath(digest))
}

// HasLayer reports whether the layer for the given digest has already been
// extracted.
func (s *Store) HasLayer(digest string) bool {
	_, err := os.Stat(s.LayerPath(digest))
	return err == nil
}

// ImageDir returns the directory that holds manifest.json and config.json for
// a pulled image identified by repo and tag.
func (s *Store) ImageDir(repo, tag string) string {
	return filepath.Join(imagesDir, imageKey(repo, tag))
}

// SaveImage writes manifest.json and config.json into the image directory for
// the given repo and tag, creating the directory if needed.
func (s *Store) SaveImage(repo, tag string, manifest, config []byte) error {
	dir := s.ImageDir(repo, tag)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create image dir %s: %w", dir, err)
	}

	manifestFile := filepath.Join(dir, "manifest.json")
	if err := os.WriteFile(manifestFile, manifest, 0644); err != nil {
		return fmt.Errorf("create image manifest %s: %w", manifestFile, err)
	}

	configFile := filepath.Join(dir, "config.json")
	if err := os.WriteFile(configFile, config, 0644); err != nil {
		return fmt.Errorf("create image config %s: %w", configFile, err)
	}

	return nil
}
