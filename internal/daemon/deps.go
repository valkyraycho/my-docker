//go:build linux

package daemon

import (
	"github.com/valkyraycho/my-docker/internal/state"
)

type ImageResolver interface {
	Resolve(ref string) ([]string, error)
}

type Deps struct {
	Registry   *state.Registry
	ImageStore ImageResolver
}
