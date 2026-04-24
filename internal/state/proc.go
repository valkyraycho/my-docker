//go:build linux

package state

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

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

func IsRunning(pid int, wantStart uint64) bool {
	gotStart, err := ReadStartTime(pid)
	if err != nil {
		return false
	}

	return gotStart == wantStart
}
