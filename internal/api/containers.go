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

// ContainerSummary is one entry returned by GET /containers/json.
// Keep field names identical to Docker's wire format: any cross-tool
// script that parses `docker ps -a --format='{{json .}}'` should
// parse mydocker's output with zero changes.
type ContainerSummary struct {
	ID      string `json:"Id"`
	Image   string `json:"Image"`
	Command string `json:"Command"` // space-joined argv, single string for display
	Created int64  `json:"Created"` // Unix timestamp in seconds
	State   string `json:"State"`   // "created" | "running" | "exited"
	Status  string `json:"Status"`  // human readable: "Up 5 minutes", "Exited (0) 2 hours ago"
	Ports   []Port `json:"Ports"`   // always non-null; empty slice if none
}

// Port is Docker's flat port-summary shape used inside ContainerSummary
// and NetworkSettings. Distinct from PortBinding (which appears in
// request bodies). Docker uses different shapes for these on purpose:
// bindings are inputs, Ports is a view of resolved state.
type Port struct {
	IP          string `json:"IP,omitempty"`         // host IP, empty means "all"
	PrivatePort int    `json:"PrivatePort"`          // container port
	PublicPort  int    `json:"PublicPort,omitempty"` // host port, 0 when not published
	Type        string `json:"Type"`                 // "tcp" | "udp" (we only emit tcp for now)
}

// ContainerInspect is the response body for GET /containers/{id}/json.
// More detailed than ContainerSummary — one container's full story.
//
// Simplified from Docker's schema: IP lives at top-level IPAddress
// instead of NetworkSettings.Networks["bridge"].IPAddress, and Env
// lives top-level instead of Config.Env. When/if we add multi-network
// or multi-config support, these flatten positions move under proper
// nested objects.
type ContainerInspect struct {
	ID        string         `json:"Id"`
	Created   string         `json:"Created"`   // RFC3339 timestamp
	Path      string         `json:"Path"`      // argv[0] — the program to exec
	Args      []string       `json:"Args"`      // argv[1:] — program arguments
	State     ContainerState `json:"State"`     // nested lifecycle view
	Image     string         `json:"Image"`     // image reference, e.g. "alpine:3.19"
	Env       []string       `json:"Env"`       // simplified: top-level, not under Config
	Mounts    []MountPoint   `json:"Mounts"`    // volumes attached to this container
	Ports     []Port         `json:"Ports"`     // same Port shape as in list
	IPAddress string         `json:"IPAddress"` // simplified: top-level, not under NetworkSettings
}

// ContainerState is the nested State object inside ContainerInspect.
// Running is a redundant-but-useful quick-check boolean; callers that
// only need "is it alive" don't have to string-match Status.
type ContainerState struct {
	Status     string `json:"Status"`     // "created" | "running" | "exited"
	Running    bool   `json:"Running"`    // Status == "running"
	Pid        int    `json:"Pid"`        // process id, 0 if not running
	ExitCode   int    `json:"ExitCode"`   // last exit code, 0 if never exited
	StartedAt  string `json:"StartedAt"`  // RFC3339 or zero-time "0001-01-01T00:00:00Z"
	FinishedAt string `json:"FinishedAt"` // RFC3339 or zero-time
}

// MountPoint mirrors Docker's Mounts[] entry shape. "Type" is the
// volume kind ("bind" or "volume"); "Source" is either a host path
// (for bind) or a volume name (for named/anonymous volumes).
type MountPoint struct {
	Type        string `json:"Type"`        // "bind" | "volume"
	Source      string `json:"Source"`      // host path or volume name
	Destination string `json:"Destination"` // path inside the container
	RW          bool   `json:"RW"`          // true if read-write (opposite of ReadOnly)
}
