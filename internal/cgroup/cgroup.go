//go:build linux

// Package cgroup manages cgroup v2 resource limits for containers. Each
// container gets its own sub-cgroup under /sys/fs/cgroup/mydocker/<id>/.
// Limits are enforced by writing to the kernel's cgroup interface files
// (memory.max, cpu.max, pids.max); the kernel applies them immediately when
// a PID is added to the cgroup via cgroup.procs.
package cgroup

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const root = "/sys/fs/cgroup"
const parentName = "mydocker"

// Limits holds the resource caps for a container.
// Zero values mean "no limit" — skip writing that file.
type Limits struct {
	MemoryBytes int64 // bytes, 0 = unlimited
	CPUPercent  int   // 0-100, 0 = unlimited
	PidsMax     int   // max processes, 0 = unlimited
}

// Manager owns one container's cgroup.
type Manager struct {
	id   string // container ID
	path string // full path: /sys/fs/cgroup/mydocker/<id>
}

// New returns a Manager for the given container ID. It does not create anything
// on disk; call Create to allocate the cgroup.
func New(id string) *Manager {
	return &Manager{
		id:   id,
		path: filepath.Join(root, parentName, id),
	}
}

// Create allocates the cgroup hierarchy and writes resource limits. It first
// calls prepareRoot to ensure the root cgroup's controllers are enabled
// (cgroup v2 requires explicit opt-in via cgroup.subtree_control at each
// ancestor), then creates the per-container leaf directory and writes only
// the limits that are non-zero.
func (m *Manager) Create(l Limits) error {
	if err := prepareRoot(); err != nil {
		return fmt.Errorf("prepare root: %w", err)
	}

	cgroupParentDir := filepath.Join(root, parentName)
	if err := os.MkdirAll(cgroupParentDir, 0755); err != nil {
		return fmt.Errorf("create cgroup parent dir: %w", err)
	}

	if err := writeFile(filepath.Join(cgroupParentDir, "cgroup.subtree_control"), "+memory +cpu +pids"); err != nil {
		return fmt.Errorf("enable controllers: %w", err)
	}

	if err := os.MkdirAll(m.path, 0755); err != nil {
		return fmt.Errorf("create cgroup child dir: %w", err)
	}

	if l.MemoryBytes > 0 {
		if err := writeFile(filepath.Join(m.path, "memory.max"), strconv.FormatInt(l.MemoryBytes, 10)); err != nil {
			return fmt.Errorf("set memory limit: %w", err)
		}
	}

	if l.PidsMax > 0 {
		if err := writeFile(filepath.Join(m.path, "pids.max"), strconv.Itoa(l.PidsMax)); err != nil {
			return fmt.Errorf("set pids limit: %w", err)
		}
	}

	if l.CPUPercent > 0 {
		if err := writeFile(filepath.Join(m.path, "cpu.max"), formatCPU(l.CPUPercent)); err != nil {
			return fmt.Errorf("set cpu limit: %w", err)
		}
	}

	return nil
}

// prepareRoot moves existing root-cgroup processes into an "init" leaf so the
// root cgroup becomes a non-leaf, which is required before cgroup v2 will allow
// enabling controllers on it. Without this step, writing to
// /sys/fs/cgroup/cgroup.subtree_control fails with EBUSY.
func prepareRoot() error {
	initCgroup := filepath.Join(root, "init")
	if err := os.MkdirAll(initCgroup, 0755); err != nil {
		return fmt.Errorf("create init cgroup: %w", err)
	}

	procs, err := os.ReadFile(filepath.Join(root, "cgroup.procs"))
	if err != nil {
		return fmt.Errorf("read root procs: %w", err)
	}

	initProcs := filepath.Join(initCgroup, "cgroup.procs")
	for pid := range strings.FieldsSeq(string(procs)) {
		_ = os.WriteFile(initProcs, []byte(pid), 0644)
	}

	if err := writeFile(filepath.Join(root, "cgroup.subtree_control"),
		"+memory +cpu +pids"); err != nil {
		return fmt.Errorf("enable root controllers: %w", err)
	}

	return nil
}

// AddPID moves a process into this cgroup.
// After this, the kernel enforces the cgroup's limits on that PID.
func (m *Manager) AddPID(pid int) error {
	return writeFile(filepath.Join(m.path, "cgroup.procs"), strconv.Itoa(pid))
}

// Destroy removes the container's cgroup directory. The kernel only allows
// rmdir on an empty cgroup (no live PIDs), so this must be called after the
// container process has exited.
func (m *Manager) Destroy() error {
	if err := os.Remove(m.path); err != nil {
		return fmt.Errorf("remove cgroup %s: %w", m.path, err)
	}
	return nil
}

func writeFile(path, value string) error {
	if err := os.WriteFile(path, []byte(value), 0644); err != nil {
		return fmt.Errorf("write %s = %q: %w", path, value, err)
	}
	return nil
}

// formatCPU converts a percentage into the "quota period" format expected by
// cpu.max. For example, 50% becomes "50000 100000": the cgroup may use 50 ms
// out of every 100 ms period, which caps it at 0.5 CPUs regardless of how
// many cores the host has.
func formatCPU(percent int) string {
	period := 100000
	quota := period * percent / 100
	return fmt.Sprintf("%d %d", quota, period)
}
