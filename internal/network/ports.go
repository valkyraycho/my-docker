//go:build linux

package network

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

type PortSpec struct {
	HostPort      int    `json:"host_port"`
	ContainerPort int    `json:"container_port"`
	Protocol      string `json:"protocol"`
}

func ParsePortSpec(s string) (*PortSpec, error) {
	specs := strings.Split(s, ":")
	if len(specs) != 2 {
		return nil, fmt.Errorf("invalid port spec %q, expected host:container", s)
	}

	hostPort, err := strconv.Atoi(specs[0])
	if err != nil {
		return nil, fmt.Errorf("invalid host port %q: %w", specs[0], err)
	}

	if hostPort < 1 || hostPort > 65535 {
		return nil, fmt.Errorf("invalid host port %d, must be between 1 and 65535", hostPort)
	}

	containerPort, err := strconv.Atoi(specs[1])
	if err != nil {
		return nil, fmt.Errorf("invalid container port %q: %w", specs[1], err)
	}

	if containerPort < 1 || containerPort > 65535 {
		return nil, fmt.Errorf("invalid container port %d, must be between 1 and 65535", containerPort)
	}

	return &PortSpec{
		HostPort:      hostPort,
		ContainerPort: containerPort,
		Protocol:      "tcp",
	}, nil
}

func PublishPorts(containerIP string, specs []*PortSpec) error {
	var installed []*PortSpec

	rollback := func() {
		for _, spec := range installed {
			_ = runIPTablesDelete("PREROUTING", containerIP, spec, true)
			_ = runIPTablesDelete("OUTPUT", containerIP, spec, true)
		}
	}

	for _, spec := range specs {
		if err := runIPTablesAppend("PREROUTING", containerIP, spec, false); err != nil {
			rollback()
			return err
		}
		if err := runIPTablesAppend("OUTPUT", containerIP, spec, true); err != nil {
			_ = runIPTablesDelete("PREROUTING", containerIP, spec, false)
			rollback()
			return err
		}
		installed = append(installed, spec)
	}
	return nil
}

func UnpublishPorts(containerIP string, specs []*PortSpec) error {
	var errs []error
	for _, spec := range specs {
		if err := runIPTablesDelete("PREROUTING", containerIP, spec, false); err != nil {
			if !isNoSuchRule(err) {
				errs = append(errs, fmt.Errorf("unpublish prerouting %d: %w", spec.HostPort, err))
			}
		}
		if err := runIPTablesDelete("OUTPUT", containerIP, spec, true); err != nil {
			if !isNoSuchRule(err) {
				errs = append(errs, fmt.Errorf("unpublish output %d: %w", spec.HostPort, err))
			}
		}
	}

	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

func iptablesRule(action, chain, containerIP string, spec *PortSpec, outLoopback bool) []string {
	args := []string{"-t", "nat", action, chain,
		"-p", spec.Protocol,
		"--dport", strconv.Itoa(spec.HostPort)}
	if outLoopback {
		args = append(args, "-o", "lo")
	}
	args = append(args, "-j", "DNAT", "--to-destination",
		fmt.Sprintf("%s:%d", containerIP, spec.ContainerPort))
	return args
}

func runIPTablesAppend(chain, containerIP string, spec *PortSpec, outLoopback bool) error {
	return run("iptables", iptablesRule("-A", chain, containerIP, spec,
		outLoopback)...)
}

func runIPTablesDelete(chain, containerIP string, spec *PortSpec, outLoopback bool) error {
	return run("iptables", iptablesRule("-D", chain, containerIP, spec,
		outLoopback)...)
}
func isNoSuchRule(err error) bool {
	s := err.Error()
	return strings.Contains(s, "No chain/target/match by that name") ||
		strings.Contains(s, "does a matching rule exist")
}
