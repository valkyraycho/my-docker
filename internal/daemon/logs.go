//go:build linux

package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"

	"github.com/valkyraycho/my-docker/internal/api"
	"github.com/valkyraycho/my-docker/internal/state"
	"github.com/valkyraycho/my-docker/internal/stdcopy"
)

// handleContainerLogs implements GET /containers/{id}/logs. It streams the
// container's captured stdout/stderr as Docker-format stdcopy frames.
//
// Query params (all booleans, "1" or "true" = truthy):
//
//	stdout  — include stdout frames (required: at least one of stdout/stderr)
//	stderr  — include stderr frames
//	follow  — when true, stay open after EOF and stream new writes (3b.2)
//
// The response is committed (status + headers flushed) before any bytes of
// the log are written. Errors encountered mid-stream become log lines on the
// daemon — we can't turn a committed 200 into an error.
func (d *Deps) handleContainerLogs(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, errors.New("id is required"))
		return
	}
	opts := parseLogsQuery(r.URL.Query())

	if !opts.Stdout && !opts.Stderr {
		writeError(w, http.StatusBadRequest, errors.New("at least one of stdout or stderr is required"))
		return
	}

	c, err := d.Registry.Find(id)
	if err != nil {
		writeError(w, statusForError(err), err)
		return
	}

	w.Header().Set("Content-Type", "application/vnd.docker.raw-stream")
	w.WriteHeader(http.StatusOK)

	muxer := stdcopy.NewMuxer(&flushWriter{
		w: w,
	})

	if opts.Stdout {
		if err := streamLog(r.Context(), state.StdoutPath(c.ID), muxer.Stream(stdcopy.Stdout), opts.Follow); err != nil {
			log.Printf("stream stdout: %v", err)
		}
	}

	if opts.Stderr {
		if err := streamLog(r.Context(), state.StderrPath(c.ID), muxer.Stream(stdcopy.Stderr), opts.Follow); err != nil {
			log.Printf("stream stderr: %v", err)
		}
	}

}

// parseLogsQuery extracts ContainerLogsOptions from the HTTP query string.
// Docker treats "1" and "true" as truthy for each boolean; everything else
// (including absent keys) is false.
func parseLogsQuery(q url.Values) api.ContainerLogsOptions {
	return api.ContainerLogsOptions{
		Stdout: parseBool(q.Get("stdout")),
		Stderr: parseBool(q.Get("stderr")),
		Follow: parseBool(q.Get("follow")),
	}
}

func parseBool(s string) bool {
	return s == "1" || s == "true"
}

type flushWriter struct {
	w http.ResponseWriter
}

func (fw *flushWriter) Write(p []byte) (int, error) {
	n, err := fw.w.Write(p)
	if f, ok := fw.w.(http.Flusher); ok {
		f.Flush()
	}
	return n, err
}

func streamLog(ctx context.Context, logPath string, dst io.Writer, follow bool) error {
	f, err := os.Open(logPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open log path %s: %w", logPath, err)
	}

	defer f.Close()

	_, err = io.Copy(dst, f)
	return err
}
