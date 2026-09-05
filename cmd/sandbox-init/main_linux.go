//go:build linux

package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/TH1NKING/Build-your-own-Docker-with-Go/internal/sandboxsupervisor"
)

func main() {
	var err error
	switch {
	case len(os.Args) == 1:
		err = sandboxsupervisor.RunInit()
	case len(os.Args) == 3 && os.Args[1] == "--workload":
		err = sandboxsupervisor.RunInitWorkload(os.Args[2])
	default:
		err = errors.New("unexpected Sandbox Init arguments")
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "sandbox-init:", err)
		os.Exit(1)
	}
}
