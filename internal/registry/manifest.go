// Package registry provides an HTTP client for the OCI Distribution
// (Docker Registry HTTP API V2) protocol, including bearer-token auth,
// manifest fetching, and blob streaming.
package registry

// Media-type constants for OCI and Docker manifest wire formats.
// The registry returns one of these in the Content-Type header so the
// caller knows whether it received a single-platform manifest or a
// multi-platform index.
const (
	MediaTypeOCIManifest    = "application/vnd.oci.image.manifest.v1+json"
	MediaTypeOCIIndex       = "application/vnd.oci.image.index.v1+json"
	MediaTypeDockerManifest = "application/vnd.docker.distribution.manifest.v2+json"
	MediaTypeDockerIndex    = "application/vnd.docker.distribution.manifest.list.v2+json"
)

// Descriptor is a content-addressable reference to a blob or manifest. It
// appears both as a layer entry inside a Manifest and as a per-platform entry
// inside an Index.
type Descriptor struct {
	MediaType string    `json:"mediaType"`
	Digest    string    `json:"digest"`
	Size      int64     `json:"size"`
	Platform  *Platform `json:"platform,omitempty"`
}

// Platform describes the OS and CPU architecture a manifest targets.
type Platform struct {
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
	Variant      string `json:"variant,omitempty"`
}

// Manifest is the wire format for a single-platform OCI or Docker image
// manifest (schemaVersion 2). It lists the image config blob and the ordered
// set of layer blobs.
type Manifest struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType"`
	Config        Descriptor   `json:"config"`
	Layers        []Descriptor `json:"layers"`
}

// Index is the wire format for a multi-platform manifest list (OCI image index
// or Docker manifest list). Each entry in Manifests points to a
// single-platform Manifest selected by Platform.
type Index struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType"`
	Manifests     []Descriptor `json:"manifests"`
}
