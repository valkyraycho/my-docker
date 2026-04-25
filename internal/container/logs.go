//go:build linux

package container

import (
	"fmt"
	"io"
	"os"

	"github.com/valkyraycho/my-docker/internal/state"
)

func Logs(w io.Writer, prefix string) error {
	c, err := state.Find(prefix)
	if err != nil {
		return fmt.Errorf("find container: %w", err)
	}

	f, err := os.Open(state.StdoutPath(c.ID))
	if err != nil {
		return fmt.Errorf("no logs for %s — foreground container?", c.ID)
	}
	defer f.Close()

	if _, err := io.Copy(w, f); err != nil {
		return err
	}
	return nil
}
