//go:build linux

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

func New(id string) *Manager {
	return &Manager{
		id:   id,
		path: filepath.Join(root, parentName, id),
	}
}

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

func formatCPU(percent int) string {
	period := 100000
	quota := period * percent / 100
	return fmt.Sprintf("%d %d", quota, period)
}
