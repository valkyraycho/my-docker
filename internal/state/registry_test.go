//go:build linux

package state

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// useTempContainersDir points the package's containersDir at a
// per-test temp directory. Restored automatically when the test
// ends via t.Cleanup. Internal-package test so we can touch
// the unexported package var.
func useTempContainersDir(t *testing.T) string {
	t.Helper()
	old := containersDir
	t.Cleanup(func() { containersDir = old })
	containersDir = t.TempDir()
	return containersDir
}

// newContainer builds a valid *Container with the given id. Keeps
// each test's setup one line instead of 10.
func newContainer(id string) *Container {
	return &Container{
		ID:        id,
		Image:     "library/alpine:3.19",
		Command:   []string{"sh"},
		Status:    StatusRunning,
		CreatedAt: time.Now(),
	}
}

func TestNewRegistry_EmptyTempDir(t *testing.T) {
	useTempContainersDir(t)

	r, err := NewRegistry()
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	got, err := r.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty registry, got %d containers", len(got))
	}
}

func TestNewRegistry_MissingDir(t *testing.T) {
	old := containersDir
	t.Cleanup(func() { containersDir = old })
	containersDir = filepath.Join(t.TempDir(), "does-not-exist")

	r, err := NewRegistry()
	if err != nil {
		t.Fatalf("NewRegistry on missing dir should succeed, got: %v", err)
	}
	got, _ := r.List()
	if len(got) != 0 {
		t.Errorf("expected empty, got %d", len(got))
	}
}

func TestNewRegistry_LoadsExistingState(t *testing.T) {
	useTempContainersDir(t)

	// Seed the directory by using one Registry to Add, then rebuild.
	r1, _ := NewRegistry()
	if err := r1.Add(newContainer("abc123def456")); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := r1.Add(newContainer("111222333444")); err != nil {
		t.Fatalf("Add: %v", err)
	}

	r2, err := NewRegistry()
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	got, _ := r2.List()
	if len(got) != 2 {
		t.Errorf("expected 2 containers reloaded from disk, got %d", len(got))
	}
}

func TestNewRegistry_SkipsCorruptState(t *testing.T) {
	dir := useTempContainersDir(t)

	// One valid container via Add.
	r1, _ := NewRegistry()
	if err := r1.Add(newContainer("goodgoodgood")); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// One corrupt state file written by hand. Must live inside its
	// own directory (matches containerStateDir layout).
	corruptDir := filepath.Join(dir, "badbadbad")
	if err := os.MkdirAll(corruptDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(corruptDir, "state.json"),
		[]byte("not json"), 0644); err != nil {
		t.Fatalf("write corrupt: %v", err)
	}

	r2, err := NewRegistry()
	if err != nil {
		t.Fatalf("NewRegistry with corrupt file should skip, not fail: %v", err)
	}
	got, _ := r2.List()
	if len(got) != 1 {
		t.Errorf("expected only the good container, got %d", len(got))
	}
	if got[0].ID != "goodgoodgood" {
		t.Errorf("wrong container survived: %s", got[0].ID)
	}
}

func TestRegistry_AddGet(t *testing.T) {
	useTempContainersDir(t)
	r, _ := NewRegistry()

	c := newContainer("abc123456789")
	if err := r.Add(c); err != nil {
		t.Fatalf("Add: %v", err)
	}

	got, err := r.Get("abc123456789")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != c.ID {
		t.Errorf("Get returned %s, want %s", got.ID, c.ID)
	}
}

func TestRegistry_AddDuplicate(t *testing.T) {
	useTempContainersDir(t)
	r, _ := NewRegistry()

	c := newContainer("dup123456789")
	if err := r.Add(c); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := r.Add(c); err == nil {
		t.Error("expected error on duplicate Add, got nil")
	}
}

func TestRegistry_Get_NotFound(t *testing.T) {
	useTempContainersDir(t)
	r, _ := NewRegistry()

	_, err := r.Get("does-not-exist")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestRegistry_Update(t *testing.T) {
	useTempContainersDir(t)
	r, _ := NewRegistry()

	c := newContainer("upd123456789")
	if err := r.Add(c); err != nil {
		t.Fatalf("Add: %v", err)
	}

	c.Status = StatusExited
	c.ExitCode = 7
	if err := r.Update(c); err != nil {
		t.Fatalf("Update: %v", err)
	}

	// Rebuild registry from disk to confirm persistence.
	r2, _ := NewRegistry()
	got, err := r2.Get("upd123456789")
	if err != nil {
		t.Fatalf("Get after reload: %v", err)
	}
	if got.Status != StatusExited {
		t.Errorf("Status: got %q, want %q", got.Status, StatusExited)
	}
	if got.ExitCode != 7 {
		t.Errorf("ExitCode: got %d, want 7", got.ExitCode)
	}
}

func TestRegistry_Update_NotFound(t *testing.T) {
	useTempContainersDir(t)
	r, _ := NewRegistry()

	err := r.Update(newContainer("never-added"))
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestRegistry_Remove(t *testing.T) {
	dir := useTempContainersDir(t)
	r, _ := NewRegistry()

	c := newContainer("rm1234567890")
	if err := r.Add(c); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := r.Remove(c.ID); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	// Gone from memory.
	if _, err := r.Get(c.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get after Remove: expected ErrNotFound, got %v", err)
	}

	// Gone from disk.
	if _, err := os.Stat(filepath.Join(dir, c.ID)); !os.IsNotExist(err) {
		t.Errorf("state dir still exists on disk: err=%v", err)
	}
}

func TestRegistry_Remove_NotFound(t *testing.T) {
	useTempContainersDir(t)
	r, _ := NewRegistry()

	err := r.Remove("nonexistent")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestRegistry_Find(t *testing.T) {
	useTempContainersDir(t)
	r, _ := NewRegistry()

	mustAdd := func(id string) {
		t.Helper()
		if err := r.Add(newContainer(id)); err != nil {
			t.Fatalf("Add %s: %v", id, err)
		}
	}
	mustAdd("abc123456789")
	mustAdd("abc999999999")
	mustAdd("xyz000000000")

	tests := []struct {
		name       string
		prefix     string
		wantID     string
		wantErrIs  error  // errors.Is target; nil means expect no error
		wantSubstr string // expected in error message if wantErrIs is nil but wantID is ""
	}{
		{name: "exact full ID", prefix: "xyz000000000", wantID: "xyz000000000"},
		{name: "unique prefix", prefix: "xyz", wantID: "xyz000000000"},
		{name: "not found", prefix: "zzz", wantErrIs: ErrNotFound},
		{name: "empty prefix", prefix: "", wantSubstr: "empty prefix"},
		{name: "ambiguous", prefix: "abc", wantSubstr: "ambiguous"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := r.Find(tc.prefix)

			if tc.wantErrIs != nil {
				if !errors.Is(err, tc.wantErrIs) {
					t.Errorf("expected errors.Is %v, got %v", tc.wantErrIs, err)
				}
				return
			}
			if tc.wantSubstr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantSubstr) {
					t.Errorf("expected error containing %q, got %v", tc.wantSubstr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.ID != tc.wantID {
				t.Errorf("got ID %s, want %s", got.ID, tc.wantID)
			}
		})
	}
}

// TestRegistry_ConcurrentAccess proves the RWMutex actually protects
// the map from concurrent mutation. Must be run with `go test -race`
// to catch unsynchronized access in the future if anyone removes a
// lock. Without -race the test may pass even with a broken mutex.
func TestRegistry_ConcurrentAccess(t *testing.T) {
	useTempContainersDir(t)
	r, _ := NewRegistry()

	const goroutines = 20
	const opsPer = 50

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := range goroutines {
		go func(g int) {
			defer wg.Done()
			for i := range opsPer {
				// Build a deterministic but per-goroutine ID.
				id := makeID(g, i)
				_ = r.Add(newContainer(id))
				_, _ = r.Get(id)
				_, _ = r.List()
			}
		}(g)
	}
	wg.Wait()
}

// makeID produces a 12-char hex-ish ID for tests. Full IDs in
// production are random; here we just need uniqueness within the run.
func makeID(g, i int) string {
	const prefix = "g"
	s := prefix
	for _, n := range [2]int{g, i} {
		s += fmtHex(n)
	}
	for len(s) < 12 {
		s += "0"
	}
	return s[:12]
}

func fmtHex(n int) string {
	const hexdigits = "0123456789abcdef"
	if n == 0 {
		return "0"
	}
	out := ""
	for n > 0 {
		out = string(hexdigits[n%16]) + out
		n /= 16
	}
	return out
}
