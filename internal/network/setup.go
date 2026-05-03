//go:build linux

// Package network implements container networking using the Linux bridge model.
//
// On first use, it creates a host-side bridge (mydocker0) with gateway
// 172.42.0.1/24, then for each container: allocates a /24 IP, creates a veth
// pair (host side attached to the bridge, peer side moved into the container
// network namespace and renamed eth0), installs an iptables MASQUERADE rule so
// containers can reach the internet, and optionally installs DNAT rules for
// -p host:container port forwarding. This mirrors Docker's default docker0
// bridge network model.
package network

import (
	"errors"
	"fmt"
)

// Setup orchestrates the full network setup for a single container. It ensures
// the bridge and NAT rule exist, allocates a container IP, wires up the veth
// pair, writes /etc/resolv.conf, and installs any requested port-forward rules.
// On any failure after a partial setup, it rolls back the steps already taken.
// Returns the allocated IP address on success.
func Setup(containerID, rootfs string, pid int, ports []*PortSpec) (string, error) {
	if err := EnsureBridge(); err != nil {
		return "", fmt.Errorf("ensure bridge: %w", err)
	}

	if err := EnsureNAT(); err != nil {
		return "", fmt.Errorf("ensure NAT: %w", err)
	}

	ip, err := AllocateIP(containerID)
	if err != nil {
		return "", fmt.Errorf("allocate IP: %w", err)
	}

	if err := SetupVeth(containerID, pid, ip); err != nil {
		_ = ReleaseIP(containerID)
		return "", fmt.Errorf("setup veth: %w", err)
	}
	if err := WriteResolvConf(rootfs); err != nil {
		_ = RemoveVeth(containerID)
		_ = ReleaseIP(containerID)
		return "", fmt.Errorf("write resolv.conf: %w", err)
	}
	if err := PublishPorts(ip, ports); err != nil {
		_ = UnpublishPorts(ip, ports)
		_ = RemoveVeth(containerID)
		_ = ReleaseIP(containerID)
		return "", fmt.Errorf("publish ports: %w", err)
	}
	return ip, nil
}

// Teardown removes all network resources allocated for a container: port-forward
// rules, the veth pair, and the IP allocation. All steps are attempted even if
// one fails; errors are joined and returned together.
func Teardown(containerID string, ports []*PortSpec, ip string) error {
	var errs []error

	if err := UnpublishPorts(ip, ports); err != nil {
		errs = append(errs, fmt.Errorf("unpublish ports: %w", err))
	}

	if err := RemoveVeth(containerID); err != nil {
		errs = append(errs, fmt.Errorf("remove veth: %w", err))
	}
	if err := ReleaseIP(containerID); err != nil {
		errs = append(errs, fmt.Errorf("release IP: %w", err))
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}
