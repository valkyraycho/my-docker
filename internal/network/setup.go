//go:build linux

package network

import (
	"errors"
	"fmt"
)

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
