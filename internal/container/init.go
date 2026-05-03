//go:build linux

package container

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"

	"golang.org/x/sys/unix"
)

// Init runs inside the newly created namespaces as the container's PID 1.
// The kernel re-execed this binary after clone; Init completes the isolation:
// it blocks on the sync pipe until the parent finishes cgroup setup, then sets
// the hostname, calls pivot_root to switch the filesystem root to the overlay
// merged dir, mounts /proc, /dev, and /sys, and finally starts the user command.
// Because we are PID 1 inside CLONE_NEWPID, we must reap all orphaned children
// (reapUntilDirectExits) — otherwise zombie processes would accumulate.
func Init(rootfs string, args []string) error {
	if err := waitForParent(); err != nil {
		return fmt.Errorf("wait for parent: %w", err)
	}

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

// reapUntilDirectExits loops over SIGCHLD notifications, calling Wait4 in
// WNOHANG mode until all pending children are reaped. It returns the exit code
// only when the direct child exits; other children (grandchildren adopted after
// their parent died) are reaped and discarded so they don't become zombies.
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

// waitForParent drains the sync pipe passed as fd 3 by the parent process.
// The read blocks until the parent closes its write end, signalling that the
// cgroup and network setup are complete and it is safe to proceed.
func waitForParent() error {
	syncFile := os.NewFile(3, "sync")
	if syncFile == nil {
		return fmt.Errorf("sync pipe (fd 3) not present")
	}

	defer syncFile.Close()

	if _, err := io.Copy(io.Discard, syncFile); err != nil {
		return fmt.Errorf("read sync: %w", err)
	}

	return nil
}
