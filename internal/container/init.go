//go:build linux

package container

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"golang.org/x/sys/unix"
)

func Init(rootfs string, args []string) error {
	if err := unix.Sethostname([]byte("my-docker")); err != nil {
		return fmt.Errorf("sethostname: %w", err)
	}

	if err := setupRoot(rootfs); err != nil {
		return fmt.Errorf("setup root: %w", err)
	}

	if err := setupMounts(); err != nil {
		return fmt.Errorf("setup mounts: %w", err)
	}

	binary, err := exec.LookPath(args[0])
	if err != nil {
		return fmt.Errorf("lookpath %q: %w", args[0], err)
	}

	cmd := exec.Command(binary, args[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = os.Environ()
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start command %s: %w", binary, err)
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT,
		syscall.SIGQUIT, syscall.SIGHUP,
		syscall.SIGUSR1, syscall.SIGUSR2)

	go func() {
		for sig := range sigCh {
			_ = cmd.Process.Signal(sig)
		}
	}()

	err = cmd.Wait()
	signal.Stop(sigCh)
	close(sigCh)

	if err != nil && cmd.ProcessState == nil {
		return fmt.Errorf("wait: %w", err)
	}
	os.Exit(cmd.ProcessState.ExitCode())

	return nil
}
