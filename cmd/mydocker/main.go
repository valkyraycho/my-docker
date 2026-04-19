//go:build linux

package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/valkyraycho/my-docker/internal/cgroup"
	"github.com/valkyraycho/my-docker/internal/container"
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
		fmt.Fprintf(os.Stderr, "usage: mydocker run [flags] <rootfs> <cmd> [args...]\n")
		os.Exit(1)
	}

	rootfs := posArgs[0]
	cmdArgs := posArgs[1:]

	limits := cgroup.Limits{
		MemoryBytes: int64(*memMB) * 1024 * 1024,
		CPUPercent:  *cpuPct,
		PidsMax:     *pidsMax,
	}

	if err := container.Run(rootfs, limits, cmdArgs); err != nil {
		fmt.Fprintf(os.Stderr, "run: %v\n", err)
		os.Exit(1)
	}
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
