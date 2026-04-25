//go:build linux

package container

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/valkyraycho/my-docker/internal/state"
	"golang.org/x/sys/unix"
)

const DefaultStopTimeout = 10 * time.Second

func Stop(prefix string, timeout time.Duration) error {
	c, err := state.Find(prefix)
	if err != nil {
		return fmt.Errorf("find container: %w", err)
	}

	if !state.IsRunning(c.PID, c.StartTime) {
		return reconcileExited(c)
	}

	if err := unix.Kill(c.PID, unix.SIGTERM); err != nil {
		if !errors.Is(err, unix.ESRCH) {
			return fmt.Errorf("sigterm: %w", err)
		}
	}

	if waitForExit(c, timeout) {
		return reconcileExited(c)
	}

	if err := unix.Kill(c.PID, unix.SIGKILL); err != nil {
		if !errors.Is(err, unix.ESRCH) {
			return fmt.Errorf("sigkill: %w", err)
		}
	}

	waitForExit(c, time.Second)
	return reconcileExited(c)
}

func reconcileExited(c *state.Container) error {
	c.Status = state.StatusExited
	c.FinishedAt = time.Now()

	if err := c.Save(); err != nil {
		return fmt.Errorf("save state: %w", err)
	}
	return nil
}

func waitForExit(c *state.Container, timeout time.Duration) bool {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	if !state.IsRunning(c.PID, c.StartTime) {
		return true
	}

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
			if !state.IsRunning(c.PID, c.StartTime) {
				return true
			}
		}
	}
}
