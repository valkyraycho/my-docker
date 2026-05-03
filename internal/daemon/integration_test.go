//go:build linux

package daemon_test

import (
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/valkyraycho/my-docker/internal/api"
	"github.com/valkyraycho/my-docker/internal/client"
	"github.com/valkyraycho/my-docker/internal/daemon"
)

// TestEndToEnd_Ping exercises the full chunk 1 pipeline:
//
//	daemon.Server.Start -> Unix socket -> NewHandler -> handlePing
//	  -> response headers -> client.Transport dial -> client.Ping
//
// If this passes, every seam in chunk 1 lines up. It uses t.TempDir()
// for the socket so no root is required — the test can run under go
// test with zero privileges.
//
// Package is daemon_test (external test package) so we exercise the
// library exactly as a real caller would, via its exported API only.
func TestEndToEnd_Ping(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "mydocker.sock")

	s := daemon.New(socketPath, daemon.NewHandler(nil))

	// Start the server in a goroutine; Start blocks until Shutdown.
	// Buffered channel so the goroutine can always send-and-exit,
	// even if the test bails before reading errCh.
	errCh := make(chan error, 1)
	go func() { errCh <- s.Start() }()

	// t.Cleanup runs on every exit path (pass, fail, skip). We stop
	// the server and drain the goroutine so it doesn't leak across
	// tests in the same package.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)

		select {
		case err := <-errCh:
			if err != nil {
				t.Logf("server returned error on shutdown: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Log("warning: Start() did not return after Shutdown")
		}
	})

	// Start() returns before the socket is bound (go statement races
	// with net.Listen). Poll until a dial succeeds or we time out.
	waitForSocket(t, socketPath, 2*time.Second)

	c := client.New(socketPath)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	ping, err := c.Ping(ctx)
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}

	if ping.APIVersion != api.Version {
		t.Errorf("APIVersion: got %q, want %q", ping.APIVersion, api.Version)
	}
	if ping.OSType != "linux" {
		t.Errorf("OSType: got %q, want %q", ping.OSType, "linux")
	}
	if ping.BuilderVersion != "" {
		t.Errorf("BuilderVersion: got %q, want empty string", ping.BuilderVersion)
	}
}

// waitForSocket polls a Unix socket path until a dial succeeds or
// the deadline expires. This works around the race between
// `go s.Start()` returning and net.Listen actually binding.
//
// In production, main.go could signal readiness through a channel
// exposed by Server (e.g. s.Ready() <-chan struct{}). For tests,
// polling is simple and obvious — we sacrifice a few milliseconds
// of startup time for an easier-to-read test.
func waitForSocket(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.Dial("unix", path)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("socket %q did not come up within %s", path, timeout)
}
