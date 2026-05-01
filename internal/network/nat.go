//go:build linux

package network

import "strings"

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

func ensureRule(chain string, args ...string) error {
	checkArgs := append([]string{"-t", "nat", "-C", chain}, args...)
	if err := run("iptables", checkArgs...); err == nil {
		return nil
	}
	addArgs := append([]string{"-t", "nat", "-A", chain}, args...)
	return run("iptables", addArgs...)
}
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
