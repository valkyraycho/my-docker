//go:build linux

package daemon

import (
	"cmp"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/valkyraycho/my-docker/internal/api"
	"github.com/valkyraycho/my-docker/internal/container"
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
		Env:       req.Env,
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

func (d *Deps) handleContainerStart(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, errors.New("id is required"))
		return
	}

	c, err := d.Registry.Find(id)
	if err != nil {
		writeError(w, statusForError(err), err)
		return
	}

	if c.Status == state.StatusRunning {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	if err := d.StartInit(c); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("start: %w", err))
		return
	}

	if err := d.Registry.Update(c); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("persist state: %w", err))
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (d *Deps) handleContainerList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("all")
	all := q == "1" || q == "true"

	containers, err := d.Registry.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("list containers: %w", err))
		return
	}

	out := make([]api.ContainerSummary, 0, len(containers))
	for _, c := range containers {
		if !all && c.Status != state.StatusRunning {
			continue
		}

		out = append(out, toSummary(c))
	}

	slices.SortFunc(out, func(a, b api.ContainerSummary) int {
		return cmp.Compare(b.Created, a.Created)
	})

	writeJSON(w, http.StatusOK, out)
}

func (d *Deps) handleContainerInspect(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, errors.New("id is required"))
		return
	}

	c, err := d.Registry.Find(id)
	if err != nil {
		writeError(w, statusForError(err), err)
		return
	}

	writeJSON(w, http.StatusOK, toInspect(c))
}

func toSummary(c *state.Container) api.ContainerSummary {
	return api.ContainerSummary{
		ID:      c.ID,
		Image:   c.Image,
		Command: strings.Join(c.Command, " "),
		Created: c.CreatedAt.UTC().Unix(),
		State:   c.Status,
		Status:  humanStatus(c),
		Ports:   toPorts(c),
	}
}

func toInspect(c *state.Container) api.ContainerInspect {
	var path string
	var args []string

	if len(c.Command) > 0 {
		path = c.Command[0]
		args = c.Command[1:]
	}

	return api.ContainerInspect{
		ID:        c.ID,
		Created:   c.CreatedAt.UTC().Format(time.RFC3339),
		Path:      path,
		Args:      args,
		State:     toState(c),
		Image:     c.Image,
		Env:       c.Env,
		Mounts:    toMounts(c),
		Ports:     toPorts(c),
		IPAddress: c.IP,
	}
}

func toState(c *state.Container) api.ContainerState {
	return api.ContainerState{
		Status:     c.Status,
		Running:    c.Status == state.StatusRunning,
		Pid:        c.PID,
		ExitCode:   c.ExitCode,
		StartedAt:  rfc3339OrZero(c.StartedAt),
		FinishedAt: rfc3339OrZero(c.FinishedAt),
	}
}

func rfc3339OrZero(t time.Time) string {
	if t.IsZero() {
		return time.Time{}.UTC().Format(time.RFC3339)
	}
	return t.UTC().Format(time.RFC3339)
}

func toPorts(c *state.Container) []api.Port {
	out := make([]api.Port, 0, len(c.Ports))
	for _, p := range c.Ports {
		out = append(out, api.Port{
			PrivatePort: p.ContainerPort,
			PublicPort:  p.HostPort,
			Type:        p.Protocol,
		})
	}
	return out
}

func toMounts(c *state.Container) []api.MountPoint {
	out := make([]api.MountPoint, 0, len(c.Volumes))
	for _, v := range c.Volumes {
		mtype := "volume"
		if v.Kind == volume.Bind {
			mtype = "bind"
		}

		out = append(out, api.MountPoint{
			Type:        mtype,
			Source:      v.Source,
			Destination: v.Target,
			RW:          !v.ReadOnly,
		})
	}
	return out
}
func humanStatus(c *state.Container) string {
	switch c.Status {
	case state.StatusCreated:
		return "Created"
	case state.StatusRunning:
		return "Up " + humanDuration(time.Since(c.StartedAt))
	case state.StatusExited:
		return fmt.Sprintf("Exited (%d) %s ago", c.ExitCode, humanDuration(time.Since(c.FinishedAt)))
	}
	return c.Status
}

func humanDuration(d time.Duration) string {
	if d < time.Second {
		return "Less than a second"
	}

	if d < time.Minute {
		return fmt.Sprintf("%d seconds", int(d/time.Second))
	}

	if d < time.Hour {
		return fmt.Sprintf("%d minutes", int(d/time.Minute))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%d hours", int(d/time.Hour))
	}
	return fmt.Sprintf("%d days", int(d/(24*time.Hour)))
}

// handleContainerStop implements POST /containers/{id}/stop.
//
// Query params:
//   t=<seconds> — grace period between SIGTERM and SIGKILL. Default
//     container.DefaultStopTimeout. A zero or unset value uses the
//     default; invalid values are rejected with 400.
//
// Returns 204 on successful stop, 304 Not Modified if the container
// wasn't running, 404 if unknown, 500 on runtime failure.
func (d *Deps) handleContainerStop(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, errors.New("id is required"))
		return
	}

	timeout := container.DefaultStopTimeout
	if raw := r.URL.Query().Get("t"); raw != "" {
		secs, err := strconv.Atoi(raw)
		if err != nil || secs < 0 {
			writeError(w, http.StatusBadRequest, fmt.Errorf("invalid timeout %q", raw))
			return
		}
		timeout = time.Duration(secs) * time.Second
	}

	c, err := d.Registry.Find(id)
	if err != nil {
		writeError(w, statusForError(err), err)
		return
	}

	if c.Status != state.StatusRunning {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	if err := d.StopInit(c, timeout); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("stop: %w", err))
		return
	}

	if err := d.Registry.Update(c); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("persist state: %w", err))
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// handleContainerKill implements POST /containers/{id}/kill.
// Immediate SIGKILL — no grace, no timeout.
//
// Returns 204 on success, 304 Not Modified if not running, 404 if unknown.
func (d *Deps) handleContainerKill(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, errors.New("id is required"))
		return
	}

	c, err := d.Registry.Find(id)
	if err != nil {
		writeError(w, statusForError(err), err)
		return
	}

	if c.Status != state.StatusRunning {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	if err := d.KillInit(c); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("kill: %w", err))
		return
	}

	if err := d.Registry.Update(c); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("persist state: %w", err))
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// handleContainerRemove implements DELETE /containers/{id}.
//
// Query params:
//   force=1 (or true) — stop the container first if it's running.
//     Without force, a running container produces 409 Conflict.
//
// Returns 204 on success, 404 if unknown, 409 if running without force,
// 500 on teardown failure.
func (d *Deps) handleContainerRemove(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, errors.New("id is required"))
		return
	}

	q := r.URL.Query().Get("force")
	force := q == "1" || q == "true"

	c, err := d.Registry.Find(id)
	if err != nil {
		writeError(w, statusForError(err), err)
		return
	}

	if c.Status == state.StatusRunning {
		if !force {
			writeError(w, http.StatusConflict,
				errors.New("container is running; stop it first or use force"))
			return
		}
		if err := d.StopInit(c, container.DefaultStopTimeout); err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Errorf("stop before remove: %w", err))
			return
		}
		// Persist the stop so a subsequent RemoveInit failure leaves a
		// consistent registry view instead of a ghost "running" entry.
		if err := d.Registry.Update(c); err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Errorf("persist stop: %w", err))
			return
		}
	}

	if err := d.RemoveInit(c); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("teardown: %w", err))
		return
	}

	if err := d.Registry.Remove(c.ID); err != nil {
		writeError(w, statusForError(err), err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
