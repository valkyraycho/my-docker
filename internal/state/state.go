//go:build linux

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

const (
	StatusCreated = "created"
	StatusRunning = "running"
	StatusExited  = "exited"
)

var containersDir = "/var/lib/mydocker/containers"

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

func StdoutPath(id string) string {
	return filepath.Join(containersDir, id, "stdout.log")
}
func StderrPath(id string) string {
	return filepath.Join(containersDir, id, "stderr.log")
}

func RemoveDir(id string) error {
	return os.RemoveAll(containerStateDir(id))
}
