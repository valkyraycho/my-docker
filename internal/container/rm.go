//go:build linux

package container

import (
	"errors"
	"fmt"

	"github.com/valkyraycho/my-docker/internal/cgroup"
	"github.com/valkyraycho/my-docker/internal/network"
	"github.com/valkyraycho/my-docker/internal/overlay"
	"github.com/valkyraycho/my-docker/internal/state"
	"github.com/valkyraycho/my-docker/internal/volume"
)

// Remove releases all host resources for a stopped container:
//   - volume bind mounts
//   - the OverlayFS merged mount
//   - the cgroup directory
//   - any network interfaces and iptables rules
//
// The container's state-directory removal is NOT done here — the
// caller (daemon handler) persists the "removed" step via
// Registry.Remove, which owns the on-disk deletion.
//
// Pre-condition: the container is stopped. The handler enforces this
// (409 Conflict without ?force=1); Remove itself does not restart.
//
// Errors from each teardown step are accumulated with errors.Join so
// a single partial failure doesn't prevent the remaining cleanup —
// better to leak one of five resources than all of them.
func Remove(c *state.Container) error {
	var errs []error

	rootfs := overlay.MergedPath(c.ID)
	for _, spec := range c.Volumes {
		if err := volume.Unmount(spec, rootfs); err != nil {
			errs = append(errs, fmt.Errorf("unmount volume %s: %w", spec.Target, err))
		}
	}

	if err := overlay.Unmount(c.ID); err != nil {
		errs = append(errs, fmt.Errorf("unmount overlay: %w", err))
	}

	cg := cgroup.New(c.ID)
	if err := cg.Destroy(); err != nil {
		errs = append(errs, fmt.Errorf("destroy cgroup: %w", err))
	}

	if err := network.Teardown(c.ID, c.Ports, c.IP); err != nil {
		errs = append(errs, fmt.Errorf("teardown network: %w", err))
	}

	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}
