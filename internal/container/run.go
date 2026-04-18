//go:build linux

package container

import (
	"os"
	"os/exec"
	"syscall"
)

func Run(args []string) error {
	cmd := exec.Command("/proc/self/exe", append([]string{"init"}, args...)...)

	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWPID | syscall.CLONE_NEWUTS | syscall.CLONE_NEWNS | syscall.CLONE_NEWIPC,
	}
	return cmd.Run()
}
