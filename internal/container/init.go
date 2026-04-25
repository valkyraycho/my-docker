//go:build linux

package container

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"

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

	childCh := make(chan os.Signal, 1)
	signal.Notify(childCh, unix.SIGCHLD)

	if err := cmd.Start(); err != nil {
		signal.Stop(childCh)
		return fmt.Errorf("start command %s: %w", binary, err)
	}
	directChild := cmd.Process.Pid

	fwdCh := make(chan os.Signal, 1)
	signal.Notify(fwdCh, unix.SIGTERM, unix.SIGINT,
		unix.SIGQUIT, unix.SIGHUP,
		unix.SIGUSR1, unix.SIGUSR2)

	go func() {
		for sig := range fwdCh {
			_ = cmd.Process.Signal(sig)
		}
	}()

	exitCode := reapUntilDirectExits(directChild, childCh)

	signal.Stop(fwdCh)
	close(fwdCh)
	signal.Stop(childCh)
	close(childCh)

	os.Exit(exitCode)
	return nil
}

func reapUntilDirectExits(directChild int, childCh <-chan os.Signal) int {
	for range childCh {
		for {
			var ws unix.WaitStatus
			pid, err := unix.Wait4(-1, &ws, unix.WNOHANG, nil)
			if err != nil || pid <= 0 {
				break
			}
			if pid != directChild {
				continue
			}

			switch {
			case ws.Exited():
				return ws.ExitStatus()
			case ws.Signaled():

				return 128 + int(ws.Signal())
			default:
				return 1
			}
		}
	}
	return 1
}
