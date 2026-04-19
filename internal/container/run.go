//go:build linux

package container

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"github.com/valkyraycho/my-docker/internal/cgroup"
)

func Run(containerID string, rootfs string, limits cgroup.Limits, args []string) error {
	cmd := exec.Command("/proc/self/exe", append([]string{"init", rootfs}, args...)...)

	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr

	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWPID | syscall.CLONE_NEWUTS | syscall.CLONE_NEWNS | syscall.CLONE_NEWIPC,
	}

	cg := cgroup.New(containerID)
	if err := cg.Create(limits); err != nil {
		return fmt.Errorf("create cgroup: %w", err)
	}
	defer cg.Destroy()

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start: %w", err)
	}

	if err := cg.AddPID(cmd.Process.Pid); err != nil {
		return fmt.Errorf("add pid: %w", err)
	}

	return cmd.Wait()
}
