//go:build linux

package network

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

// SetupVeth creates a veth pair for the container identified by containerID,
// wires the host side into the mydocker0 bridge, moves the peer side into the
// container's network namespace (identified by pid), renames it to eth0, assigns
// the given IP, and adds a default route via the gateway. A veth pair is a
// virtual Ethernet cable: what goes in one end comes out the other, even across
// namespace boundaries.
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

// RemoveVeth deletes the host-side veth interface for containerID. When the host
// side is deleted, the kernel automatically destroys the peer inside the
// container namespace. Errors for already-missing interfaces are silenced.
// Equivalent to: ip link del v<containerID>
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

// createVethPair creates a linked veth pair: hostSide stays on the host, peerSide
// will be moved into the container namespace.
// Equivalent to: ip link add <hostSide> type veth peer name <peerSide>
func createVethPair(hostSide string, peerSide string) error {
	return run("ip", "link", "add", hostSide, "type", "veth", "peer", "name", peerSide)
}
// movePeerSideIntoNetns moves the peer veth into the network namespace of the
// process with the given pid. After this call, the interface is invisible to
// the host and visible only inside the container.
// Equivalent to: ip link set <peerSide> netns <pid>
func movePeerSideIntoNetns(peerSide string, pid int) error {
	return run("ip", "link", "set", peerSide, "netns", strconv.Itoa(pid))
}

// attachHostSideToBridge enslaves the host-side veth to the mydocker0 bridge,
// so frames from the container are forwarded to other bridge ports (and the host).
// Equivalent to: ip link set <hostSide> master mydocker0
func attachHostSideToBridge(hostSide string) error {
	return run("ip", "link", "set", hostSide, "master", bridgeName)
}

// bringHostSideUp transitions the host-side veth to UP state.
// Equivalent to: ip link set <hostSide> up
func bringHostSideUp(hostSide string) error {
	return run("ip", "link", "set", hostSide, "up")
}

// configureInsideNetns enters the container network namespace via nsenter and
// performs all interface configuration that must happen from inside that namespace:
// rename peerSide to eth0, bring up loopback, assign the IP, bring eth0 up, and
// add a default route through the bridge gateway.
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

// nsRun runs a command inside the network namespace of the process with the
// given pid using nsenter. It is the equivalent of prefixing any ip command
// with: nsenter -t <pid> -n <cmd> <args...>
func nsRun(pid int, cmd string, args ...string) error {
	all := append([]string{"-t", strconv.Itoa(pid), "-n", cmd}, args...)
	return run("nsenter", all...)
}

// vethHostName returns the name of the host-side veth for a container.
// Prefixed with "v" to stay within the 15-character Linux interface name limit.
func vethHostName(containerID string) string {
	return "v" + containerID
}

// vethPeerName returns the name of the container-side (peer) veth before it is
// renamed to eth0 inside the namespace.
func vethPeerName(containerID string) string {
	return "p" + containerID
}
