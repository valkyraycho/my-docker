package image

import (
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/valkyraycho/my-docker/internal/registry"
)

// DefaultRegistry is the Docker Hub registry used when no host is specified in
// an image ref.
const DefaultRegistry = "registry-1.docker.io"

// parseRef splits a human-readable image ref (e.g. "alpine:3.19") into a
// fully-qualified repo and tag. Bare names like "alpine" are expanded to
// "library/alpine"; missing tags default to "latest".
func parseRef(ref string) (string, string) {
	repo, tag, ok := strings.Cut(ref, ":")
	if !ok {
		tag = "latest"
	}

	if !strings.Contains(repo, "/") {
		repo = "library/" + repo
	}

	return repo, tag
}

// Pull orchestrates a full image pull for the given ref. It resolves the ref,
// fetches the manifest (following an index to select the current platform when
// needed), downloads and verifies each blob, extracts layers into the cache,
// and saves image metadata to disk.
func (s *Store) Pull(client *registry.Client, ref string) error {
	repo, tag := parseRef(ref)
	if err := s.EnsureDirs(); err != nil {
		return fmt.Errorf("ensure directory exists: %w", err)
	}

	mediaType, manifestBytes, err := client.GetManifest(repo, tag)
	if err != nil {
		return fmt.Errorf("get manifest %s:%s: %w", repo, tag, err)
	}

	switch mediaType {
	case registry.MediaTypeOCIIndex, registry.MediaTypeDockerIndex:
		var index registry.Index
		if err := json.Unmarshal(manifestBytes, &index); err != nil {
			return fmt.Errorf("unmarshal index: %w", err)
		}
		entry := matchPlatform(&index)
		if entry == nil {
			return fmt.Errorf("no manifest for %s/%s in %s:%s", runtime.GOOS, runtime.GOARCH, repo, tag)
		}

		mediaType, manifestBytes, err = client.GetManifest(repo, entry.Digest)
		if err != nil {
			return fmt.Errorf("get platform manifest %s: %w", entry.Digest, err)
		}
	}

	var manifest registry.Manifest
	switch mediaType {
	case registry.MediaTypeOCIManifest, registry.MediaTypeDockerManifest:
		if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
			return fmt.Errorf("unmarshal manifest: %w", err)
		}
	default:
		return fmt.Errorf("unexpected media type: %s", mediaType)
	}

	if err := s.FetchBlob(client, repo, manifest.Config.Digest); err != nil {
		return fmt.Errorf("fetch config: %w", err)
	}

	configBytes, err := os.ReadFile(s.BlobPath(manifest.Config.Digest))
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}

	for _, layer := range manifest.Layers {
		fmt.Fprintf(os.Stderr, "fetching %s\n", layer.Digest)
		if err := s.FetchBlob(client, repo, layer.Digest); err != nil {
			return fmt.Errorf("fetch layer %s: %w", layer.Digest, err)
		}

		if err := s.ExtractLayer(layer.Digest); err != nil {
			return fmt.Errorf("extract layer %s: %w", layer.Digest, err)
		}
	}

	if err := s.SaveImage(repo, tag, manifestBytes, configBytes); err != nil {
		return fmt.Errorf("save image: %w", err)
	}
	return nil
}

// matchPlatform finds the manifest descriptor in index that matches the current
// runtime OS and architecture (e.g. linux/amd64). Returns nil if no match.
func matchPlatform(index *registry.Index) *registry.Descriptor {
	for i := range index.Manifests {
		m := &index.Manifests[i]
		if m.Platform != nil && m.Platform.OS == runtime.GOOS && m.Platform.Architecture == runtime.GOARCH {
			return m
		}
	}
	return nil
}
