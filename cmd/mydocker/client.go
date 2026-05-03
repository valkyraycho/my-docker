// Package main — see main.go for the package doc comment.
package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/valkyraycho/my-docker/internal/client"
)

// defaultHost is the daemon UNIX socket address used when neither --host nor
// MYDOCKER_HOST are set.
const defaultHost = "unix:///var/run/mydocker.sock"

// hostFlag holds the value of the global --host / -H flag registered in main.go.
var hostFlag string

// getClient resolves the daemon address (flag → env → default) and returns a
// client.Client dialling that UNIX socket.
func getClient() (*client.Client, error) {
	host := hostFlag
	if host == "" {
		host = os.Getenv("MYDOCKER_HOST")
	}

	if host == "" {
		host = defaultHost
	}

	socketPath, err := parseHost(host)
	if err != nil {
		return nil, fmt.Errorf("parse host: %w", err)
	}

	return client.New(socketPath), nil
}

// parseHost strips the "unix://" scheme prefix and returns the bare socket
// path. It rejects any non-unix scheme because the daemon only speaks over a
// UNIX socket.
func parseHost(host string) (string, error) {
	const prefix = "unix://"
	if !strings.HasPrefix(host, prefix) {
		return "", fmt.Errorf("host %q: only %q scheme is supported", host, prefix)
	}
	return host[len(prefix):], nil
}
