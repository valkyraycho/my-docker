//go:build linux

package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/valkyraycho/my-docker/internal/cgroup"
	"github.com/valkyraycho/my-docker/internal/container"
	"github.com/valkyraycho/my-docker/internal/image"
	"github.com/valkyraycho/my-docker/internal/overlay"
	"github.com/valkyraycho/my-docker/internal/registry"
	"github.com/valkyraycho/my-docker/internal/volume"
)

var (
	runMemMB   int
	runCPUPct  int
	runPidsMax int
	runDetach  bool
	runVolumes []string
	runEnv     []string
)

var runCmd = &cobra.Command{
	Use:   "run [flags] <image> <cmd> [args...]",
	Short: "Run a command in a new container",
	Args:  cobra.MinimumNArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := overlay.EnsureRoot(); err != nil {
			return fmt.Errorf("setup: %w", err)
		}

		containerID := generateID()

		ref := args[0]
		cmdArgs := args[1:]

		store := image.New()

		layers, err := store.Resolve(ref)
		if err != nil {
			if errors.Is(err, image.ErrImageNotFound) {
				fmt.Fprintf(os.Stderr, "image %q not found locally, pulling...\n", ref)
				client := registry.New(image.DefaultRegistry)
				if err := store.Pull(client, ref); err != nil {
					return fmt.Errorf("pull: %w", err)
				}
				layers, err = store.Resolve(ref)
			}
		}
		if err != nil {
			return fmt.Errorf("resolve: %w", err)
		}

		mergedPath, err := overlay.Mount(containerID, layers)
		if err != nil {
			return fmt.Errorf("mount: %w", err)
		}
		if !runDetach {
			defer func() {
				if err := overlay.Unmount(containerID); err != nil {
					fmt.Fprintf(os.Stderr, "cleanup: %v\n", err)
				}
			}()
		}

		limits := cgroup.Limits{
			MemoryBytes: int64(runMemMB) * 1024 * 1024,
			CPUPercent:  runCPUPct,
			PidsMax:     runPidsMax,
		}

		var specs []*volume.Spec
		for _, s := range runVolumes {
			spec, err := volume.Parse(s)
			if err != nil {
				return fmt.Errorf("volume: %w", err)
			}
			specs = append(specs, spec)
		}

		var envs []string
		for _, e := range runEnv {
			if strings.Contains(e, "=") {
				envs = append(envs, e)
			} else {
				if val, ok := os.LookupEnv(e); ok {
					envs = append(envs, e+"="+val)
				}
			}
		}

		opts := container.RunOptions{
			ContainerID: containerID,
			Image:       ref,
			Layers:      layers,
			Rootfs:      mergedPath,
			Limits:      limits,
			Args:        cmdArgs,
			Detach:      runDetach,
			Volumes:     specs,
			Env:         envs,
		}

		if err := container.Run(opts); err != nil {
			return err
		}

		if runDetach {
			fmt.Println(containerID)
		}
		return nil
	},
}

func init() {
	f := runCmd.Flags()
	f.SetInterspersed(false)
	f.IntVarP(&runMemMB, "memory", "m", 0, "memory limit in MB (0 = no limit)")
	f.IntVar(&runCPUPct, "cpu", 0, "cpu limit as percent (0 = no limit)")
	f.IntVar(&runPidsMax, "pids", 0, "max processes (0 = no limit)")
	f.BoolVarP(&runDetach, "detach", "d", false, "run container in background")
	f.StringArrayVarP(&runVolumes, "volume", "v", nil, "volume mount (repeatable): src:dst[:ro]")
	f.StringArrayVarP(&runEnv, "env", "e", nil, "environment variable (repeatable): KEY=VAL or KEY")
}

func generateID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
