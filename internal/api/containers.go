// Package api defines the wire types shared between the daemon and the client.
// Each type corresponds to an HTTP route; field names and JSON tags deliberately
// match Docker's Engine API so that the two can be compared and eventually
// made interoperable.
package api

// ContainerCreateRequest is the JSON body for POST /containers/create.
//
// Field names match Docker's Engine API exactly (CamelCase on the wire,
// so `Image` not `image`). See docs.docker.com/engine/api/v1.43/#tag/Container.
// We implement a subset: enough for images we can run, ignoring the
// long tail Docker supports.
type ContainerCreateRequest struct {
	Image string   `json:"Image"`         // e.g. "library/alpine:3.19". Required.
	Cmd   []string `json:"Cmd,omitempty"` // argv override; empty = use image default (not yet implemented)
	Env   []string `json:"Env,omitempty"` // VAR=value entries

	// HostConfig holds settings about how the container is wired into
	// the host. Kept nested to match Docker's schema even though we
	// only implement a small subset of its fields today.
	HostConfig HostConfig `json:"HostConfig"`
}

// HostConfig is the subset of Docker's HostConfig we implement.
type HostConfig struct {
	// Binds is a list of volume specifications in Docker format:
	//   "/host/path:/container/path"          (bind mount, rw)
	//   "volname:/container/path"             (named volume)
	//   "/container/path"                     (anonymous volume)
	Binds []string `json:"Binds,omitempty"`

	// PortBindings maps container ports to host publishers.
	// Key format: "<port>/<proto>", e.g. "80/tcp".
	// Value is a list of host bindings; we support a single entry.
	PortBindings map[string][]PortBinding `json:"PortBindings,omitempty"`
}

// PortBinding describes one host-side publisher for a container port.
// HostIP is optional; empty means "bind on all host interfaces" (0.0.0.0).
type PortBinding struct {
	HostIP   string `json:"HostIp,omitempty"`   // Docker's field is "HostIp", not "HostIP" — match it exactly
	HostPort string `json:"HostPort,omitempty"` // string, not int, matching Docker
}

// ContainerCreateResponse is the 201 Created body.
// Warnings is always present (possibly empty) to match Docker's schema;
// we don't emit any warnings yet but the field reserves space for future
// "using deprecated option" style notices.
type ContainerCreateResponse struct {
	ID       string   `json:"Id"`       // Docker's field is "Id" (two letters), not "ID"
	Warnings []string `json:"Warnings"` // always non-null, even when empty
}
