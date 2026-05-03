//go:build linux

// Package main is the entry point for mydockerd, the container daemon.
// It listens on a UNIX socket (default /var/run/mydocker.sock), handles HTTP
// API requests from the mydocker CLI, and shuts down gracefully on SIGINT/SIGTERM.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/valkyraycho/my-docker/internal/container"
	"github.com/valkyraycho/my-docker/internal/daemon"
	"github.com/valkyraycho/my-docker/internal/image"
	"github.com/valkyraycho/my-docker/internal/state"
	"golang.org/x/sys/unix"
)

// defaultHost is the UNIX socket address the daemon binds to by default.
const defaultHost = "unix:///var/run/mydocker.sock"

// shutdownTimeout is how long the daemon waits for in-flight requests to
// finish before it forcefully closes the listener on SIGINT/SIGTERM.
const shutdownTimeout = 15 * time.Second

func main() {
	os.Exit(run())
}

// run is the real main body, split out so it can return an exit code that
// main() passes to os.Exit. This pattern avoids deferred calls being skipped
// when os.Exit is called directly inside main.
func run() int {
	var hostFlag string
	flag.StringVar(&hostFlag, "H", defaultHost, "Daemon socket to connect to")
	flag.Parse()

	socketPath, err := parseHost(hostFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid host: %v\n", err)
		return 2
	}

	registry, err := state.NewRegistry()
	if err != nil {
		fmt.Fprintf(os.Stderr, "load state: %v\n", err)
		return 1
	}

	imageStore := image.New()
	if err := imageStore.EnsureDirs(); err != nil {
		fmt.Fprintf(os.Stderr, "init image store: %v\n", err)
		return 1
	}

	deps := &daemon.Deps{
		Registry:   registry,
		ImageStore: imageStore,
		StartInit:  container.Start,
		StopInit:   container.Stop,
		KillInit:   container.Kill,
		RemoveInit: container.Remove,
	}

	handler := daemon.NewHandler(deps)

	s := daemon.New(socketPath, handler)

	ctx, stop := signal.NotifyContext(context.Background(), unix.SIGINT, unix.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		errCh <- s.Start()
	}()

	log.Printf("mydockerd: listening on %s", socketPath)

	select {
	case <-ctx.Done():
		log.Printf("mydockerd: shutting down (timeout %s)", shutdownTimeout)

		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()

		if err := s.Shutdown(shutdownCtx); err != nil {
			fmt.Fprintf(os.Stderr, "shutdown: %v\n", err)
			return 1
		}
		return 0
	case err := <-errCh:
		if err != nil {
			fmt.Fprintf(os.Stderr, "server: %v\n", err)
			return 1
		}
		return 0
	}

}

// parseHost strips the "unix://" scheme prefix and returns the bare socket
// path. It rejects non-unix schemes because the daemon only binds to a local
// UNIX socket.
func parseHost(host string) (string, error) {
	const prefix = "unix://"
	if !strings.HasPrefix(host, prefix) {
		return "", fmt.Errorf("host %q: only %q scheme is supported", host, prefix)
	}
	return host[len(prefix):], nil
}
