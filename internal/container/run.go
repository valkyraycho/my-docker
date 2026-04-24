//go:build linux

package container

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/valkyraycho/my-docker/internal/cgroup"
	"github.com/valkyraycho/my-docker/internal/state"
)

type RunOptions struct {
	ContainerID string
	Image       string
	Layers      []string
	Rootfs      string
	Limits      cgroup.Limits
	Args        []string
	Detach      bool
}

func Run(opts RunOptions) error {
	cmd := exec.Command("/proc/self/exe", append([]string{"init", opts.Rootfs}, opts.Args...)...)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWPID | syscall.CLONE_NEWUTS | syscall.CLONE_NEWNS | syscall.CLONE_NEWIPC,
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

	if err := cmd.Start(); err != nil {
		cg.Destroy()
		return fmt.Errorf("start: %w", err)
	}

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
	}
	if err := c.Save(); err != nil {
		cmd.Process.Kill()
		cg.Destroy()
		return fmt.Errorf("save state: %w", err)
	}

	if opts.Detach {
		return nil
	}
	defer cg.Destroy()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
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
