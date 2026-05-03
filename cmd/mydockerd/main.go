//go:build linux

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

	"github.com/valkyraycho/my-docker/internal/daemon"
	"github.com/valkyraycho/my-docker/internal/state"
	"golang.org/x/sys/unix"
)

const defaultHost = "unix:///var/run/mydocker.sock"

const shutdownTimeout = 15 * time.Second

func main() {
	os.Exit(run())
}

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

	deps := &daemon.Deps{
		Registry: registry,
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
func parseHost(host string) (string, error) {
	const prefix = "unix://"
	if !strings.HasPrefix(host, prefix) {
		return "", fmt.Errorf("host %q: only %q scheme is supported", host, prefix)
	}
	return host[len(prefix):], nil
}
