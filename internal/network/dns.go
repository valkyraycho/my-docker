//go:build linux

package network

import (
	"fmt"
	"os"
	"path/filepath"
)

const resolvConfContents = `nameserver 8.8.8.8
nameserver 1.1.1.1
`

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
