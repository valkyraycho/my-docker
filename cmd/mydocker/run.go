//go:build linux

package main

import (
	"fmt"
	"os"
	"strconv"

	"github.com/spf13/cobra"
	"github.com/valkyraycho/my-docker/internal/api"
)

// run-command flags. Stored as package-level vars so cobra can bind them.
var (
	runDetach  bool
	runVolumes []string
	runEnv     []string
	runPorts   []string
)

// runCmd implements "mydocker run" as CLI-side sugar for
// ContainerCreate + ContainerStart against the daemon. After M9 the CLI
// no longer forks container processes directly — the daemon does that.
//
// Foreground attach (stdout streaming for non-detached runs) lands in
// chunk 3. Until then, every `run` is effectively detached: the CLI
// prints the container ID and returns; logs are available via
// `mydocker logs`.
var runCmd = &cobra.Command{
	Use:   "run [flags] <image> <cmd> [args...]",
	Short: "Create and start a container",
	Args:  cobra.MinimumNArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		cli, err := getClient()
		if err != nil {
			return err
		}

		image := args[0]
		cmdArgs := args[1:]

		ports, err := parsePublishFlags(runPorts)
		if err != nil {
			return fmt.Errorf("publish: %w", err)
		}

		req := &api.ContainerCreateRequest{
			Image: image,
			Cmd:   cmdArgs,
			Env:   runEnv,
			HostConfig: api.HostConfig{
				Binds:        runVolumes,
				PortBindings: ports,
			},
		}

		created, err := cli.ContainerCreate(cmd.Context(), req)
		if err != nil {
			return fmt.Errorf("create: %w", err)
		}

		if err := cli.ContainerStart(cmd.Context(), created.ID); err != nil {
			// Create succeeded, start failed. The container is in the
			// registry but not running. Surface the ID so the user can
			// inspect or clean up (`mydocker rm <id>`).
			return fmt.Errorf("container %s created but start failed: %w",
				created.ID, err)
		}

		// Chunk 3 will add foreground attach. Until then every run is
		// effectively detached — print the ID so the user can follow up.
		if !runDetach {
			fmt.Fprintf(os.Stderr,
				"note: attach not yet implemented; running detached. Use `mydocker logs %s` to view output.\n",
				created.ID)
		}
		fmt.Println(created.ID)
		return nil
	},
}

func init() {
	f := runCmd.Flags()
	f.SetInterspersed(false)
	f.BoolVarP(&runDetach, "detach", "d", false, "run container in background (currently the default; see note in chunk 3)")
	f.StringArrayVarP(&runVolumes, "volume", "v", nil, "volume mount (repeatable): src:dst[:ro]")
	f.StringArrayVarP(&runEnv, "env", "e", nil, "environment variable (repeatable): KEY=VAL")
	f.StringArrayVarP(&runPorts, "publish", "p", nil, "publish port (repeatable): hostPort:containerPort[/proto]")
}

// parsePublishFlags converts CLI "-p 8080:80[/tcp]" entries into
// Docker's nested PortBindings wire shape
// (map of "80/tcp" -> [{HostPort: "8080"}]). Protocol defaults to tcp.
// Numeric validation is done here so the user gets a local error
// instead of a round-trip-to-daemon one.
func parsePublishFlags(flags []string) (map[string][]api.PortBinding, error) {
	if len(flags) == 0 {
		return nil, nil
	}
	out := make(map[string][]api.PortBinding, len(flags))
	for _, raw := range flags {
		host, cont, ok := splitPort(raw)
		if !ok {
			return nil, fmt.Errorf("port spec %q: expected hostPort:containerPort[/proto]", raw)
		}
		if _, err := strconv.Atoi(host); err != nil {
			return nil, fmt.Errorf("host port %q: %w", host, err)
		}
		portOnly, _, _ := cutProto(cont)
		if _, err := strconv.Atoi(portOnly); err != nil {
			return nil, fmt.Errorf("container port %q: %w", portOnly, err)
		}

		key := cont
		if _, hasProto, _ := cutProto(cont); !hasProto {
			key = cont + "/tcp"
		}
		out[key] = append(out[key], api.PortBinding{HostPort: host})
	}
	return out, nil
}

// splitPort splits "host:container[/proto]" on the first colon.
func splitPort(s string) (host, cont string, ok bool) {
	for i := range len(s) {
		if s[i] == ':' {
			return s[:i], s[i+1:], true
		}
	}
	return "", "", false
}

// cutProto splits "80/tcp" into ("80", true, "tcp"). If no slash is
// present returns (s, false, "").
func cutProto(s string) (port string, hasProto bool, proto string) {
	for i := range len(s) {
		if s[i] == '/' {
			return s[:i], true, s[i+1:]
		}
	}
	return s, false, ""
}
