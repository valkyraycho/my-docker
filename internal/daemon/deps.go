//go:build linux

package daemon

import (
	"time"

	"github.com/valkyraycho/my-docker/internal/state"
)

// ImageResolver looks up the ordered list of layer paths for a named image.
// The daemon calls Resolve before creating a container; tests can inject a
// fake that returns fixed paths without touching the real image store.
type ImageResolver interface {
	Resolve(ref string) ([]string, error)
}

// Deps is the dependency-injection struct passed to every handler method.
// Grouping runtime dependencies here lets tests swap real implementations
// for fakes (e.g. an in-memory registry, a stubbed ImageResolver) without
// changing handler logic.
//
// Lifecycle function fields (StartInit, StopInit, KillInit, RemoveInit) are
// kept as plain func types rather than collected behind one interface. That
// costs four fields but keeps each operation independently mockable in tests
// and avoids forcing a single struct to implement every method during the
// M9 transition. If the field count grows beyond ~6, promoting to an
// interface is the right refactor.
type Deps struct {
	Registry   *state.Registry
	ImageStore ImageResolver

	// StartInit forks the container init process and wires up namespaces,
	// cgroup, network, and volumes. Mutates c with runtime fields on
	// success. Handler persists via Registry.Update.
	StartInit func(c *state.Container) error

	// StopInit signals the running init (SIGTERM then SIGKILL after
	// timeout). Mutates c to Exited. Handler persists via Registry.Update.
	StopInit func(c *state.Container, timeout time.Duration) error

	// KillInit is StopInit's impatient cousin — SIGKILL immediately, no
	// grace period. Used by POST /containers/{id}/kill.
	KillInit func(c *state.Container) error

	// RemoveInit tears down all host resources (overlay, cgroup, volumes,
	// network) for a stopped container. Pre-condition: container is
	// already Exited — handler enforces with 409 if still running.
	RemoveInit func(c *state.Container) error
}
