//go:build linux

package main

import (
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"os"

	"github.com/valkyraycho/my-docker/internal/cgroup"
	"github.com/valkyraycho/my-docker/internal/container"
	"github.com/valkyraycho/my-docker/internal/overlay"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "usage: mydocker run <cmd> [args...]\n")
		os.Exit(1)
	}

	switch os.Args[1] {
	case "run":
		runCommand(os.Args[2:])
	case "init":
		initCommand(os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", os.Args[1])
		os.Exit(1)
	}
}

func runCommand(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	memMB := fs.Int("memory", 0, "memory limit in MB (0 = no limit)")
	cpuPct := fs.Int("cpu", 0, "cpu limit as percent (0 = no limit)")
	pidsMax := fs.Int("pids", 0, "max processes (0 = no limit)")
	if err := fs.Parse(args); err != nil {
		os.Exit(1)
	}

	posArgs := fs.Args()
	if len(posArgs) < 2 {
		fmt.Fprintf(os.Stderr, "usage: mydocker run [flags] <layer> <cmd> [args...]\n")
		os.Exit(1)
	}

	if err := overlay.EnsureRoot(); err != nil {
		fmt.Fprintf(os.Stderr, "setup: %v\n", err)
		os.Exit(1)
	}

	containerID := generateID()

	layer := posArgs[0]
	cmdArgs := posArgs[1:]

	mergedPath, err := overlay.Mount(containerID, []string{layer})
	if err != nil {
		fmt.Fprintf(os.Stderr, "mount: %v\n", err)
		os.Exit(1)
	}
	defer func() {
		if err := overlay.Unmount(containerID); err != nil {
			fmt.Fprintf(os.Stderr, "cleanup: %v\n", err)
		}
	}()

	limits := cgroup.Limits{
		MemoryBytes: int64(*memMB) * 1024 * 1024,
		CPUPercent:  *cpuPct,
		PidsMax:     *pidsMax,
	}

	if err := container.Run(containerID, mergedPath, limits, cmdArgs); err != nil {
		fmt.Fprintf(os.Stderr, "run: %v\n", err)
		os.Exit(1)
	}
}

func generateID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func initCommand(args []string) {
	if len(args) < 2 {
		fmt.Fprintf(os.Stderr, "init: missing args\n")
		os.Exit(1)
	}

	rootfs := args[0]
	cmdArgs := args[1:]

	if err := container.Init(rootfs, cmdArgs); err != nil {
		fmt.Fprintf(os.Stderr, "init: %v\n", err)
		os.Exit(1)
	}
}
