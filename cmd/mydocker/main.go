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
	"github.com/valkyraycho/my-docker/internal/image"
	"github.com/valkyraycho/my-docker/internal/overlay"
	"github.com/valkyraycho/my-docker/internal/registry"
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
	case "pull":
		pullCommand(os.Args[2:])
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
	detach := fs.Bool("d", false, "run container in background")
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

	ref := posArgs[0]
	cmdArgs := posArgs[1:]

	store := image.New()
	layers, err := store.Resolve(ref)
	if err != nil {
		fmt.Fprintf(os.Stderr, "resolve: %v\n", err)
		os.Exit(1)
	}

	mergedPath, err := overlay.Mount(containerID, layers)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mount: %v\n", err)
		os.Exit(1)
	}
	if !*detach {
		defer func() {
			if err := overlay.Unmount(containerID); err != nil {
				fmt.Fprintf(os.Stderr, "cleanup: %v\n", err)
			}
		}()
	}

	limits := cgroup.Limits{
		MemoryBytes: int64(*memMB) * 1024 * 1024,
		CPUPercent:  *cpuPct,
		PidsMax:     *pidsMax,
	}

	opts := container.RunOptions{
		ContainerID: containerID,
		Image:       ref,
		Layers:      layers,
		Rootfs:      mergedPath,
		Limits:      limits,
		Args:        cmdArgs,
		Detach:      *detach,
	}

	if err := container.Run(opts); err != nil {
		fmt.Fprintf(os.Stderr, "run: %v\n", err)
		os.Exit(1)
	}

	if *detach {
		fmt.Println(containerID)
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

func pullCommand(args []string) {
	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "usage: mydocker pull <image>[:<tag>]\n")
		os.Exit(1)
	}

	ref := args[0]
	client := registry.New(image.DefaultRegistry)
	store := image.New()

	if err := store.Pull(client, ref); err != nil {
		fmt.Fprintf(os.Stderr, "pull: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("pulled %s\n", ref)
}
