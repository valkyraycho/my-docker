package network

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
)

const (
	// subnet is the /24 block from which container IPs are handed out.
	subnet = "172.42.0.0/24"
	// gatewayIP is the bridge address — it is reserved and never allocated to a container.
	gatewayIP = "172.42.0.1"
	// allocFile is the JSON file that persists the containerID→IP mapping across daemon restarts.
	allocFile = "/var/lib/mydocker/network/allocated_ips.json"
)

// allocation records a single containerID→IP lease persisted in allocFile.
type allocation struct {
	ContainerID string `json:"container_id"`
	IP          string `json:"ip"`
}

// AllocateIP picks the first free IP in the subnet (skipping the gateway) and
// records the containerID→IP lease in allocFile. The allocation is written
// atomically via a temp-file rename to avoid corruption on concurrent writes.
// Returns the allocated IP string (e.g. "172.42.0.2").
func AllocateIP(containerID string) (string, error) {
	allocations, err := readIPAllocations()
	if err != nil {
		return "", fmt.Errorf("read ip allocations: %w", err)
	}

	used := make(map[string]struct{}, len(allocations))
	for _, a := range allocations {
		used[a.IP] = struct{}{}
	}

	candidates, err := ipRange(subnet)
	if err != nil {
		return "", fmt.Errorf("compute ip range: %w", err)
	}

	var picked string
	for _, c := range candidates {
		if _, ok := used[c]; !ok {
			picked = c
			break
		}
	}

	if picked == "" {
		return "", errors.New("no free IPs in subnet")
	}

	allocations = append(allocations, allocation{
		ContainerID: containerID,
		IP:          picked,
	})

	if err := writeAllocations(allocations); err != nil {
		return "", err
	}

	return picked, nil
}

// writeAllocations serializes the allocation slice to allocFile using an
// atomic write (write to .tmp, then rename) so a crash mid-write cannot
// leave a partially written JSON file.
func writeAllocations(allocations []allocation) error {
	if err := os.MkdirAll(filepath.Dir(allocFile), 0755); err != nil {
		return fmt.Errorf("mkdir allocation dir: %w", err)
	}

	b, err := json.MarshalIndent(allocations, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal allocations: %w", err)
	}

	tmpAllocFile := allocFile + ".tmp"
	if err := os.WriteFile(tmpAllocFile, b, 0644); err != nil {
		return fmt.Errorf("write temp allocation file %s: %w", tmpAllocFile, err)
	}
	if err := os.Rename(tmpAllocFile, allocFile); err != nil {
		return fmt.Errorf("rename allocation file: %w", err)
	}
	return nil
}

// readIPAllocations loads current leases from allocFile. A missing file is
// treated as an empty pool (first-run case), not an error.
func readIPAllocations() ([]allocation, error) {
	b, err := os.ReadFile(allocFile)
	if err != nil {
		switch {
		case errors.Is(err, os.ErrNotExist):
			return []allocation{}, nil
		default:
			return nil, fmt.Errorf("read %s: %w", allocFile, err)
		}
	}

	var allocations []allocation
	if err := json.Unmarshal(b, &allocations); err != nil {
		return nil, fmt.Errorf("parse %s: %w", allocFile, err)
	}

	return allocations, nil
}

// ipRange returns all assignable host IPs in cidr, excluding the network
// address, broadcast address, and the gateway IP. For a /24 that is up to 253
// addresses (.2–.254, skipping .1 which is the gateway).
func ipRange(cidr string) ([]string, error) {
	_, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, fmt.Errorf("parse CIDR %s: %w", cidr, err)
	}

	base := ipnet.IP.To4()
	if base == nil {
		return nil, errors.New("not an IPv4 subnet")
	}

	ones, bits := ipnet.Mask.Size()
	total := 1 << uint(bits-ones)

	gw := net.ParseIP(gatewayIP).To4()
	gwLast := int(gw[3])

	result := make([]string, 0, total-3)
	for i := 1; i < total-1; i++ {
		if i == gwLast {
			continue
		}

		result = append(result, net.IPv4(base[0], base[1], base[2], byte(i)).String())
	}
	return result, nil
}
// ReleaseIP removes the lease for containerID from allocFile, returning the IP
// to the free pool for future containers.
func ReleaseIP(containerID string) error {
	allocations, err := readIPAllocations()
	if err != nil {
		return fmt.Errorf("read ip allocations: %w", err)
	}

	allocations = slices.DeleteFunc(allocations, func(a allocation) bool {
		return a.ContainerID == containerID
	})

	return writeAllocations(allocations)
}
