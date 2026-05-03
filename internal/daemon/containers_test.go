//go:build linux

package daemon_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/valkyraycho/my-docker/internal/api"
	"github.com/valkyraycho/my-docker/internal/client"
	"github.com/valkyraycho/my-docker/internal/daemon"
	"github.com/valkyraycho/my-docker/internal/image"
	"github.com/valkyraycho/my-docker/internal/state"
)

// fakeResolver is a test double for daemon.ImageResolver. Returns
// whatever layers/err the test configures. Zero-value produces an
// empty-layers, nil-error result — tests usually set at least one.
type fakeResolver struct {
	layers []string
	err    error
}

func (r *fakeResolver) Resolve(string) ([]string, error) {
	return r.layers, r.err
}

// newTestDaemon starts a daemon on a tempdir socket with the supplied
// ImageResolver and a fresh Registry backed by a tempdir. Returns a
// client pointing at it. All teardown (socket shutdown, state dir
// restore, goroutine drain) registers via t.Cleanup so tests only
// need to care about the assertion phase.
//
// Cleanup order (LIFO): daemon shutdown runs first, then state dir
// restore. That's intentional — shutdown writes final state while
// the override is still active; restoring the dir first would race.
func newTestDaemon(t *testing.T, resolver daemon.ImageResolver) *client.Client {
	t.Helper()

	tmp := t.TempDir()
	socketPath := filepath.Join(tmp, "mydocker.sock")

	// Override containersDir so Registry.Add writes into our tempdir.
	// Register restore FIRST so it runs LAST (Cleanup is LIFO).
	t.Cleanup(state.WithTempDir(filepath.Join(tmp, "containers")))

	registry, err := state.NewRegistry()
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	deps := &daemon.Deps{
		Registry:   registry,
		ImageStore: resolver,
	}

	s := daemon.New(socketPath, daemon.NewHandler(deps))

	errCh := make(chan error, 1)
	go func() { errCh <- s.Start() }()

	// Register daemon shutdown AFTER state restore so it runs FIRST
	// (see cleanup-order note on this function).
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

	waitForSocket(t, socketPath, 2*time.Second)

	return client.New(socketPath)
}

// TestContainerCreate_Success goes end-to-end through the real HTTP
// transport: client marshals, daemon parses, resolver returns layers,
// registry persists, daemon encodes the response. If this passes, the
// whole create path is working.
func TestContainerCreate_Success(t *testing.T) {
	c := newTestDaemon(t, &fakeResolver{
		layers: []string{"/fake/layer1", "/fake/layer2"},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	resp, err := c.ContainerCreate(ctx, &api.ContainerCreateRequest{
		Image: "alpine",
		Cmd:   []string{"sh"},
	})
	if err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}

	if resp.ID == "" {
		t.Error("response ID is empty")
	}
	if got, want := len(resp.ID), 12; got != want {
		t.Errorf("response ID length: got %d (%q), want %d", got, resp.ID, want)
	}
	if resp.Warnings == nil {
		// Wire contract: always emit an array, even if empty.
		t.Error("response Warnings is nil, want empty slice")
	}
}

// TestContainerCreate_ImageNotFound: resolver returns ErrImageNotFound,
// daemon maps to HTTP 404, client surfaces a non-nil error whose
// message includes the daemon's status + message. We do a loose
// substring match rather than an exact compare because the full
// error string depends on http.Status formatting and the wrapped
// image error text.
func TestContainerCreate_ImageNotFound(t *testing.T) {
	c := newTestDaemon(t, &fakeResolver{err: image.ErrImageNotFound})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	_, err := c.ContainerCreate(ctx, &api.ContainerCreateRequest{
		Image: "does-not-exist",
	})
	if err == nil {
		t.Fatal("expected error for missing image, got nil")
	}

	msg := err.Error()
	if !strings.Contains(msg, "404") {
		t.Errorf("error %q: expected 404 status in message", msg)
	}
	if !strings.Contains(strings.ToLower(msg), "not found") {
		t.Errorf("error %q: expected 'not found' in message", msg)
	}
}

// TestContainerCreate_MissingImageField: request with empty Image
// should get rejected at the validation step with 400, before the
// resolver is called. We verify this by using a resolver that would
// panic if invoked — proving we never reached it.
func TestContainerCreate_MissingImageField(t *testing.T) {
	c := newTestDaemon(t, panicResolver{})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	_, err := c.ContainerCreate(ctx, &api.ContainerCreateRequest{
		// Image intentionally omitted.
		Cmd: []string{"sh"},
	})
	if err == nil {
		t.Fatal("expected error for empty Image, got nil")
	}
	if !strings.Contains(err.Error(), "400") {
		t.Errorf("error %q: expected 400 status in message", err.Error())
	}
}

// panicResolver fails the test if Resolve is called. Used to prove
// that earlier validation rejects a request before it reaches the
// image layer.
type panicResolver struct{}

func (panicResolver) Resolve(string) ([]string, error) {
	panic("Resolve should not be called when request validation fails")
}
