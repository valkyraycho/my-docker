//go:build linux

package daemon

import (
	"net/http"

	"github.com/valkyraycho/my-docker/internal/api"
)

func NewHandler(deps *Deps) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /_ping", handlePing)
	_ = deps
	return mux
}

func handlePing(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Api-Version", api.Version)
	w.Header().Set("OSType", "linux")
	w.Header().Set("Builder-Version", "")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("OK\n"))
}
