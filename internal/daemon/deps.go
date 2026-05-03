//go:build linux

package daemon

import "github.com/valkyraycho/my-docker/internal/state"

type Deps struct {
	Registry *state.Registry
}
