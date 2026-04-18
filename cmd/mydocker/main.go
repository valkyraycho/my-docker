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
		container.Run(os.Args[2:])
	case "init":
		if err := container.Init(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "init: %v\n", err)
			os.Exit(1)
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", os.Args[1])
		os.Exit(1)
	}
}
