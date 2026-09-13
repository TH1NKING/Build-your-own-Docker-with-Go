//go:build linux

package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/TH1NKING/Build-your-own-Docker-with-Go/internal/worker"
	"github.com/TH1NKING/Build-your-own-Docker-with-Go/internal/workerapi"
	"github.com/TH1NKING/Build-your-own-Docker-with-Go/internal/workercredential"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "worker:", err)
		os.Exit(1)
	}
}

func run(arguments []string) error {
	flags := flag.NewFlagSet("worker", flag.ContinueOnError)
	var config worker.Config
	var baseURL, credentialFile, caFile string
	var once bool
	flags.StringVar(&baseURL, "control-plane", "", "HTTPS origin exposing the authenticated Worker API")
	flags.StringVar(&credentialFile, "credential-file", "", "absolute path to the restricted Worker Credential file")
	flags.StringVar(&caFile, "ca-file", "", "optional PEM private certificate authority added to system trust")
	flags.StringVar(&config.SupervisorSocket, "supervisor-socket", "", "absolute path to the restricted local Supervisor socket")
	flags.StringVar(&config.ProfileIdentity, "profile-identity", "", "operator-approved installed Profile Bundle sha256 identity")
	flags.IntVar(&config.Capacity, "capacity", 2, "maximum concurrent Sandboxes on this Worker (1-64)")
	flags.DurationVar(&config.PollWait, "poll-wait", 20*time.Second, "HTTPS long-poll duration (at most 25s)")
	flags.DurationVar(&config.RetryInterval, "retry-interval", 250*time.Millisecond, "delay between result-report or long-poll retries")
	flags.DurationVar(&config.ReportTimeout, "report-timeout", 30*time.Second, "maximum time to acknowledge one immutable Execution Result")
	flags.DurationVar(&config.CleanupTimeout, "cleanup-timeout", 10*time.Second, "independent deadline for Supervisor cleanup")
	flags.BoolVar(&once, "once", false, "claim and process at most one Execution, then exit")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if os.Getuid() == 0 || os.Geteuid() == 0 {
		return errors.New("Worker must run as a dedicated unprivileged account")
	}
	token, err := workercredential.LoadFile(credentialFile)
	if err != nil {
		return err
	}
	var httpClient *http.Client
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return errors.New("read Worker API certificate authority")
		}
		roots, err := x509.SystemCertPool()
		if err != nil {
			roots = x509.NewCertPool()
		}
		if !roots.AppendCertsFromPEM(pem) {
			return errors.New("Worker API certificate authority contains no certificates")
		}
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}
		defer transport.CloseIdleConnections()
		httpClient = &http.Client{Transport: transport}
	}
	config.API, err = workerapi.NewClient(baseURL, token, httpClient)
	if err != nil {
		return err
	}
	node, err := worker.New(config)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if once {
		_, err = node.RunOnce(ctx)
	} else {
		err = node.Run(ctx)
	}
	if err == context.Canceled && ctx.Err() != nil {
		return nil
	}
	return err
}
