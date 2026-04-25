//go:build linux

package container

import (
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/valkyraycho/my-docker/internal/state"
)

func Ps(w io.Writer, showAll bool) error {
	containers, err := state.List()
	if err != nil {
		return fmt.Errorf("list containers: %w", err)
	}

	for _, c := range containers {
		if c.Status == state.StatusRunning && !state.IsRunning(c.PID, c.StartTime) {
			c.Status = state.StatusExited
			c.FinishedAt = time.Now()
			if err := c.Save(); err != nil {
				fmt.Fprintf(os.Stderr, "warning: failed to update state for %s: %v\n", c.ID, err)
			}
		}
	}

	tabWriter := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tabWriter, "CONTAINER ID\tIMAGE\tCOMMAND\tSTATUS\tCREATED")

	for _, c := range containers {
		if !showAll && c.Status != state.StatusRunning {
			continue
		}
		fmt.Fprintf(tabWriter, "%s\t%s\t%s\t%s\t%s\n", shortID(c.ID), c.Image, strings.Join(c.Command, " "), c.Status, sinceFormatted(c.CreatedAt))
	}

	return tabWriter.Flush()
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func sinceFormatted(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours())/24)
	}
}
