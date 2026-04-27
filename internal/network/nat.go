//go:build linux

package network

import "strings"

func EnsureNAT() error {
	if err := run("iptables", "-t", "nat", "-C", "POSTROUTING", "-s", subnet, "!", "-o", bridgeName, "-j", "MASQUERADE"); err == nil {
		return nil
	}

	return run("iptables", "-t", "nat", "-A", "POSTROUTING", "-s", subnet, "!", "-o", bridgeName, "-j", "MASQUERADE")
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
