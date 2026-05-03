//go:build linux

package daemon

import (
	"net/http"

	"github.com/valkyraycho/my-docker/internal/api"
)

// NewHandler builds the root http.Handler for the daemon, registering all API
// routes on a new ServeMux. Pass a non-nil Deps to enable container routes;
// a nil Deps registers only the infrastructure endpoints (e.g. /_ping).
func NewHandler(deps *Deps) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /_ping", handlePing)
	if deps != nil {
		mux.HandleFunc("POST /containers/create", deps.handleContainerCreate)
		mux.HandleFunc("POST /containers/{id}/start", deps.handleContainerStart)
	}
	return mux
}

// handlePing implements GET /_ping. It returns 200 "OK" with the API version,
// OS type, and builder version in response headers — matching Docker's contract
// so that clients can negotiate capability before sending real requests.
func handlePing(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Api-Version", api.Version)
	w.Header().Set("OSType", "linux")
	w.Header().Set("Builder-Version", "")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("OK\n"))
}
