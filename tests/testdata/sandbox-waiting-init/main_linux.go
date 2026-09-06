//go:build linux

// This trusted test-only Init is installed through the normal verified Profile
// Bundle path. It preserves the inherited readiness pipe without writing to it,
// allowing host tests to exercise creation cancellation and the readiness guard.
package main

import (
	"io"
	"os"
)

func main() {
	if os.Getpid() != 1 || os.Geteuid() != 0 {
		os.Exit(2)
	}
	requests := os.NewFile(4, "waiting-init-requests")
	defer requests.Close()
	_, _ = io.Copy(io.Discard, requests)
}
