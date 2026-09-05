//go:build linux

// This trusted conformance fixture is installed as the test Profile entrypoint.
// The host harness enters the Sandbox namespaces with nsenter; sandboxd never
// exposes a test-only execution operation or accepts this probe's arguments.
package main

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

func main() {
	if len(os.Args) != 2 {
		fail("expected the host-only marker path")
	}
	if os.Geteuid() != 0 {
		fail("probe must exercise namespace root's filesystem privileges")
	}
	for _, prefix := range []string{"", "/../../../../", "/.oldroot", "/proc/1/root"} {
		if _, err := os.ReadFile(prefix + os.Args[1]); !errors.Is(err, syscall.ENOENT) {
			fail("host-only marker lookup returned %v, want ENOENT", err)
		}
	}
	if err := os.WriteFile("/unexpected-write", []byte("not allowed"), 0o600); !errors.Is(err, syscall.EROFS) {
		fail("root filesystem write returned %v, want EROFS", err)
	}
	fmt.Println("host-root-unreachable; profile-read-only")
}

func fail(format string, arguments ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", arguments...)
	os.Exit(1)
}
