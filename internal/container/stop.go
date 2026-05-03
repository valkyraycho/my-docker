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

// DefaultStopTimeout is the grace period between SIGTERM and SIGKILL
// when the caller doesn't specify one. Matches Docker's default.
const DefaultStopTimeout = 10 * time.Second

// Stop graceful-stops a running container. Sends SIGTERM and waits up
// to timeout for the process to exit; if it hasn't, escalates to
// SIGKILL. Mutates c in place — caller persists via Registry.Update.
//
// If the process is already gone (PID reused, never ran, or exited
// before we got here), the function is idempotent and marks the
// container as Exited without signaling anything.
//
// Exit code tracking is deferred to chunk 6 (shim/reconciliation).
// For now c.ExitCode is left unchanged.
func Stop(c *state.Container, timeout time.Duration) error {
	if !state.IsRunning(c.PID, c.StartTime) {
		markExited(c)
		return nil
	}

	if err := unix.Kill(c.PID, unix.SIGTERM); err != nil && !errors.Is(err, unix.ESRCH) {
		return fmt.Errorf("sigterm: %w", err)
	}

	if waitForExit(c, timeout) {
		markExited(c)
		return nil
	}

	// Graceful window elapsed — escalate.
	if err := unix.Kill(c.PID, unix.SIGKILL); err != nil && !errors.Is(err, unix.ESRCH) {
		return fmt.Errorf("sigkill: %w", err)
	}
	waitForExit(c, time.Second)
	markExited(c)
	return nil
}

// Kill is Stop's impatient cousin: SIGKILL immediately, short wait,
// mark exited. Used by POST /containers/{id}/kill.
func Kill(c *state.Container) error {
	if !state.IsRunning(c.PID, c.StartTime) {
		markExited(c)
		return nil
	}

	if err := unix.Kill(c.PID, unix.SIGKILL); err != nil && !errors.Is(err, unix.ESRCH) {
		return fmt.Errorf("sigkill: %w", err)
	}

	waitForExit(c, 2*time.Second)
	markExited(c)
	return nil
}

// markExited stamps c with the exit-time invariants. Does NOT persist;
// the caller owns writing through Registry.Update.
func markExited(c *state.Container) {
	c.Status = state.StatusExited
	if c.FinishedAt.IsZero() {
		c.FinishedAt = time.Now()
	}
}

// waitForExit polls (PID, StartTime) every 100 ms until the process is
// gone or the timeout elapses. Returns true on exit, false on timeout.
//
// Polling is cheap against /proc and avoids a kernel-level Wait that
// would require us to own the *exec.Cmd handle (we don't, because
// Start returns after forking). In chunk 5 the shim will hold the Wait
// and give us proper exit-code retrieval.
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
