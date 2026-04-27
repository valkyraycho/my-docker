//go:build linux

package network

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

func SetupVeth(containerID string, pid int, ip string) error {
	hostSide := vethHostName(containerID)
	peerSide := vethPeerName(containerID)

	_, ipnet, err := net.ParseCIDR(subnet)
	if err != nil {
		return fmt.Errorf("parse CIDR %s: %w", subnet, err)
	}

	ones, _ := ipnet.Mask.Size()
	ipCIDR := fmt.Sprintf("%s/%d", ip, ones)

	if err := createVethPair(hostSide, peerSide); err != nil {
		return fmt.Errorf("create veth pair: %w", err)
	}

	if err := movePeerSideIntoNetns(peerSide, pid); err != nil {
		return fmt.Errorf("move peer side into netns: %w", err)
	}

	if err := attachHostSideToBridge(hostSide); err != nil {
		return fmt.Errorf("attach host side to bridge: %w", err)
	}

	if err := bringHostSideUp(hostSide); err != nil {
		return fmt.Errorf("activate host side: %w", err)
	}

	if err := configureInsideNetns(pid, peerSide, ipCIDR); err != nil {
		return fmt.Errorf("configure inside netns: %w", err)
	}

	return nil
}

func RemoveVeth(containerID string) error {
	err := run("ip", "link", "del", vethHostName(containerID))
	if err != nil {
		if strings.Contains(err.Error(), "does not exist") ||
			strings.Contains(err.Error(), "Cannot find device") {
			return nil
		}
		return err
	}
	return nil
}

func createVethPair(hostSide string, peerSide string) error {
	return run("ip", "link", "add", hostSide, "type", "veth", "peer", "name", peerSide)
}
func movePeerSideIntoNetns(peerSide string, pid int) error {
	return run("ip", "link", "set", peerSide, "netns", strconv.Itoa(pid))
}

func attachHostSideToBridge(hostSide string) error {
	return run("ip", "link", "set", hostSide, "master", bridgeName)
}

func bringHostSideUp(hostSide string) error {
	return run("ip", "link", "set", hostSide, "up")
}

func configureInsideNetns(pid int, peerSide string, ipCIDR string) error {
	if err := nsRun(pid, "ip", "link", "set", peerSide, "name", "eth0"); err != nil {
		return fmt.Errorf("rename veth peer to eth0: %w", err)
	}

	if err := nsRun(pid, "ip", "link", "set", "lo", "up"); err != nil {
		return fmt.Errorf("activate loopback: %w", err)
	}

	if err := nsRun(pid, "ip", "addr", "add", ipCIDR, "dev", "eth0"); err != nil {
		return fmt.Errorf("assign ip to eth0: %w", err)
	}

	if err := nsRun(pid, "ip", "link", "set", "eth0", "up"); err != nil {
		return fmt.Errorf("bring eth0 up: %w", err)
	}

	if err := nsRun(pid, "ip", "route", "add", "default", "via", gatewayIP); err != nil {
		return fmt.Errorf("add default route: %w", err)
	}

	return nil

}

func nsRun(pid int, cmd string, args ...string) error {
	all := append([]string{"-t", strconv.Itoa(pid), "-n", cmd}, args...)
	return run("nsenter", all...)
}

func vethHostName(containerID string) string {
	return "v" + containerID
}

func vethPeerName(containerID string) string {
	return "p" + containerID
}
