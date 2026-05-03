package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/valkyraycho/my-docker/internal/client"
)

const defaultHost = "unix:///var/run/mydocker.sock"

var hostFlag string

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

func parseHost(host string) (string, error) {
	const prefix = "unix://"
	if !strings.HasPrefix(host, prefix) {
		return "", fmt.Errorf("host %q: only %q scheme is supported", host, prefix)
	}
	return host[len(prefix):], nil
}
