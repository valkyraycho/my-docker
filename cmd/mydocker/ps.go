//go:build linux

package main

import (
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

// psShowAll is set by the -a flag; when true, stopped containers are included.
var psShowAll bool

// psCmd implements "mydocker ps". By default only running containers are shown;
// pass -a to include stopped containers.
//
// After the M9 daemon split this command talks to mydockerd over the UNIX
// socket — state is owned by the daemon, not read from disk by the CLI.
var psCmd = &cobra.Command{
	Use:   "ps",
	Short: "List containers",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		cli, err := getClient()
		if err != nil {
			return err
		}

		containers, err := cli.ContainerList(cmd.Context(), psShowAll)
		if err != nil {
			return fmt.Errorf("list containers: %w", err)
		}

		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
		defer tw.Flush()

		fmt.Fprintln(tw, "CONTAINER ID\tIMAGE\tCOMMAND\tCREATED\tSTATUS")
		for _, cs := range containers {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
				shortID(cs.ID),
				cs.Image,
				cs.Command,
				sinceUnix(cs.Created),
				cs.Status,
			)
		}
		return nil
	},
}

func init() {
	psCmd.Flags().BoolVarP(&psShowAll, "all", "a", false, "show all containers (default: running only)")
}

// shortID returns the first 12 hex characters of a container ID, matching
// Docker's default display format.
func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// sinceUnix returns a human-readable age string ("30 seconds ago",
// "5 minutes ago", etc.) for the given Unix timestamp, mirroring
// Docker's CREATED column style.
func sinceUnix(unix int64) string {
	d := time.Since(time.Unix(unix, 0))
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d seconds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d minutes ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d hours ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%d days ago", int(d.Hours())/24)
	}
}
