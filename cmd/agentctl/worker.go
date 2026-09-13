package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"time"

	"github.com/TH1NKING/Build-your-own-Docker-with-Go/internal/workercredential"
)

func runWorker(arguments []string) error {
	if len(arguments) == 0 {
		return errors.New("expected worker subcommand: provision, rotate, revoke, or list")
	}
	if arguments[0] == "--help" || arguments[0] == "-h" {
		printWorkerUsage()
		return nil
	}
	operation := arguments[0]
	if operation != "provision" && operation != "rotate" && operation != "revoke" && operation != "list" {
		return errors.New("unknown worker subcommand")
	}
	flags := flag.NewFlagSet("agentctl worker", flag.ContinueOnError)
	// Parser errors can echo arbitrary arguments. Keep credentials and connection
	// strings out of diagnostics even when an operator supplies the wrong option.
	flags.SetOutput(io.Discard)
	id := flags.String("id", "", "Worker Node identity")
	expires := flags.String("expires-at", "", "new credential expiry (RFC3339)")
	timeout := flags.Duration("timeout", time.Minute, "maximum database operation duration")
	if err := flags.Parse(arguments[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printWorkerUsage()
			return nil
		}
		return errors.New("invalid worker command options (use worker --help)")
	}
	if flags.NArg() != 0 || *timeout <= 0 {
		return errors.New("worker accepts no positional arguments and requires a positive timeout")
	}
	if operation == "list" && (*id != "" || *expires != "") {
		return errors.New("worker list accepts neither --id nor --expires-at")
	}
	if operation != "list" && *id == "" {
		return errors.New("--id is required")
	}
	var expiry time.Time
	if operation == "provision" || operation == "rotate" {
		var err error
		expiry, err = time.Parse(time.RFC3339, *expires)
		if err != nil {
			return errors.New("--expires-at must specify a future RFC3339 time")
		}
	} else if *expires != "" {
		return errors.New("--expires-at is only valid for provision or rotate")
	}
	interruptContext, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(interruptContext, *timeout)
	defer cancel()
	store := workercredential.NewStore(os.Getenv("AGENT_DATABASE_URL"))
	var output any
	switch operation {
	case "provision", "rotate":
		var issued workercredential.Issued
		var err error
		if operation == "provision" {
			issued, err = store.Provision(ctx, *id, expiry)
		} else {
			issued, err = store.Rotate(ctx, *id, expiry)
		}
		if err != nil {
			return err
		}
		output = issued
	case "revoke":
		if err := store.Revoke(ctx, *id); err != nil {
			return err
		}
		output = struct {
			ID      string `json:"id"`
			Revoked bool   `json:"revoked"`
		}{*id, true}
	case "list":
		workers, err := store.List(ctx)
		if err != nil {
			return err
		}
		output = workers
	}
	if err := json.NewEncoder(os.Stdout).Encode(output); err != nil {
		return errors.New("cannot write worker command result; if issuance committed, rotate to obtain a new credential")
	}
	return nil
}

func printWorkerUsage() {
	fmt.Fprintln(os.Stdout, "Usage: agentctl worker provision|rotate --id ID --expires-at RFC3339 [--timeout=1m]")
	fmt.Fprintln(os.Stdout, "       agentctl worker revoke --id ID [--timeout=1m]")
	fmt.Fprintln(os.Stdout, "       agentctl worker list [--timeout=1m]")
	fmt.Fprintln(os.Stdout, "Provision and rotate print a raw credential once; save it in an owner-only Worker file.")
}
