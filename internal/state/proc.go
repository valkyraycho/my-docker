//go:build linux

package state

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// ReadStartTime reads field 22 of /proc/<pid>/stat, which is the process start
// time expressed in clock ticks (jiffies) since system boot. This value is
// stable for the lifetime of the process and is used together with PID to
// detect PID reuse: if a new process has inherited the same PID but its
// StartTime differs, the original container process is gone.
func ReadStartTime(pid int) (uint64, error) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, fmt.Errorf("read stat of pid %d: %w", pid, err)
	}
	s := string(b)
	lastParamIdx := strings.LastIndexByte(s, ')')
	if lastParamIdx == -1 {
		return 0, fmt.Errorf("stat of pid %d is malformed", pid)
	}

	tail := s[lastParamIdx+2:]
	fields := strings.Fields(tail)

	if len(fields) < 20 {
		return 0, fmt.Errorf("stat of pid %d has %d fields, want >=20", pid, len(fields))
	}

	startTime, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse start time %q: %w", fields[19], err)
	}

	return startTime, nil
}

// IsRunning reports whether the process with the given PID is still the same
// process that was recorded with wantStart. Returns false if the process has
// exited or if a different process has recycled the PID.
func IsRunning(pid int, wantStart uint64) bool {
	gotStart, err := ReadStartTime(pid)
	if err != nil {
		return false
	}

	return gotStart == wantStart
}
