//go:build linux

// Package container implements the high-level container lifecycle operations:
// run, init, stop, rm, ps, and logs. Each operation coordinates the lower-level
// subsystems (overlay, cgroup, network, volume) that together form a container.
package container

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"time"

	"github.com/valkyraycho/my-docker/internal/cgroup"
	"github.com/valkyraycho/my-docker/internal/network"
	"github.com/valkyraycho/my-docker/internal/state"
	"github.com/valkyraycho/my-docker/internal/volume"
	"golang.org/x/sys/unix"
)

// RunOptions carries all parameters the parent process needs to launch a container.
// It is populated by the daemon handler and passed into Run.
type RunOptions struct {
	ContainerID string
	Image       string
	Layers      []string
	Rootfs      string
	Limits      cgroup.Limits
	Args        []string
	Detach      bool
	Volumes     []*volume.Spec
	Env         []string
	Ports       []*network.PortSpec
}

// Run is the parent-side entrypoint for "mydocker run". It mounts volumes,
// creates the cgroup, then re-execs /proc/self/exe with "init" as its first
// argument inside new namespaces (CLONE_NEWPID|CLONE_NEWNS|CLONE_NEWUTS|
// CLONE_NEWIPC|CLONE_NEWNET). A pipe synchronises the two sides: the child
// blocks on the read end until the parent has written its PID into the cgroup
// and saved container state, at which point the pipe is closed and the child
// proceeds to set up mounts and exec the user command.
func Run(opts RunOptions) error {
	cmd := exec.Command("/proc/self/exe", append([]string{"init", opts.Rootfs}, opts.Args...)...)
	cmd.SysProcAttr = &unix.SysProcAttr{
		Cloneflags: unix.CLONE_NEWPID | unix.CLONE_NEWUTS | unix.CLONE_NEWNS | unix.CLONE_NEWIPC | unix.CLONE_NEWNET,
	}

	if opts.Detach {
		stdoutLogFile, err := os.OpenFile(state.StdoutPath(opts.ContainerID), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			return fmt.Errorf("open stdout log: %w", err)
		}
		defer stdoutLogFile.Close()

		stderrLogFile, err := os.OpenFile(state.StderrPath(opts.ContainerID), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			stdoutLogFile.Close()
			return fmt.Errorf("open stderr log: %w", err)
		}
		defer stderrLogFile.Close()

		cmd.SysProcAttr.Setsid = true
		cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, stdoutLogFile, stderrLogFile
	} else {
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	}

	cg := cgroup.New(opts.ContainerID)
	if err := cg.Create(opts.Limits); err != nil {
		return fmt.Errorf("create cgroup: %w", err)
	}

	var mountedSoFar []*volume.Spec
	for _, spec := range opts.Volumes {
		if err := volume.Mount(spec, opts.Rootfs); err != nil {
			for _, prev := range mountedSoFar {
				_ = volume.Unmount(prev, opts.Rootfs)
			}
			cg.Destroy()
			return fmt.Errorf("mount volume %s:%s: %w", spec.Source, spec.Target, err)
		}
		mountedSoFar = append(mountedSoFar, spec)
	}

	pipeR, pipeW, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("create sync pipe: %w", err)
	}

	cmd.ExtraFiles = []*os.File{pipeR}

	defer pipeW.Close()

	cmd.Env = append(os.Environ(), opts.Env...)

	if err := cmd.Start(); err != nil {
		pipeR.Close()
		cg.Destroy()
		return fmt.Errorf("start: %w", err)
	}
	pipeR.Close()

	if err := cg.AddPID(cmd.Process.Pid); err != nil {
		cmd.Process.Kill()
		cg.Destroy()
		return fmt.Errorf("add pid: %w", err)
	}

	startTime, err := state.ReadStartTime(cmd.Process.Pid)
	if err != nil {
		cmd.Process.Kill()
		cg.Destroy()
		return fmt.Errorf("read start time: %w", err)
	}

	ip, err := network.Setup(opts.ContainerID, opts.Rootfs, cmd.Process.Pid, opts.Ports)
	if err != nil {
		cmd.Process.Kill()
		cg.Destroy()
		return fmt.Errorf("network setup: %w", err)
	}

	now := time.Now()

	c := &state.Container{
		ID:        opts.ContainerID,
		Image:     opts.Image,
		Layers:    opts.Layers,
		Command:   opts.Args,
		PID:       cmd.Process.Pid,
		StartTime: startTime,
		Status:    state.StatusRunning,
		CreatedAt: now,
		StartedAt: now,
		IP:        ip,
		Volumes:   opts.Volumes,
		Ports:     opts.Ports,
	}
	if err := c.Save(); err != nil {
		cmd.Process.Kill()
		cg.Destroy()
		network.Teardown(opts.ContainerID, opts.Ports, ip)
		return fmt.Errorf("save state: %w", err)
	}

	if err := pipeW.Close(); err != nil {
		cmd.Process.Kill()
		cg.Destroy()
		network.Teardown(opts.ContainerID, opts.Ports, ip)
		return fmt.Errorf("signal child: %w", err)
	}

	if opts.Detach {
		return nil
	}
	defer cg.Destroy()
	defer network.Teardown(opts.ContainerID, opts.Ports, ip)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, unix.SIGINT, unix.SIGTERM)
	go func() {
		for sig := range sigCh {
			cmd.Process.Signal(sig)
		}
	}()

	err = cmd.Wait()
	signal.Stop(sigCh)
	close(sigCh)

	c.Status = state.StatusExited
	c.ExitCode = cmd.ProcessState.ExitCode()
	c.FinishedAt = time.Now()

	if err := c.Save(); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to persist exit state for %s: %v\n", c.ID, err)
	}

	return err

}
