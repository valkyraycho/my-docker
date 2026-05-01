//go:build linux

package network

import (
	"bytes"
	"fmt"
	"net"
	"os/exec"
	"strings"
)

const bridgeName = "mydocker0"

func enableRouteLocalnet() error {
	return run("sysctl", "-w", fmt.Sprintf("net.ipv4.conf.%s.route_localnet=1",
		bridgeName))
}

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

func bridgeExists() bool {
	c := exec.Command("ip", "link", "show", bridgeName)
	return c.Run() == nil
}

func createBridge() error {
	return run("ip", "link", "add", bridgeName, "type", "bridge")
}

func assignGatewayIP() error {
	_, ipnet, err := net.ParseCIDR(subnet)
	if err != nil {
		return fmt.Errorf("parse CIDR %s: %w", subnet, err)
	}
	ones, _ := ipnet.Mask.Size()
	return run("ip", "addr", "add", fmt.Sprintf("%s/%d", gatewayIP, ones), "dev", bridgeName)
}

func bringBridgeUp() error {
	return run("ip", "link", "set", bridgeName, "up")
}

func enableIPForwarding() error {
	return run("sysctl", "-w", "net.ipv4.ip_forward=1")
}

func RemoveBridge() error {
	return run("ip", "link", "del", bridgeName)
}

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
