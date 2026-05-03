//go:build linux

package network

import (
	"fmt"
	"os"
	"path/filepath"
)

// resolvConfContents is written verbatim to the container's /etc/resolv.conf.
// It points to Google's (8.8.8.8) and Cloudflare's (1.1.1.1) public resolvers.
const resolvConfContents = `nameserver 8.8.8.8
nameserver 1.1.1.1
`

// WriteResolvConf writes a minimal /etc/resolv.conf into the container's rootfs
// so that DNS resolution works inside the container. Without this file, libc's
// resolver falls back to only checking /etc/hosts, breaking hostname lookups.
// The file is created under <rootfs>/etc/resolv.conf, which the container sees
// as /etc/resolv.conf after its root is changed via chroot/pivot_root.
func WriteResolvConf(rootfs string) error {

	etcDir := filepath.Join(rootfs, "etc")
	if err := os.MkdirAll(etcDir, 0755); err != nil {
		return fmt.Errorf("mkdir %s: %w", etcDir, err)
	}

	confPath := filepath.Join(etcDir, "resolv.conf")
	if err := os.WriteFile(confPath, []byte(resolvConfContents), 0644); err != nil {
		return fmt.Errorf("write resolv.conf: %w", err)
	}
	return nil
}
