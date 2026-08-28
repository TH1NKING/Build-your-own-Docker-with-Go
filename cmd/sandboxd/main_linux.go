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
	flags := flag.NewFlagSet("sandboxd", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)

	var config sandboxsupervisor.ServerConfig
	flags.StringVar(&config.SocketPath, "socket", "", "absolute path for the restricted Worker socket")
	flags.StringVar(&config.ProfileStore, "profile-store", "", "absolute path to the root-owned Profile store")
	flags.StringVar(&config.SandboxRoot, "sandbox-root", "", "absolute path to the runtime-owned Sandbox root")
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
