//go:build linux

package main

import (
	"fmt"
	"os"

	"github.com/valkyraycho/my-docker/internal/container"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "usage: mydocker run <cmd> [args...]\n")
		os.Exit(1)
	}

	switch os.Args[1] {
	case "run":
		container.Run(os.Args)
	case "init":
		container.Init(os.Args)
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", os.Args[1])
		os.Exit(1)
	}
}
