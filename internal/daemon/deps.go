//go:build linux

package daemon

import (
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
type Deps struct {
	Registry   *state.Registry
	ImageStore ImageResolver
	StartInit  func(c *state.Container) error
}
