//go:build linux

// Package state owns the on-disk representation of containers under
// /var/lib/mydocker/containers/<id>/state.json, plus an in-memory registry
// that loads and caches those records at daemon start-up.
package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/valkyraycho/my-docker/internal/network"
	"github.com/valkyraycho/my-docker/internal/volume"
)

// Container lifecycle status values stored in state.json.
const (
	StatusCreated = "created"
	StatusRunning = "running"
	StatusExited  = "exited"
)

var containersDir = "/var/lib/mydocker/containers"

// Container is the persisted record of one mydocker container. It is written
// atomically (write to ".tmp" then rename) on every state transition so a
// crash never leaves a torn state.json on disk.
//
// A container is uniquely identified by the (PID, StartTime) tuple, not PID
// alone. Linux reuses PIDs after a process exits, so storing StartTime (read
// from /proc/<pid>/stat field 22 in jiffies) lets us confirm we are still
// looking at the original process and not an unrelated recycled PID.
type Container struct {
	// Identity
	ID      string   `json:"id"`      // 12-char hex, generated at run-time
	Image   string   `json:"image"`   // "library/alpine:3.19" — display only
	Layers  []string `json:"layers"`  // digest paths for overlay (in top-first order)
	Command []string `json:"command"` // argv, for `ps` display and debug

	// Runtime identity — the (PID, StartTime) tuple uniquely identifies the process
	PID       int    `json:"pid"`
	StartTime uint64 `json:"start_time"` // from /proc/<pid>/stat field 22 (jiffies)

	// Lifecycle
	Status     string    `json:"status"` // "running", "exited"
	ExitCode   int       `json:"exit_code,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	StartedAt  time.Time `json:"started_at,omitempty"`
	FinishedAt time.Time `json:"finished_at,omitempty"`

	IP string `json:"ip,omitempty"`

	Volumes []*volume.Spec `json:"volumes,omitempty"`

	Ports []*network.PortSpec `json:"ports,omitempty"`
}

// Save serializes c to <containersDir>/<id>/state.json using a tmp+rename for
// atomicity. Safe to call whenever state changes (create, start, stop).
func (c *Container) Save() error {
	d := containerStateDir(c.ID)
	if err := os.MkdirAll(d, 0755); err != nil {
		return fmt.Errorf("mkdir container state %s: %w", d, err)
	}
	statePath := filepath.Join(d, "state.json")
	tmpStatePath := statePath + ".tmp"

	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal container state: %w", err)
	}

	if err := os.WriteFile(tmpStatePath, b, 0644); err != nil {
		return fmt.Errorf("write state to %s: %w", tmpStatePath, err)
	}

	if err := os.Rename(tmpStatePath, statePath); err != nil {
		return fmt.Errorf("rename %s to %s: %w", tmpStatePath, statePath, err)
	}
	return nil
}

// Load reads and deserializes the state.json for the container with the given
// ID. Returns an error wrapping os.ErrNotExist if the container directory does
// not exist.
func Load(id string) (*Container, error) {
	statePath := filepath.Join(containerStateDir(id), "state.json")

	b, err := os.ReadFile(statePath)
	if err != nil {
		return nil, fmt.Errorf("read state %s: %w", statePath, err)
	}

	var c Container
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("unmarshal state %s: %w", statePath, err)
	}

	return &c, nil
}

func containerStateDir(id string) string {
	return filepath.Join(containersDir, id)
}

// StdoutPath returns the path to the captured stdout log for a container.
func StdoutPath(id string) string {
	return filepath.Join(containersDir, id, "stdout.log")
}

// StderrPath returns the path to the captured stderr log for a container.
func StderrPath(id string) string {
	return filepath.Join(containersDir, id, "stderr.log")
}

// RemoveDir deletes the entire state directory for the given container ID.
func RemoveDir(id string) error {
	return os.RemoveAll(containerStateDir(id))
}
