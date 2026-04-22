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

type Store struct{}

func New() *Store {
	return &Store{}
}

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

func (s *Store) BlobPath(digest string) string {
	return filepath.Join(blobsDir, digestPath(digest), "data")
}

func (s *Store) HasBlob(digest string) bool {
	_, err := os.Stat(s.BlobPath(digest))
	return err == nil
}

func (s *Store) LayerPath(digest string) string {
	return filepath.Join(layersDir, digestPath(digest))
}

func (s *Store) HasLayer(digest string) bool {
	_, err := os.Stat(s.LayerPath(digest))
	return err == nil
}

func (s *Store) ImageDir(repo, tag string) string {
	return filepath.Join(imagesDir, imageKey(repo, tag))
}

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
