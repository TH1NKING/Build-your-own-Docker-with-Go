package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/TH1NKING/Build-your-own-Docker-with-Go/internal/profilebundle"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "profile-bundle:", err)
		os.Exit(1)
	}
}

func run(arguments []string) error {
	if len(arguments) == 0 {
		return errors.New("expected a subcommand: build or install")
	}

	switch arguments[0] {
	case "build":
		return runBuild(arguments[1:])
	case "install":
		return runInstall(arguments[1:])
	default:
		return fmt.Errorf("unknown subcommand %q", arguments[0])
	}
}

func runBuild(arguments []string) error {
	flags := flag.NewFlagSet("profile-bundle build", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)

	var options profilebundle.BuildOptions
	flags.StringVar(&options.LockPath, "lock", "", "path to the reviewed profile build lock")
	flags.StringVar(&options.SourceCache, "source-cache", "", "directory containing locked source artifacts")
	flags.StringVar(&options.SystemCallPolicyPath, "system-call-policy", "", "path to the reviewed System Call Policy")
	flags.StringVar(&options.SandboxInitPath, "sandbox-init", "", "path to the externally supplied Sandbox Init")
	flags.StringVar(&options.OutputPath, "output", "", "path for the deterministic Profile Bundle")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments: %v", flags.Args())
	}

	digest, err := profilebundle.Build(options)
	if err != nil {
		return err
	}
	fmt.Fprintln(os.Stdout, digest)
	return nil
}

func runInstall(arguments []string) error {
	flags := flag.NewFlagSet("profile-bundle install", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)

	var options profilebundle.InstallOptions
	flags.StringVar(&options.BundlePath, "bundle", "", "path to the Profile Bundle")
	flags.StringVar(&options.ExpectedSHA256, "expected-sha256", "", "trusted SHA-256 of the complete Profile Bundle")
	flags.StringVar(&options.StorePath, "store", "", "absolute path to the root-owned Profile store")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments: %v", flags.Args())
	}

	installedPath, err := profilebundle.Install(options)
	if err != nil {
		return err
	}
	fmt.Fprintln(os.Stdout, installedPath)
	return nil
}
