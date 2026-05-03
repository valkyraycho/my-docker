package image

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/valkyraycho/my-docker/internal/registry"
)

// FetchBlob downloads the blob identified by expectedDigest from the registry
// and stores it at BlobPath. It is a no-op if the blob is already cached.
// The download is written to a ".tmp" file first and renamed on success;
// the SHA-256 digest is verified before the rename so a partial download is
// never promoted to the cache.
func (s *Store) FetchBlob(client *registry.Client, repo, expectedDigest string) error {
	if s.HasBlob(expectedDigest) {
		return nil
	}

	algo, hexWant, ok := strings.Cut(expectedDigest, ":")
	if !ok {
		return fmt.Errorf("digest malformed: %s", expectedDigest)
	}

	if algo != "sha256" {
		return fmt.Errorf("digest algorithm is not sha256: %s", algo)
	}

	body, err := client.GetBlob(repo, expectedDigest)
	if err != nil {
		return fmt.Errorf("get blob: %w", err)
	}
	defer body.Close()

	blobPath := s.BlobPath(expectedDigest)
	tmpBlobPath := blobPath + ".tmp"

	if err := os.MkdirAll(filepath.Dir(blobPath), 0755); err != nil {
		return fmt.Errorf("mkdir blob %s: %w", blobPath, err)
	}

	f, err := os.Create(tmpBlobPath)
	if err != nil {
		return fmt.Errorf("create temp blob path %s: %w", tmpBlobPath, err)
	}
	defer f.Close()

	hasher := sha256.New()
	if _, err := io.Copy(f, io.TeeReader(body, hasher)); err != nil {
		f.Close()
		os.Remove(tmpBlobPath)
		return fmt.Errorf("stream blob: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close file %s: %w", tmpBlobPath, err)
	}

	hexGot := hex.EncodeToString(hasher.Sum(nil))
	if hexGot != hexWant {
		os.Remove(tmpBlobPath)
		return fmt.Errorf("digest mismatch for %s: got sha256:%s", expectedDigest, hexGot)
	}

	if err := os.Rename(tmpBlobPath, blobPath); err != nil {
		return fmt.Errorf("rename %s to %s: %w", tmpBlobPath, blobPath, err)
	}

	return nil
}

// ExtractLayer decompresses the gzipped-tar blob for digest into LayerPath.
// It is a no-op if the layer directory already exists. Extraction goes into a
// ".tmp" sibling directory that is renamed atomically on success; a deferred
// cleanup removes it if any error occurs mid-extraction.
func (s *Store) ExtractLayer(digest string) (retErr error) {

	if s.HasLayer(digest) {
		return nil
	}

	blobPath := s.BlobPath(digest)
	destLayerPath := s.LayerPath(digest)
	tmpDestLayerPath := destLayerPath + ".tmp"

	defer func() {
		if retErr != nil {
			os.RemoveAll(tmpDestLayerPath)
		}
	}()

	if err := os.MkdirAll(tmpDestLayerPath, 0755); err != nil {
		return fmt.Errorf("mkdir temp dest layer %s: %w", tmpDestLayerPath, err)
	}

	f, err := os.Open(blobPath)
	if err != nil {
		return fmt.Errorf("open blob path %s: %w", blobPath, err)
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("create gzip reader: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}

		if err != nil {
			return fmt.Errorf("read next tar header: %w", err)
		}

		target := filepath.Join(tmpDestLayerPath, hdr.Name)
		if !strings.HasPrefix(target, filepath.Clean(tmpDestLayerPath)+string(os.PathSeparator)) {
			return fmt.Errorf("tar entry escapes dest: %s", hdr.Name)
		}

		if strings.HasPrefix(filepath.Base(hdr.Name), ".wh.") {
			continue
		}

		mode := hdr.FileInfo().Mode()

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, mode.Perm()); err != nil {
				return fmt.Errorf("mkdir tar header TypeDir %s: %w", target, err)
			}
		case tar.TypeReg:
			targetDir := filepath.Dir(target)
			if err := os.MkdirAll(targetDir, 0755); err != nil {
				return fmt.Errorf("mkdir tar header TypeReg target Dir %s: %w", targetDir, err)
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode.Perm())
			if err != nil {
				return fmt.Errorf("cannot open file %s: %w", target, err)
			}

			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return fmt.Errorf("write %s: %w", target, err)
			}
			f.Close()
		case tar.TypeSymlink:
			targetDir := filepath.Dir(target)
			if err := os.MkdirAll(targetDir, 0755); err != nil {
				return fmt.Errorf("mkdir parent for symlink %s: %w", targetDir, err)
			}

			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return fmt.Errorf("symlink %s to %s: %w", hdr.Linkname, target, err)
			}
		case tar.TypeLink:
			targetDir := filepath.Dir(target)
			if err := os.MkdirAll(targetDir, 0755); err != nil {
				return fmt.Errorf("mkdir parent for hardlink %s: %w", targetDir, err)
			}
			linkSrc := filepath.Join(tmpDestLayerPath, hdr.Linkname)
			if err := os.Link(linkSrc, target); err != nil {
				return fmt.Errorf("link %s to %s: %w", linkSrc, target, err)
			}
		default:
			continue
		}

	}
	if err := os.Rename(tmpDestLayerPath, destLayerPath); err != nil {
		return fmt.Errorf("rename %s to %s: %w", tmpDestLayerPath, destLayerPath, err)
	}
	return nil
}
