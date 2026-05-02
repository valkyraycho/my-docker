package daemon

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/valkyraycho/my-docker/internal/api"
)

// TestHandlePing_Direct calls handlePing with a ResponseRecorder, no
// network involved. This catches handler-logic bugs: wrong status,
// wrong body, missing/incorrect headers. Runs in microseconds.
func TestHandlePing_Direct(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/_ping", nil)
	rec := httptest.NewRecorder()

	handlePing(rec, req)

	resp := rec.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status: got %d, want %d", resp.StatusCode, http.StatusOK)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if got, want := string(body), "OK\n"; got != want {
		t.Errorf("body: got %q, want %q", got, want)
	}

	// Header keys use canonical form ("Ostype", not "OSType") because
	// http.Header.Get canonicalizes the lookup key. See routes.go
	// teaching note about HTTP header canonicalization.
	wantHeaders := map[string]string{
		"Content-Type":    "text/plain; charset=utf-8",
		"Api-Version":     api.Version,
		"Ostype":          "linux",
		"Builder-Version": "",
	}
	for name, want := range wantHeaders {
		if got := resp.Header.Get(name); got != want {
			t.Errorf("header %q: got %q, want %q", name, got, want)
		}
	}
}

// TestNewHandler_PingRouted exercises the full mux through a real
// HTTP client. This proves "GET /_ping" is actually registered at
// that path, which the Direct test cannot check because it calls
// handlePing directly and bypasses the router.
func TestNewHandler_PingRouted(t *testing.T) {
	srv := httptest.NewServer(NewHandler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/_ping")
	if err != nil {
		t.Fatalf("GET /_ping: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status: got %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if got := resp.Header.Get("Api-Version"); got != api.Version {
		t.Errorf("Api-Version: got %q, want %q", got, api.Version)
	}
}

// TestNewHandler_WrongMethod asserts that non-GET verbs on /_ping
// return 405, not silently invoke handlePing. Go 1.22+ method-scoped
// patterns ("GET /_ping") provide this automatically; we lock it in
// so a future refactor to an unscoped pattern would fail CI.
func TestNewHandler_WrongMethod(t *testing.T) {
	srv := httptest.NewServer(NewHandler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/_ping", "text/plain", nil)
	if err != nil {
		t.Fatalf("POST /_ping: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status: got %d, want %d", resp.StatusCode, http.StatusMethodNotAllowed)
	}
}

// TestNewHandler_UnknownPath asserts unregistered paths return 404.
// Safety net against a future route accidentally acting as a catch-all.
func TestNewHandler_UnknownPath(t *testing.T) {
	srv := httptest.NewServer(NewHandler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/does-not-exist")
	if err != nil {
		t.Fatalf("GET /does-not-exist: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status: got %d, want %d", resp.StatusCode, http.StatusNotFound)
	}
}
