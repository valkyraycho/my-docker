//go:build linux

package network

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// PortSpec is a parsed "-p host:container[/proto]" mapping — one DNAT rule.
type PortSpec struct {
	// HostPort is the port on the host that external traffic arrives on.
	HostPort int `json:"host_port"`
	// ContainerPort is the port inside the container that traffic is forwarded to.
	ContainerPort int `json:"container_port"`
	// Protocol is the IP protocol, currently always "tcp".
	Protocol string `json:"protocol"`
}

// ParsePortSpec parses a "-p host:container" string into a PortSpec. The
// protocol defaults to "tcp". Both port numbers are validated to be in the
// valid port range [1, 65535].
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

// PublishPorts installs iptables DNAT rules for each PortSpec so that traffic
// arriving on the host port is redirected to the container's IP and port. Two
// rules are installed per spec: one in PREROUTING (for traffic from outside the
// host) and one in OUTPUT (for traffic originating on the host itself, which
// bypasses PREROUTING). On any error, already-installed rules are rolled back.
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

// UnpublishPorts removes the DNAT rules added by PublishPorts. Missing rules are
// silently ignored so teardown is safe even for partially published containers.
// All specs are attempted; errors are joined and returned together.
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

// iptablesRule builds the iptables argument slice for a DNAT rule. When
// outLoopback is true it adds "-o lo" to match the OUTPUT chain rule, which
// handles connections from the host to its own published ports.
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

// runIPTablesAppend appends (-A) a DNAT rule for the given spec to chain.
func runIPTablesAppend(chain, containerIP string, spec *PortSpec, outLoopback bool) error {
	return run("iptables", iptablesRule("-A", chain, containerIP, spec,
		outLoopback)...)
}

// runIPTablesDelete deletes (-D) the matching DNAT rule from chain.
func runIPTablesDelete(chain, containerIP string, spec *PortSpec, outLoopback bool) error {
	return run("iptables", iptablesRule("-D", chain, containerIP, spec,
		outLoopback)...)
}
// isNoSuchRule returns true when an iptables delete fails because the rule was
// already absent, so callers can distinguish a clean no-op from a real error.
func isNoSuchRule(err error) bool {
	s := err.Error()
	return strings.Contains(s, "No chain/target/match by that name") ||
		strings.Contains(s, "does a matching rule exist")
}
