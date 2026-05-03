//go:build linux

package network

import "strings"

// EnsureNAT installs a MASQUERADE rule in the iptables nat/POSTROUTING chain so
// that packets from the container subnet leaving the host on any interface other
// than the bridge have their source IP rewritten to the host's outbound IP.
// This is source NAT (SNAT) — it is what lets a container with a private
// 172.42.x.x address reach the public internet.
// Equivalent to:
//
//	iptables -t nat -A POSTROUTING -s 172.42.0.0/24 ! -o mydocker0 -j MASQUERADE
//
// It is idempotent: the rule is only added if not already present.
func EnsureNAT() error {
	if err := run("iptables", "-t", "nat", "-C", "POSTROUTING", "-s", subnet, "!", "-o", bridgeName, "-j", "MASQUERADE"); err == nil {
		return nil
	}
	if err := ensureRule("POSTROUTING", "-o", bridgeName, "!", "-s", subnet, "-j",
		"MASQUERADE"); err != nil {
		return err
	}

	return run("iptables", "-t", "nat", "-A", "POSTROUTING", "-s", subnet, "!", "-o", bridgeName, "-j", "MASQUERADE")
}

// ensureRule checks whether an iptables nat rule already exists (-C) and appends
// it (-A) only if it is absent. This prevents duplicate rules accumulating across
// daemon restarts.
func ensureRule(chain string, args ...string) error {
	checkArgs := append([]string{"-t", "nat", "-C", chain}, args...)
	if err := run("iptables", checkArgs...); err == nil {
		return nil
	}
	addArgs := append([]string{"-t", "nat", "-A", chain}, args...)
	return run("iptables", addArgs...)
}
// RemoveNAT deletes the MASQUERADE rule installed by EnsureNAT. A missing rule
// is treated as a no-op so teardown remains safe even if EnsureNAT was never
// called.
func RemoveNAT() error {
	err := run("iptables", "-t", "nat", "-D", "POSTROUTING", "-s", subnet, "!", "-o", bridgeName, "-j", "MASQUERADE")
	if err != nil {
		if strings.Contains(err.Error(), "does not exist") {
			return nil
		}
		return err
	}
	return nil
}
