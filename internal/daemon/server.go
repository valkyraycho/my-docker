package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
)

type Server struct {
	socketPath string
	httpServer *http.Server
	listener   net.Listener
}

func New(socketPath string) *Server {
	mux := http.NewServeMux()
	return &Server{
		socketPath: socketPath,
		httpServer: &http.Server{Handler: mux},
	}
}

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

func (s *Server) Shutdown(ctx context.Context) error {
	if err := s.httpServer.Shutdown(ctx); err != nil {
		return fmt.Errorf("shutdown server: %w", err)
	}

	_ = os.Remove(s.socketPath)
	return nil
}

func (s *Server) SocketPath() string {
	return s.socketPath
}
