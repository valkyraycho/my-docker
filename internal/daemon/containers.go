//go:build linux

package daemon

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/valkyraycho/my-docker/internal/api"
	"github.com/valkyraycho/my-docker/internal/state"
)

func (d *Deps) handleContainerCreate(w http.ResponseWriter, r *http.Request) {
	var req api.ContainerCreateRequest

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request: %w", err))
		return
	}

	if req.Image == "" {
		writeError(w, http.StatusBadRequest, errors.New("image is required"))
		return
	}

	id := newContainerID()

	container := state.Container{
		ID:        id,
		Image:     req.Image,
		Command:   req.Cmd,
		Status:    state.StatusCreated,
		CreatedAt: time.Now(),
	}

	if err := d.Registry.Add(&container); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("add container: %w", err))
		return
	}

	writeJSON(w, http.StatusCreated, api.ContainerCreateResponse{
		ID:       id,
		Warnings: []string{},
	})
}

func newContainerID() string {
	buf := make([]byte, 6)

	_, err := rand.Read(buf)
	if err != nil {
		panic(err)
	}
	return hex.EncodeToString(buf)
}

func writeJSON(w http.ResponseWriter, statusCode int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, statusCode int, err error) {
	writeJSON(w, statusCode, api.ErrorResponse{Message: err.Error()})
}

func statusForError(err error) int {
	switch {
	case errors.Is(err, state.ErrNotFound):
		return http.StatusNotFound
	default:
		return http.StatusInternalServerError
	}
}
