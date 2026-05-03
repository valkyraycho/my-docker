//go:build linux

// Package daemon implements the mydocker daemon: it listens on a UNIX socket,
// routes HTTP requests to container-management handlers, and owns the process
// lifetime of the server.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
)

// Server wires HTTP routes to their handlers and owns the UNIX socket listener.
// A Server holds a Deps struct that handlers read from; tests inject fakes there.
type Server struct {
	socketPath string
	httpServer *http.Server
	listener   net.Listener
}

// New creates a Server that will listen on socketPath and dispatch requests
// to handler. Call Start to begin accepting connections.
func New(socketPath string, handler http.Handler) *Server {
	return &Server{
		socketPath: socketPath,
		httpServer: &http.Server{Handler: handler},
	}
}

// Start removes any stale socket file, binds to the UNIX socket at
// s.socketPath, and blocks serving requests until Shutdown is called.
func (s *Server) Start() error {
	if err := os.Remove(s.socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove stale socket: %w", err)
	}

	listener, err := net.Listen("unix", s.socketPath)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", s.socketPath, err)
	}

	s.listener = listener

	if err := s.httpServer.Serve(listener); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Shutdown gracefully stops the HTTP server (waiting up to ctx's deadline for
// in-flight requests to finish) and removes the socket file.
func (s *Server) Shutdown(ctx context.Context) error {
	if err := s.httpServer.Shutdown(ctx); err != nil {
		return fmt.Errorf("shutdown server: %w", err)
	}

	_ = os.Remove(s.socketPath)
	return nil
}

// SocketPath returns the UNIX socket path this server listens on.
func (s *Server) SocketPath() string {
	return s.socketPath
}
