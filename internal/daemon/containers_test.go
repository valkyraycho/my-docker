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

// panicStartInit is the default stub plugged into Deps.StartInit when
// a test passes nil. If a test accidentally triggers the start path
// without configuring behavior, this surfaces a clear message instead
// of a confusing nil-func panic deep inside http handling.
func panicStartInit(*state.Container) error {
	panic("StartInit not configured for this test")
}

// newTestDaemon starts a daemon on a tempdir socket with the given
// ImageResolver + StartInit and a fresh Registry backed by a tempdir.
// Returns a client pointing at the daemon AND the registry handle so
// tests can seed or inspect state directly.
//
// Passing nil for resolver or startInit substitutes a panic-on-call
// default — explicitly opting out of that path. If a test triggers a
// path it didn't configure, the panic message identifies which piece.
//
// Cleanup order (LIFO): daemon shutdown runs first, then state dir
// restore. Shutdown writes final state while the override is still
// active; restoring the dir first would race.
func newTestDaemon(
	t *testing.T,
	resolver daemon.ImageResolver,
	startInit func(*state.Container) error,
) (*client.Client, *state.Registry) {
	t.Helper()

	if resolver == nil {
		resolver = panicResolver{}
	}
	if startInit == nil {
		startInit = panicStartInit
	}

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
		StartInit:  startInit,
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

	return client.New(socketPath), registry
}

// TestContainerCreate_Success goes end-to-end through the real HTTP
// transport: client marshals, daemon parses, resolver returns layers,
// registry persists, daemon encodes the response. If this passes, the
// whole create path is working.
func TestContainerCreate_Success(t *testing.T) {
	c, _ := newTestDaemon(t, &fakeResolver{
		layers: []string{"/fake/layer1", "/fake/layer2"},
	}, nil)

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
	c, _ := newTestDaemon(t, &fakeResolver{err: image.ErrImageNotFound}, nil)

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
	c, _ := newTestDaemon(t, panicResolver{}, nil)

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

// -------------------- container start tests --------------------

// seedCreated inserts a "created" container into the registry for use
// in start tests. Shared to keep setup noise out of each test body.
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

// TestContainerStart_Success: happy path. The fake StartInit pretends
// to have forked successfully by setting runtime fields on c. After
// the call, the registry should reflect those fields — proving the
// handler persisted via Update.
func TestContainerStart_Success(t *testing.T) {
	const fakePID = 9999

	startCalled := false
	c, reg := newTestDaemon(t, nil, func(c *state.Container) error {
		startCalled = true
		c.Status = state.StatusRunning
		c.PID = fakePID
		c.StartedAt = time.Now()
		return nil
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

	// Registry should show the mutations the fake made.
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

// TestContainerStart_NotFound: unknown ID gets mapped to 404. StartInit
// must never be called for a container that doesn't exist; the panic
// stub surfaces that violation clearly if it regresses.
func TestContainerStart_NotFound(t *testing.T) {
	c, _ := newTestDaemon(t, nil, nil) // nil -> panic stubs

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

// TestContainerStart_AlreadyRunning: daemon returns 304 Not Modified
// and the client translates that to nil. The StartInit stub would
// panic if called — proving the 304 short-circuit fires before any
// attempt to start.
func TestContainerStart_AlreadyRunning(t *testing.T) {
	c, reg := newTestDaemon(t, nil, nil) // panic stub — must not be called

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

// TestContainerStart_InitFailure: StartInit returns an error, daemon
// maps to 500, client surfaces the error with the daemon's message.
// The registry state should remain "created" — the handler only
// persists on a successful StartInit.
func TestContainerStart_InitFailure(t *testing.T) {
	c, reg := newTestDaemon(t, nil, func(*state.Container) error {
		return errors.New("simulated kernel failure")
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

	// Registry should be untouched — Status still "created".
	got, err := reg.Get(id)
	if err != nil {
		t.Fatalf("Get after failed start: %v", err)
	}
	if got.Status != state.StatusCreated {
		t.Errorf("Status after failure: got %q, want %q (handler should not persist)",
			got.Status, state.StatusCreated)
	}
}

// -------------------- container list / inspect tests --------------------

// seedStatus adds a container with the given status and timestamp.
// Used to build fixtures that exercise the list endpoint's filter
// and ordering logic.
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

// TestContainerList_Empty: no containers -> empty slice, never nil.
// Important because Go's json.Marshal on a nil slice emits `null`, which
// would break clients expecting to iterate an array. The daemon's
// handler explicitly allocates an empty slice to avoid that.
func TestContainerList_Empty(t *testing.T) {
	c, _ := newTestDaemon(t, nil, nil)

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

// TestContainerList_FiltersRunningOnly: with all=false the endpoint
// hides non-running containers. Matches `docker ps` default behavior.
func TestContainerList_FiltersRunningOnly(t *testing.T) {
	c, reg := newTestDaemon(t, nil, nil)

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

// TestContainerList_AllIncludesStopped: with all=true all three
// statuses come back. Matches `docker ps -a`.
func TestContainerList_AllIncludesStopped(t *testing.T) {
	c, reg := newTestDaemon(t, nil, nil)

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

// TestContainerList_SortedNewestFirst: handler sorts by CreatedAt desc
// so the most recent container shows up first. Matches Docker's UX
// where newly-created containers appear at the top of `ps` output.
func TestContainerList_SortedNewestFirst(t *testing.T) {
	c, reg := newTestDaemon(t, nil, nil)

	base := time.Now()
	// Intentionally seed out of chronological order to prove the sort
	// is doing work, not just preserving insertion order.
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

// TestContainerInspect_Success: seed a fully populated container,
// inspect it, verify the projection lands every field in the right
// place. Catches regressions in the toInspect / toState / toPorts /
// toMounts helpers.
func TestContainerInspect_Success(t *testing.T) {
	c, reg := newTestDaemon(t, nil, nil)

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
	// Path/Args split: argv[0] and argv[1:]
	if got.Path != "sh" {
		t.Errorf("Path: got %q, want %q", got.Path, "sh")
	}
	if len(got.Args) != 2 || got.Args[0] != "-c" || got.Args[1] != "echo hi" {
		t.Errorf("Args: got %v, want [-c echo hi]", got.Args)
	}
	if got.IPAddress != "172.42.0.2" {
		t.Errorf("IPAddress: got %q", got.IPAddress)
	}
	// Nested State object carries the Running boolean + PID.
	if !got.State.Running {
		t.Error("State.Running: got false, want true")
	}
	if got.State.Pid != 4242 {
		t.Errorf("State.Pid: got %d, want 4242", got.State.Pid)
	}
	if len(got.Env) != 1 || got.Env[0] != "FOO=bar" {
		t.Errorf("Env: got %v", got.Env)
	}
	// Created timestamp should round-trip as RFC3339 UTC.
	if _, err := time.Parse(time.RFC3339, got.Created); err != nil {
		t.Errorf("Created %q: not parseable as RFC3339: %v", got.Created, err)
	}
}

// TestContainerInspect_NotFound: unknown id -> 404 surfaced by the
// client as an error whose message includes the status. Parallels
// the NotFound test shape used elsewhere in this file.
func TestContainerInspect_NotFound(t *testing.T) {
	c, _ := newTestDaemon(t, nil, nil)

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
