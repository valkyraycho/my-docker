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

func Rm(prefix string, force bool) error {
	c, err := state.Find(prefix)
	if err != nil {
		return fmt.Errorf("find container: %w", err)
	}

	if state.IsRunning(c.PID, c.StartTime) {
		if !force {
			return errors.New("container is running; stop first or use -f")
		}
		if err := Stop(prefix, DefaultStopTimeout); err != nil {
			return fmt.Errorf("stop before remove: %w", err)
		}
	}
	var errs []error

	for _, spec := range c.Volumes {
		if err := volume.Unmount(spec, overlay.MergedPath(c.ID)); err != nil {
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

	if err := state.RemoveDir(c.ID); err != nil {
		errs = append(errs, fmt.Errorf("remove container state directory: %w", err))
	}

	if err := network.Teardown(c.ID); err != nil {
		errs = append(errs, fmt.Errorf("teardown network: %w", err))
	}

	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}
