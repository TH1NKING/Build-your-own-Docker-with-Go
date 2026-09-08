package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/signal"
	"time"

	"github.com/TH1NKING/Build-your-own-Docker-with-Go/internal/migrations"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "agentctl:", err)
		os.Exit(1)
	}
}

func run(arguments []string) error {
	if len(arguments) == 1 && (arguments[0] == "--help" || arguments[0] == "-h") {
		fmt.Fprintln(os.Stdout, "Usage: agentctl migrate [--status] [--timeout=1m] [--migrations-dir=DIR]")
		return nil
	}
	if len(arguments) == 0 || arguments[0] != "migrate" {
		return errors.New("expected subcommand: migrate (use --help for usage)")
	}
	flags := flag.NewFlagSet("agentctl migrate", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	status := flags.Bool("status", false, "report the schema version without changing the database")
	timeout := flags.Duration("timeout", time.Minute, "maximum time for connection, migration lock, and SQL")
	directory := flags.String("migrations-dir", "", "trusted reviewed SQL directory; defaults to bundled migrations")
	if err := flags.Parse(arguments[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("migrate accepts no positional arguments")
	}
	if *timeout <= 0 {
		return errors.New("--timeout must be positive")
	}
	if os.Getenv("AGENT_DATABASE_URL") == "" {
		return errors.New("AGENT_DATABASE_URL is required")
	}
	var source fs.FS = migrations.Bundled()
	if *directory != "" {
		source = os.DirFS(*directory)
	}
	interruptContext, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(interruptContext, *timeout)
	defer cancel()
	result, err := migrations.Run(ctx, os.Getenv("AGENT_DATABASE_URL"), source, *status)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "schema_version=%d applied=%d\n", result.Version, result.Applied)
	return nil
}
