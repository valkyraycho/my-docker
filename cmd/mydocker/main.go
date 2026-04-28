//go:build linux

package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"

	"github.com/valkyraycho/my-docker/internal/cgroup"
	"github.com/valkyraycho/my-docker/internal/container"
	"github.com/valkyraycho/my-docker/internal/image"
	"github.com/valkyraycho/my-docker/internal/overlay"
	"github.com/valkyraycho/my-docker/internal/registry"
	"github.com/valkyraycho/my-docker/internal/volume"
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
	case "ps":
		psCommand(os.Args[2:])
	case "logs":
		logsCommand(os.Args[2:])
	case "stop":
		stopCommand(os.Args[2:])
	case "rm":
		rmCommand(os.Args[2:])
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

	var volumeSpecs stringSliceFlag
	fs.Var(&volumeSpecs, "v", "volume mount (repeatable): src:dst[:ro]")

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
		if errors.Is(err, image.ErrImageNotFound) {
			fmt.Fprintf(os.Stderr, "image %q not found locally, pulling...\n", ref)
			client := registry.New(image.DefaultRegistry)
			if err := store.Pull(client, ref); err != nil {
				fmt.Fprintf(os.Stderr, "pull: %v\n", err)
				os.Exit(1)
			}
			layers, err = store.Resolve(ref)
		}
	}

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

	var specs []*volume.Spec
	for _, s := range volumeSpecs {
		spec, err := volume.Parse(s)
		if err != nil {
			fmt.Fprintf(os.Stderr, "volume: %v\n", err)
			os.Exit(1)
		}
		specs = append(specs, spec)
	}

	opts := container.RunOptions{
		ContainerID: containerID,
		Image:       ref,
		Layers:      layers,
		Rootfs:      mergedPath,
		Limits:      limits,
		Args:        cmdArgs,
		Detach:      *detach,
		Volumes:     specs,
	}

	if err := container.Run(opts); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			os.Exit(ee.ExitCode())
		}
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
	fmt.Fprintf(os.Stderr, "pulled %s\n", ref)
}

func psCommand(args []string) {
	fs := flag.NewFlagSet("ps", flag.ExitOnError)
	showAll := fs.Bool("a", false, "show all containers (default: running only)")

	if err := fs.Parse(args); err != nil {
		os.Exit(1)
	}

	if err := container.Ps(os.Stdout, *showAll); err != nil {
		os.Exit(1)
	}
}

func logsCommand(args []string) {
	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "usage: mydocker logs <id>\n")
		os.Exit(1)
	}
	if err := container.Logs(os.Stdout, args[0]); err != nil {
		fmt.Fprintf(os.Stderr, "logs: %v\n", err)
		os.Exit(1)
	}
}

func stopCommand(args []string) {
	fs := flag.NewFlagSet("stop", flag.ExitOnError)
	timeout := fs.Duration("t", container.DefaultStopTimeout, "timeout before sending SIGKILL")

	if err := fs.Parse(args); err != nil {
		os.Exit(1)
	}

	posArgs := fs.Args()
	if len(posArgs) < 1 {
		fmt.Fprintf(os.Stderr, "usage: mydocker stop [-t <timeout>] <id>\n")
		os.Exit(1)
	}
	if err := container.Stop(posArgs[0], *timeout); err != nil {
		fmt.Fprintf(os.Stderr, "stop: %v\n", err)
		os.Exit(1)
	}

	fmt.Println(posArgs[0])
}

func rmCommand(args []string) {
	fs := flag.NewFlagSet("rm", flag.ExitOnError)
	force := fs.Bool("f", false, "force removal (stops if running)")

	if err := fs.Parse(args); err != nil {
		os.Exit(1)
	}

	posArgs := fs.Args()
	if len(posArgs) < 1 {
		fmt.Fprintf(os.Stderr, "usage: mydocker rm [-f] <id>\n")
		os.Exit(1)
	}

	if err := container.Rm(posArgs[0], *force); err != nil {
		fmt.Fprintf(os.Stderr, "rm: %v\n", err)
		os.Exit(1)
	}

	fmt.Println(posArgs[0])
}
