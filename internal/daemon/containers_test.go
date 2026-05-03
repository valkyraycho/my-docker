//go:build linux

package daemon_test

import (
	"context"
	"errors"
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

// panicResolver fails the test if Resolve is called. Used when a test
// wants to prove a code path does NOT reach image resolution (e.g. a
// request that should be rejected by earlier validation).
type panicResolver struct{}

func (panicResolver) Resolve(string) ([]string, error) {
	panic("Resolve should not be called")
}

// Panic stubs plugged into each Deps function field when a test leaves
// that field nil. If a test accidentally triggers a path it didn't
// configure, the panic message identifies which piece — far more
// useful than a nil-func call panic deep inside the HTTP layer.
func panicStartInit(*state.Container) error {
	panic("StartInit not configured for this test")
}
func panicStopInit(*state.Container, time.Duration) error {
	panic("StopInit not configured for this test")
}
func panicKillInit(*state.Container) error {
	panic("KillInit not configured for this test")
}
func panicRemoveInit(*state.Container) error {
	panic("RemoveInit not configured for this test")
}

// newTestDaemon starts a daemon on a tempdir socket with the supplied
// Deps template (minus Registry, which the helper creates). Nil fields
// in the template are filled in with panic-on-call defaults so tests
// opt into exactly the paths they exercise.
//
// Returns a client and the registry handle — tests seed or inspect
// state directly via the registry, exercise behavior via the client.
//
// Cleanup order (LIFO): daemon shutdown runs first, then state-dir
// restore. Shutdown writes final state while the override is still
// active; restoring the dir first would race.
func newTestDaemon(t *testing.T, template daemon.Deps) (*client.Client, *state.Registry) {
	t.Helper()

	if template.ImageStore == nil {
		template.ImageStore = panicResolver{}
	}
	if template.StartInit == nil {
		template.StartInit = panicStartInit
	}
	if template.StopInit == nil {
		template.StopInit = panicStopInit
	}
	if template.KillInit == nil {
		template.KillInit = panicKillInit
	}
	if template.RemoveInit == nil {
		template.RemoveInit = panicRemoveInit
	}

	tmp := t.TempDir()
	socketPath := filepath.Join(tmp, "mydocker.sock")
	t.Cleanup(state.WithTempDir(filepath.Join(tmp, "containers")))

	registry, err := state.NewRegistry()
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	template.Registry = registry

	s := daemon.New(socketPath, daemon.NewHandler(&template))

	errCh := make(chan error, 1)
	go func() { errCh <- s.Start() }()

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

	return client.New(socketPath), registry
}

// -------------------- create tests --------------------

func TestContainerCreate_Success(t *testing.T) {
	c, _ := newTestDaemon(t, daemon.Deps{
		ImageStore: &fakeResolver{layers: []string{"/fake/layer1", "/fake/layer2"}},
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
		t.Error("response Warnings is nil, want empty slice")
	}
}

func TestContainerCreate_ImageNotFound(t *testing.T) {
	c, _ := newTestDaemon(t, daemon.Deps{
		ImageStore: &fakeResolver{err: image.ErrImageNotFound},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	_, err := c.ContainerCreate(ctx, &api.ContainerCreateRequest{Image: "does-not-exist"})
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

func TestContainerCreate_MissingImageField(t *testing.T) {
	c, _ := newTestDaemon(t, daemon.Deps{ImageStore: panicResolver{}})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	_, err := c.ContainerCreate(ctx, &api.ContainerCreateRequest{Cmd: []string{"sh"}})
	if err == nil {
		t.Fatal("expected error for empty Image, got nil")
	}
	if !strings.Contains(err.Error(), "400") {
		t.Errorf("error %q: expected 400 status in message", err.Error())
	}
}

// -------------------- start tests --------------------

func seedCreated(t *testing.T, reg *state.Registry, id string) {
	t.Helper()
	c := &state.Container{
		ID:        id,
		Image:     "alpine",
		Layers:    []string{"/fake/layer1"},
		Command:   []string{"sh"},
		Status:    state.StatusCreated,
		CreatedAt: time.Now(),
	}
	if err := reg.Add(c); err != nil {
		t.Fatalf("seed Add: %v", err)
	}
}

func TestContainerStart_Success(t *testing.T) {
	const fakePID = 9999

	startCalled := false
	c, reg := newTestDaemon(t, daemon.Deps{
		StartInit: func(c *state.Container) error {
			startCalled = true
			c.Status = state.StatusRunning
			c.PID = fakePID
			c.StartedAt = time.Now()
			return nil
		},
	})

	const id = "aabbccddeeff"
	seedCreated(t, reg, id)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := c.ContainerStart(ctx, id); err != nil {
		t.Fatalf("ContainerStart: %v", err)
	}
	if !startCalled {
		t.Error("StartInit was never invoked")
	}

	got, err := reg.Get(id)
	if err != nil {
		t.Fatalf("Get after start: %v", err)
	}
	if got.Status != state.StatusRunning {
		t.Errorf("Status: got %q, want %q", got.Status, state.StatusRunning)
	}
	if got.PID != fakePID {
		t.Errorf("PID: got %d, want %d", got.PID, fakePID)
	}
}

func TestContainerStart_NotFound(t *testing.T) {
	c, _ := newTestDaemon(t, daemon.Deps{})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	err := c.ContainerStart(ctx, "nonexistentid")
	if err == nil {
		t.Fatal("expected error for missing container, got nil")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error %q: expected 404 status in message", err.Error())
	}
}

func TestContainerStart_AlreadyRunning(t *testing.T) {
	c, reg := newTestDaemon(t, daemon.Deps{}) // panic stub on StartInit

	const id = "runningone1"
	running := &state.Container{
		ID:        id,
		Image:     "alpine",
		Status:    state.StatusRunning,
		PID:       1234,
		CreatedAt: time.Now(),
		StartedAt: time.Now(),
	}
	if err := reg.Add(running); err != nil {
		t.Fatalf("seed Add: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := c.ContainerStart(ctx, id); err != nil {
		t.Fatalf("ContainerStart on already-running: want nil (304->nil), got %v", err)
	}
}

func TestContainerStart_InitFailure(t *testing.T) {
	c, reg := newTestDaemon(t, daemon.Deps{
		StartInit: func(*state.Container) error {
			return errors.New("simulated kernel failure")
		},
	})

	const id = "failerone11"
	seedCreated(t, reg, id)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	err := c.ContainerStart(ctx, id)
	if err == nil {
		t.Fatal("expected error from failing StartInit, got nil")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error %q: expected 500 status in message", err.Error())
	}

	got, err := reg.Get(id)
	if err != nil {
		t.Fatalf("Get after failed start: %v", err)
	}
	if got.Status != state.StatusCreated {
		t.Errorf("Status after failure: got %q, want %q",
			got.Status, state.StatusCreated)
	}
}

// -------------------- list / inspect tests --------------------

func seedStatus(t *testing.T, reg *state.Registry, id, status string, createdAt time.Time) {
	t.Helper()
	c := &state.Container{
		ID:        id,
		Image:     "alpine",
		Command:   []string{"sh"},
		Status:    status,
		CreatedAt: createdAt,
	}
	if status == state.StatusRunning {
		c.PID = 1234
		c.StartedAt = createdAt.Add(time.Second)
	}
	if status == state.StatusExited {
		c.StartedAt = createdAt.Add(time.Second)
		c.FinishedAt = createdAt.Add(2 * time.Second)
	}
	if err := reg.Add(c); err != nil {
		t.Fatalf("seed Add %s: %v", id, err)
	}
}

func TestContainerList_Empty(t *testing.T) {
	c, _ := newTestDaemon(t, daemon.Deps{})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	list, err := c.ContainerList(ctx, false)
	if err != nil {
		t.Fatalf("ContainerList: %v", err)
	}
	if list == nil {
		t.Error("expected empty slice, got nil")
	}
	if len(list) != 0 {
		t.Errorf("len: got %d, want 0", len(list))
	}
}

func TestContainerList_FiltersRunningOnly(t *testing.T) {
	c, reg := newTestDaemon(t, daemon.Deps{})

	now := time.Now()
	seedStatus(t, reg, "cccccccccccc", state.StatusCreated, now)
	seedStatus(t, reg, "rrrrrrrrrrrr", state.StatusRunning, now)
	seedStatus(t, reg, "eeeeeeeeeeee", state.StatusExited, now)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	list, err := c.ContainerList(ctx, false)
	if err != nil {
		t.Fatalf("ContainerList: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("len: got %d, want 1", len(list))
	}
	if list[0].ID != "rrrrrrrrrrrr" {
		t.Errorf("ID: got %q, want %q", list[0].ID, "rrrrrrrrrrrr")
	}
}

func TestContainerList_AllIncludesStopped(t *testing.T) {
	c, reg := newTestDaemon(t, daemon.Deps{})

	now := time.Now()
	seedStatus(t, reg, "cccccccccccc", state.StatusCreated, now)
	seedStatus(t, reg, "rrrrrrrrrrrr", state.StatusRunning, now)
	seedStatus(t, reg, "eeeeeeeeeeee", state.StatusExited, now)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	list, err := c.ContainerList(ctx, true)
	if err != nil {
		t.Fatalf("ContainerList: %v", err)
	}
	if len(list) != 3 {
		t.Errorf("len: got %d, want 3", len(list))
	}
}

func TestContainerList_SortedNewestFirst(t *testing.T) {
	c, reg := newTestDaemon(t, daemon.Deps{})

	base := time.Now()
	seedStatus(t, reg, "middlecccccc", state.StatusRunning, base.Add(-1*time.Hour))
	seedStatus(t, reg, "oldestcccccc", state.StatusRunning, base.Add(-2*time.Hour))
	seedStatus(t, reg, "newestcccccc", state.StatusRunning, base)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	list, err := c.ContainerList(ctx, false)
	if err != nil {
		t.Fatalf("ContainerList: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("len: got %d, want 3", len(list))
	}
	wantOrder := []string{"newestcccccc", "middlecccccc", "oldestcccccc"}
	for i, want := range wantOrder {
		if list[i].ID != want {
			t.Errorf("position %d: got %q, want %q", i, list[i].ID, want)
		}
	}
}

func TestContainerInspect_Success(t *testing.T) {
	c, reg := newTestDaemon(t, daemon.Deps{})

	const id = "inspectabc12"
	startedAt := time.Now().Add(-5 * time.Minute).UTC()
	seed := &state.Container{
		ID:        id,
		Image:     "alpine:3.19",
		Command:   []string{"sh", "-c", "echo hi"},
		Env:       []string{"FOO=bar"},
		Status:    state.StatusRunning,
		PID:       4242,
		IP:        "172.42.0.2",
		CreatedAt: startedAt.Add(-1 * time.Second),
		StartedAt: startedAt,
	}
	if err := reg.Add(seed); err != nil {
		t.Fatalf("seed Add: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	got, err := c.ContainerInspect(ctx, id)
	if err != nil {
		t.Fatalf("ContainerInspect: %v", err)
	}
	if got.ID != id {
		t.Errorf("ID: got %q, want %q", got.ID, id)
	}
	if got.Image != "alpine:3.19" {
		t.Errorf("Image: got %q", got.Image)
	}
	if got.Path != "sh" {
		t.Errorf("Path: got %q, want %q", got.Path, "sh")
	}
	if len(got.Args) != 2 || got.Args[0] != "-c" || got.Args[1] != "echo hi" {
		t.Errorf("Args: got %v, want [-c echo hi]", got.Args)
	}
	if got.IPAddress != "172.42.0.2" {
		t.Errorf("IPAddress: got %q", got.IPAddress)
	}
	if !got.State.Running {
		t.Error("State.Running: got false, want true")
	}
	if got.State.Pid != 4242 {
		t.Errorf("State.Pid: got %d, want 4242", got.State.Pid)
	}
	if len(got.Env) != 1 || got.Env[0] != "FOO=bar" {
		t.Errorf("Env: got %v", got.Env)
	}
	if _, err := time.Parse(time.RFC3339, got.Created); err != nil {
		t.Errorf("Created %q: not parseable as RFC3339: %v", got.Created, err)
	}
}

func TestContainerInspect_NotFound(t *testing.T) {
	c, _ := newTestDaemon(t, daemon.Deps{})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	_, err := c.ContainerInspect(ctx, "nonexistentid")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error %q: expected 404 in message", err.Error())
	}
}

// -------------------- stop / kill / remove tests --------------------

// TestContainerStop_Success: a running container stops successfully,
// the mock StopInit is invoked with the caller's timeout, and registry
// state transitions to exited.
func TestContainerStop_Success(t *testing.T) {
	var (
		stopCalled bool
		sawTimeout time.Duration
	)

	c, reg := newTestDaemon(t, daemon.Deps{
		StopInit: func(c *state.Container, timeout time.Duration) error {
			stopCalled = true
			sawTimeout = timeout
			c.Status = state.StatusExited
			c.FinishedAt = time.Now()
			return nil
		},
	})

	const id = "stoprunninge1"
	reg.Add(&state.Container{
		ID:        id,
		Image:     "alpine",
		Status:    state.StatusRunning,
		PID:       1234,
		CreatedAt: time.Now(),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := c.ContainerStop(ctx, id, 5*time.Second); err != nil {
		t.Fatalf("ContainerStop: %v", err)
	}
	if !stopCalled {
		t.Fatal("StopInit was not invoked")
	}
	if sawTimeout != 5*time.Second {
		t.Errorf("timeout: got %v, want 5s", sawTimeout)
	}
	got, _ := reg.Get(id)
	if got.Status != state.StatusExited {
		t.Errorf("Status after stop: got %q, want %q", got.Status, state.StatusExited)
	}
}

// TestContainerStop_NotRunning: stop on an already-exited container
// returns 304 (nil to the client). The mock StopInit panics if called,
// proving the short-circuit works.
func TestContainerStop_NotRunning(t *testing.T) {
	c, reg := newTestDaemon(t, daemon.Deps{}) // panic stub

	const id = "alreadyexite"
	reg.Add(&state.Container{
		ID:         id,
		Image:      "alpine",
		Status:     state.StatusExited,
		CreatedAt:  time.Now(),
		FinishedAt: time.Now(),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := c.ContainerStop(ctx, id, 0); err != nil {
		t.Fatalf("ContainerStop on exited: want nil, got %v", err)
	}
}

// TestContainerStop_NotFound: unknown id -> 404.
func TestContainerStop_NotFound(t *testing.T) {
	c, _ := newTestDaemon(t, daemon.Deps{})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	err := c.ContainerStop(ctx, "nonexistentid", 0)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error %q: expected 404", err.Error())
	}
}

// TestContainerKill_Success: KillInit fires and container flips to exited.
func TestContainerKill_Success(t *testing.T) {
	killCalled := false
	c, reg := newTestDaemon(t, daemon.Deps{
		KillInit: func(c *state.Container) error {
			killCalled = true
			c.Status = state.StatusExited
			c.FinishedAt = time.Now()
			return nil
		},
	})

	const id = "killrunninge1"
	reg.Add(&state.Container{
		ID:        id,
		Image:     "alpine",
		Status:    state.StatusRunning,
		PID:       1234,
		CreatedAt: time.Now(),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := c.ContainerKill(ctx, id); err != nil {
		t.Fatalf("ContainerKill: %v", err)
	}
	if !killCalled {
		t.Error("KillInit was not invoked")
	}
	got, _ := reg.Get(id)
	if got.Status != state.StatusExited {
		t.Errorf("Status after kill: got %q", got.Status)
	}
}

// TestContainerRemove_Success: a stopped container gets removed cleanly.
// After the call reg.Get must return ErrNotFound — the container is
// gone from both memory and disk.
func TestContainerRemove_Success(t *testing.T) {
	removeCalled := false
	c, reg := newTestDaemon(t, daemon.Deps{
		RemoveInit: func(*state.Container) error {
			removeCalled = true
			return nil
		},
	})

	const id = "rmexitedabc1"
	reg.Add(&state.Container{
		ID:         id,
		Image:      "alpine",
		Status:     state.StatusExited,
		CreatedAt:  time.Now(),
		FinishedAt: time.Now(),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := c.ContainerRemove(ctx, id, false); err != nil {
		t.Fatalf("ContainerRemove: %v", err)
	}
	if !removeCalled {
		t.Error("RemoveInit was not invoked")
	}
	if _, err := reg.Get(id); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("Get after remove: expected ErrNotFound, got %v", err)
	}
}

// TestContainerRemove_RunningNoForce: 409 Conflict. Neither Stop nor
// Remove should fire — both panic stubs would surface that bug.
func TestContainerRemove_RunningNoForce(t *testing.T) {
	c, reg := newTestDaemon(t, daemon.Deps{}) // panic stubs everywhere

	const id = "runningnoforc"
	reg.Add(&state.Container{
		ID:        id,
		Image:     "alpine",
		Status:    state.StatusRunning,
		PID:       1234,
		CreatedAt: time.Now(),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	err := c.ContainerRemove(ctx, id, false)
	if err == nil {
		t.Fatal("expected 409, got nil")
	}
	if !strings.Contains(err.Error(), "409") {
		t.Errorf("error %q: expected 409 status in message", err.Error())
	}
	// Container must still exist.
	if _, err := reg.Get(id); err != nil {
		t.Errorf("container should still be present: %v", err)
	}
}

// TestContainerRemove_RunningWithForce: force=1 stops-then-removes.
// Verifies StopInit runs BEFORE RemoveInit (they both succeed here;
// we just confirm both fired).
func TestContainerRemove_RunningWithForce(t *testing.T) {
	var order []string
	c, reg := newTestDaemon(t, daemon.Deps{
		StopInit: func(c *state.Container, _ time.Duration) error {
			order = append(order, "stop")
			c.Status = state.StatusExited
			c.FinishedAt = time.Now()
			return nil
		},
		RemoveInit: func(*state.Container) error {
			order = append(order, "remove")
			return nil
		},
	})

	const id = "forcerunningx"
	reg.Add(&state.Container{
		ID:        id,
		Image:     "alpine",
		Status:    state.StatusRunning,
		PID:       1234,
		CreatedAt: time.Now(),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := c.ContainerRemove(ctx, id, true); err != nil {
		t.Fatalf("ContainerRemove force: %v", err)
	}
	if len(order) != 2 || order[0] != "stop" || order[1] != "remove" {
		t.Errorf("call order: got %v, want [stop remove]", order)
	}
	if _, err := reg.Get(id); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("container should be gone: %v", err)
	}
}

// TestContainerRemove_NotFound: unknown id -> 404.
func TestContainerRemove_NotFound(t *testing.T) {
	c, _ := newTestDaemon(t, daemon.Deps{})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	err := c.ContainerRemove(ctx, "nonexistentid", false)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error %q: expected 404", err.Error())
	}
}
