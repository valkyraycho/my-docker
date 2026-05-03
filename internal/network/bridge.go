//go:build linux

package network

import (
	"bytes"
	"fmt"
	"net"
	"os/exec"
	"strings"
)

// bridgeName is the host-side Linux bridge that all containers attach to,
// analogous to Docker's docker0.
const bridgeName = "mydocker0"

// enableRouteLocalnet allows iptables DNAT rules that redirect traffic to
// 127.0.0.1 to work on the bridge interface. Without this sysctl, the kernel
// drops packets destined for loopback addresses arriving on non-loopback
// interfaces, breaking localhost port-forwarding from the host into containers.
// Equivalent to: sysctl -w net.ipv4.conf.mydocker0.route_localnet=1
func enableRouteLocalnet() error {
	return run("sysctl", "-w", fmt.Sprintf("net.ipv4.conf.%s.route_localnet=1",
		bridgeName))
}

// EnsureBridge creates and activates the mydocker0 bridge if it does not already
// exist, assigns the gateway IP, enables kernel IP forwarding (so the host can
// route between the bridge subnet and the outside), and enables route_localnet.
// It is idempotent: safe to call on every container start.
func EnsureBridge() error {
	if !bridgeExists() {
		if err := createBridge(); err != nil {
			return fmt.Errorf("create bridge %s: %w", bridgeName, err)
		}
		if err := assignGatewayIP(); err != nil {
			return fmt.Errorf("assign gateway IP: %w", err)
		}

	}

	if err := bringBridgeUp(); err != nil {
		return fmt.Errorf("activate gateway: %w", err)
	}
	if err := enableIPForwarding(); err != nil {
		return fmt.Errorf("enable ip forwarding: %w", err)
	}
	if err := enableRouteLocalnet(); err != nil { // ← ADD THIS
		return fmt.Errorf("enable route_localnet: %w", err)
	}

	return nil
}

// bridgeExists reports whether the mydocker0 bridge link is already present.
func bridgeExists() bool {
	c := exec.Command("ip", "link", "show", bridgeName)
	return c.Run() == nil
}

// createBridge adds the mydocker0 bridge device.
// Equivalent to: ip link add mydocker0 type bridge
func createBridge() error {
	return run("ip", "link", "add", bridgeName, "type", "bridge")
}

// assignGatewayIP assigns the gateway address (172.42.0.1/24) to the bridge.
// The bridge acts as the default router for all containers in the subnet.
// Equivalent to: ip addr add 172.42.0.1/24 dev mydocker0
func assignGatewayIP() error {
	_, ipnet, err := net.ParseCIDR(subnet)
	if err != nil {
		return fmt.Errorf("parse CIDR %s: %w", subnet, err)
	}
	ones, _ := ipnet.Mask.Size()
	return run("ip", "addr", "add", fmt.Sprintf("%s/%d", gatewayIP, ones), "dev", bridgeName)
}

// bringBridgeUp transitions the bridge interface to UP state so it can forward frames.
// Equivalent to: ip link set mydocker0 up
func bringBridgeUp() error {
	return run("ip", "link", "set", bridgeName, "up")
}

// enableIPForwarding turns on the kernel's IP forwarding so packets can be
// routed between the bridge subnet and external networks.
// Equivalent to: sysctl -w net.ipv4.ip_forward=1
func enableIPForwarding() error {
	return run("sysctl", "-w", "net.ipv4.ip_forward=1")
}

// RemoveBridge deletes the mydocker0 bridge device from the host.
// Equivalent to: ip link del mydocker0
func RemoveBridge() error {
	return run("ip", "link", "del", bridgeName)
}

// run executes an external command, capturing stderr into the returned error on
// failure. It is the shared helper used by all ip/iptables/sysctl calls in this
// package.
func run(cmd string, args ...string) error {
	c := exec.Command(cmd, args...)
	var stderr bytes.Buffer
	c.Stderr = &stderr
	if err := c.Run(); err != nil {
		return fmt.Errorf("%s %v: %w: %s",
			cmd, args, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}
