package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/TH1NKING/Build-your-own-Docker-with-Go/internal/executionqueue"
	"github.com/TH1NKING/Build-your-own-Docker-with-Go/internal/workerapi"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "control-plane:", err)
		os.Exit(1)
	}
}

func run(arguments []string) error {
	flags := flag.NewFlagSet("control-plane", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	listen := flags.String("listen", "127.0.0.1:8443", "loopback IP and port for the Worker HTTPS API")
	certFile := flags.String("tls-cert", "", "PEM server certificate")
	keyFile := flags.String("tls-key", "", "PEM server private key")
	leaseDuration := flags.Duration("lease-duration", 2*time.Minute, "duration granted to one Execution")
	if err := flags.Parse(arguments); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(os.Stdout, "Usage: control-plane --tls-cert CERT --tls-key KEY [--listen=127.0.0.1:8443] [--lease-duration=2m]")
			return nil
		}
		return errors.New("invalid Control Plane options (use --help)")
	}
	if flags.NArg() != 0 {
		return errors.New("Control Plane accepts no positional arguments")
	}
	host, port, err := net.SplitHostPort(*listen)
	ip := net.ParseIP(host)
	portNumber, portErr := strconv.Atoi(port)
	if err != nil || ip == nil || !ip.IsLoopback() || portErr != nil || portNumber < 0 || portNumber > 65535 {
		return errors.New("--listen must use a loopback IP and valid port")
	}
	if *certFile == "" || *keyFile == "" {
		return errors.New("--tls-cert and --tls-key are required")
	}
	if os.Getenv("AGENT_DATABASE_URL") == "" {
		return errors.New("AGENT_DATABASE_URL is required")
	}
	if *leaseDuration < 100*time.Millisecond || *leaseDuration > time.Hour {
		return errors.New("--lease-duration must be between 100ms and 1h")
	}
	certificate, err := tls.LoadX509KeyPair(*certFile, *keyFile)
	if err != nil {
		return errors.New("cannot load Control Plane TLS certificate and private key")
	}
	store, err := executionqueue.NewStore(os.Getenv("AGENT_DATABASE_URL"), *leaseDuration)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	startupCtx, cancelStartup := context.WithTimeout(ctx, 10*time.Second)
	err = store.CheckReady(startupCtx)
	cancelStartup()
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		return errors.New("cannot bind loopback Worker API listener")
	}
	defer listener.Close()
	server := &http.Server{
		Handler:           workerapi.NewHandler(store),
		TLSConfig:         &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{certificate}},
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       35 * time.Second,
		WriteTimeout:      35 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	completed := make(chan error, 1)
	go func() { completed <- server.ServeTLS(listener, "", "") }()
	fmt.Fprintf(os.Stdout, "Worker API listening on https://%s\n", listener.Addr())
	select {
	case err := <-completed:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return errors.New("Worker API HTTPS listener failed")
	case <-ctx.Done():
		shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelShutdown()
		if err := server.Shutdown(shutdownCtx); err != nil {
			_ = server.Close()
			return errors.New("Worker API graceful shutdown deadline exceeded")
		}
		if err := <-completed; err != nil && !errors.Is(err, http.ErrServerClosed) {
			return errors.New("Worker API HTTPS listener failed during shutdown")
		}
		return nil
	}
}
