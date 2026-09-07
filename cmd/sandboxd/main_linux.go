//go:build linux

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/TH1NKING/Build-your-own-Docker-with-Go/internal/sandboxsupervisor"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "sandboxd:", err)
		os.Exit(1)
	}
}

func run(arguments []string) error {
	if len(arguments) == 1 && arguments[0] == "--sandbox-bootstrap" {
		return sandboxsupervisor.RunBootstrap()
	}
	flags := flag.NewFlagSet("sandboxd", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)

	var config sandboxsupervisor.ServerConfig
	config.ResourceBudget = sandboxsupervisor.DefaultResourceBudget()
	flags.StringVar(&config.SocketPath, "socket", "", "absolute path for the restricted Worker socket")
	flags.StringVar(&config.ProfileStore, "profile-store", "", "absolute path to the root-owned Profile store")
	flags.StringVar(&config.SandboxRoot, "sandbox-root", "", "absolute path to the runtime-owned Sandbox root")
	flags.StringVar(&config.CgroupRoot, "cgroup-root", "/sys/fs/cgroup", "root-owned cgroup v2 directory with CPU, memory and PID controllers delegated for Sandboxes")
	flags.Int64Var(&config.ResourceBudget.CPUMillis, "cpu-millis", config.ResourceBudget.CPUMillis, "Sandbox CPU quota in thousandths of a core")
	flags.Int64Var(&config.ResourceBudget.MemoryBytes, "memory-bytes", config.ResourceBudget.MemoryBytes, "Sandbox memory budget in bytes")
	flags.Int64Var(&config.ResourceBudget.SwapBytes, "swap-bytes", config.ResourceBudget.SwapBytes, "Sandbox swap budget in bytes; zero prohibits swap")
	flags.Int64Var(&config.ResourceBudget.PIDs, "pids-limit", config.ResourceBudget.PIDs, "Sandbox task budget (processes and threads)")
	flags.UintVar(&config.SubUIDStart, "subuid-start", 0, "first operator-reserved subordinate host UID")
	flags.UintVar(&config.SubGIDStart, "subgid-start", 0, "first operator-reserved subordinate host GID")
	flags.UintVar(&config.SubIDCount, "subid-count", 0, "reserved IDs per UID/GID range, in blocks of 65536; zero disables creation")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments: %v", flags.Args())
	}
	if config.SocketPath == "" || config.ProfileStore == "" || config.SandboxRoot == "" {
		return errors.New("--socket, --profile-store, and --sandbox-root are required")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return sandboxsupervisor.Serve(ctx, config)
}
