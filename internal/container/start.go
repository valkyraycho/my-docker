//go:build linux

package container

import (
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/valkyraycho/my-docker/internal/cgroup"
	"github.com/valkyraycho/my-docker/internal/network"
	"github.com/valkyraycho/my-docker/internal/overlay"
	"github.com/valkyraycho/my-docker/internal/state"
	"github.com/valkyraycho/my-docker/internal/volume"
	"golang.org/x/sys/unix"
)

// Start brings a previously-created container to life. Assumes the
// container's registry entry exists with Image, Layers, Command,
// Volumes, Ports populated (i.e. POST /containers/create has run).
// Mutates c in place with runtime fields (PID, StartTime, IP, Status,
// StartedAt) on success; caller is responsible for persisting via
// Registry.Update.
//
// The daemon is the direct parent of the container init process here.
// Daemon restart will kill the container — acknowledged gap, fixed
// when the shim lands.
//
// All containers started through this path are detached from the
// caller's perspective: stdout/stderr are written to per-container
// log files, not piped anywhere.
//
// Error-path cleanup uses a LIFO stack (`cleanups`): each resource
// acquisition appends its undo action. On success we nil the stack
// before returning so teardown is handed to the caller (future
// stop/rm handlers). On any early return the deferred runner fires
// every pending cleanup in reverse order.
func Start(c *state.Container) error {
	rootfs, err := overlay.Mount(c.ID, c.Layers)
	if err != nil {
		return fmt.Errorf("mount overlay: %w", err)
	}

	var cleanups []func()
	defer func() {
		for i := len(cleanups) - 1; i >= 0; i-- {
			cleanups[i]()
		}
	}()
	cleanups = append(cleanups, func() { _ = overlay.Unmount(c.ID) })

	stdoutF, err := os.OpenFile(state.StdoutPath(c.ID), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("open stdout log: %w", err)
	}
	defer stdoutF.Close()

	stderrF, err := os.OpenFile(state.StderrPath(c.ID), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("open stderr log: %w", err)
	}
	defer stderrF.Close()

	cmd := exec.Command("/proc/self/exe", append([]string{"init", rootfs}, c.Command...)...)
	cmd.SysProcAttr = &unix.SysProcAttr{
		Cloneflags: unix.CLONE_NEWPID | unix.CLONE_NEWUTS | unix.CLONE_NEWNS | unix.CLONE_NEWIPC | unix.CLONE_NEWNET,
		Setsid:     true,
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, stdoutF, stderrF

	cg := cgroup.New(c.ID)
	if err := cg.Create(cgroup.Limits{}); err != nil {
		return fmt.Errorf("create cgroup: %w", err)
	}
	cleanups = append(cleanups, func() { cg.Destroy() })

	for _, spec := range c.Volumes {
		if err := volume.Mount(spec, rootfs); err != nil {
			return fmt.Errorf("mount volume %s:%s: %w", spec.Source, spec.Target, err)
		}
		cleanups = append(cleanups, func() { _ = volume.Unmount(spec, rootfs) })
	}

	pipeR, pipeW, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("create sync pipe: %w", err)
	}
	// pipeW: closed explicitly below to signal the child on success; the
	// cleanup entry ensures it is closed on every early-return path too.
	// os.File.Close is safe to call twice — the second call returns
	// ErrClosed which we discard.
	cleanups = append(cleanups, func() { _ = pipeW.Close() })

	cmd.ExtraFiles = []*os.File{pipeR}
	cmd.Env = c.EnvForExec()

	if err := cmd.Start(); err != nil {
		pipeR.Close()
		return fmt.Errorf("start init: %w", err)
	}
	pipeR.Close()
	cleanups = append(cleanups, func() { _ = cmd.Process.Kill() })

	if err := cg.AddPID(cmd.Process.Pid); err != nil {
		return fmt.Errorf("add pid: %w", err)
	}

	startTime, err := state.ReadStartTime(cmd.Process.Pid)
	if err != nil {
		return fmt.Errorf("read start time: %w", err)
	}

	ip, err := network.Setup(c.ID, rootfs, cmd.Process.Pid, c.Ports)
	if err != nil {
		return fmt.Errorf("network setup: %w", err)
	}
	cleanups = append(cleanups, func() { _ = network.Teardown(c.ID, c.Ports, ip) })

	c.PID = cmd.Process.Pid
	c.StartTime = startTime
	c.IP = ip
	c.Status = state.StatusRunning
	c.StartedAt = time.Now()

	// Signal child: last thing that can fail. If it does, the cleanup
	// stack unwinds every acquisition above.
	if err := pipeW.Close(); err != nil {
		return fmt.Errorf("signal child: %w", err)
	}

	// Success — hand teardown ownership to the caller (stop/rm).
	cleanups = nil
	return nil
}
