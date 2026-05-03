//go:build linux

package daemon

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/valkyraycho/my-docker/internal/api"
	"github.com/valkyraycho/my-docker/internal/image"
	"github.com/valkyraycho/my-docker/internal/network"
	"github.com/valkyraycho/my-docker/internal/state"
	"github.com/valkyraycho/my-docker/internal/volume"
)

// handleContainerCreate implements POST /containers/create. It decodes a
// ContainerCreateRequest, resolves the image to its layer paths, parses
// volume and port specs, allocates a random container ID, persists the
// container to the registry, and responds 201 with a ContainerCreateResponse.
// Returns 400 for malformed input, 404 if the image is not found, 500 otherwise.
func (d *Deps) handleContainerCreate(w http.ResponseWriter, r *http.Request) {
	var req api.ContainerCreateRequest

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request: %w", err))
		return
	}

	if req.Image == "" {
		writeError(w, http.StatusBadRequest, errors.New("image is required"))
		return
	}

	layers, err := d.ImageStore.Resolve(req.Image)
	if err != nil {
		if errors.Is(err, image.ErrImageNotFound) {
			writeError(w, http.StatusNotFound, fmt.Errorf("image %q not found: %w", req.Image, err))
			return
		}

		writeError(w, http.StatusInternalServerError, fmt.Errorf("resolve image: %w", err))
		return
	}

	volumes := make([]*volume.Spec, 0, len(req.HostConfig.Binds))
	for _, b := range req.HostConfig.Binds {
		spec, err := volume.Parse(b)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("parse volume spec: %w", err))
			return
		}
		volumes = append(volumes, spec)
	}

	ports, err := parsePortBindings(req.HostConfig.PortBindings)
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("parse port bindings: %w", err))
		return
	}

	id := newContainerID()

	container := state.Container{
		ID:        id,
		Image:     req.Image,
		Layers:    layers,
		Command:   req.Cmd,
		Status:    state.StatusCreated,
		CreatedAt: time.Now(),
		Volumes:   volumes,
		Ports:     ports,
	}

	if err := d.Registry.Add(&container); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("add container: %w", err))
		return
	}

	writeJSON(w, http.StatusCreated, api.ContainerCreateResponse{
		ID:       id,
		Warnings: []string{},
	})
}

// newContainerID returns a 12-character lowercase hex string derived from 6
// cryptographically random bytes. Panics if the OS CSPRNG is unavailable.
func newContainerID() string {
	buf := make([]byte, 6)

	_, err := rand.Read(buf)
	if err != nil {
		panic(err)
	}
	return hex.EncodeToString(buf)
}

// writeJSON sets Content-Type to application/json, writes statusCode, and
// encodes v as JSON into the response body.
func writeJSON(w http.ResponseWriter, statusCode int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError writes an api.ErrorResponse JSON body with the given status code.
func writeError(w http.ResponseWriter, statusCode int, err error) {
	writeJSON(w, statusCode, api.ErrorResponse{Message: err.Error()})
}

// statusForError maps known sentinel errors to HTTP status codes,
// falling back to 500 for anything unexpected.
func statusForError(err error) int {
	switch {
	case errors.Is(err, state.ErrNotFound):
		return http.StatusNotFound
	default:
		return http.StatusInternalServerError
	}
}

// parsePortBindings translates Docker's wire-format port binding map
// into our internal network.PortSpec slice.
//
// Docker's shape:
//
//	{ "80/tcp": [{"HostIp":"", "HostPort":"8080"}] }
//
// Expansions:
//   - Map key is "<container_port>[/<proto>]"; proto defaults to "tcp".
//   - Value is a list of host bindings; we honor only the first one.
//   - Empty host list (len 0) is "publish to random free host port"
//     in Docker. We skip for M9 and error on it.
//   - Non-tcp protocols (udp, sctp) are rejected; our network stack
//     only wires TCP right now.
func parsePortBindings(bindings map[string][]api.PortBinding) ([]*network.PortSpec, error) {
	specs := make([]*network.PortSpec, 0, len(bindings))
	for key, hosts := range bindings {
		portStr, proto, _ := strings.Cut(key, "/")
		if proto == "" {
			proto = "tcp"
		}
		if proto != "tcp" {
			return nil, fmt.Errorf("port %s: only tcp supported", key)
		}

		containerPort, err := strconv.Atoi(portStr)
		if err != nil {
			return nil, fmt.Errorf("parse port %s: %w", portStr, err)
		}

		if containerPort < 1 || containerPort > 65535 {
			return nil, fmt.Errorf("invalid container port %d, must be between 1 and 65535", containerPort)
		}

		if len(hosts) == 0 {
			return nil, fmt.Errorf("port %s: empty host bindings", key)
		}
		if len(hosts) > 1 {
			return nil, fmt.Errorf("port %s: multiple bindings not supported", key)
		}

		hostPort, err := strconv.Atoi(hosts[0].HostPort)
		if err != nil {
			return nil, fmt.Errorf("parse host port %s: %w", hosts[0].HostPort, err)
		}

		if hostPort < 1 || hostPort > 65535 {
			return nil, fmt.Errorf("invalid host port %d, must be between 1 and 65535", hostPort)
		}

		specs = append(specs, &network.PortSpec{
			HostPort:      hostPort,
			ContainerPort: containerPort,
			Protocol:      proto,
		})
	}
	return specs, nil
}
