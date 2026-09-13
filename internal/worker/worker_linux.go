//go:build linux

// Package worker runs one leased Execution at a time through the local Sandbox
// Supervisor. It never talks to PostgreSQL or retries a Workload execution.
package worker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/TH1NKING/Build-your-own-Docker-with-Go/internal/sandboxsupervisor"
	"github.com/TH1NKING/Build-your-own-Docker-with-Go/internal/workerapi"
)

var ErrExecutionUncertain = errors.New("Execution outcome is uncertain; Workload will not be executed again")

type Config struct {
	API              *workerapi.Client
	SupervisorSocket string
	ProfileIdentity  string
	PollWait         time.Duration
	RetryInterval    time.Duration
	ReportTimeout    time.Duration
	CleanupTimeout   time.Duration
}

type Node struct {
	config     Config
	supervisor *sandboxsupervisor.Client
	mu         sync.Mutex
}

func New(config Config) (*Node, error) {
	digest := strings.TrimPrefix(config.ProfileIdentity, "sha256:")
	decoded, err := hex.DecodeString(digest)
	if config.API == nil || !filepath.IsAbs(config.SupervisorSocket) || !strings.HasPrefix(config.ProfileIdentity, "sha256:") || err != nil || len(decoded) != 32 || strings.ToLower(digest) != digest {
		return nil, errors.New("Worker requires a Control Plane client, absolute Supervisor socket and canonical Profile identity")
	}
	if config.PollWait == 0 {
		config.PollWait = 20 * time.Second
	}
	if config.RetryInterval == 0 {
		config.RetryInterval = 250 * time.Millisecond
	}
	if config.ReportTimeout == 0 {
		config.ReportTimeout = 30 * time.Second
	}
	if config.CleanupTimeout == 0 {
		config.CleanupTimeout = 10 * time.Second
	}
	if config.PollWait < 0 || config.PollWait > 25*time.Second || config.RetryInterval < time.Millisecond || config.RetryInterval > time.Minute || config.ReportTimeout < time.Millisecond || config.ReportTimeout > 5*time.Minute || config.CleanupTimeout < time.Millisecond || config.CleanupTimeout > time.Minute {
		return nil, errors.New("invalid bounded Worker polling, reporting or cleanup duration")
	}
	return &Node{config: config, supervisor: sandboxsupervisor.NewClient(config.SupervisorSocket)}, nil
}

// Run stops after any uncertain Execution or cleanup error. Only a failed
// long-poll request may be retried here; RunOnce owns result-report retries.
func (node *Node) Run(ctx context.Context) error {
	for {
		claimed, err := node.RunOnce(ctx)
		if err != nil {
			if !claimed && workerapi.Retryable(err) && ctx.Err() == nil {
				if err := pause(ctx, node.config.RetryInterval); err != nil {
					return err
				}
				continue
			}
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
}

// RunOnce claims at most one Execution. The initial tracer uses a fresh Sandbox
// per Agent Run; multi-Execution Sandbox residency belongs to the Agent Loop.
func (node *Node) RunOnce(ctx context.Context) (claimed bool, returnedErr error) {
	node.mu.Lock()
	defer node.mu.Unlock()
	lease, err := node.config.API.Claim(ctx, node.config.PollWait)
	if err != nil || lease == nil {
		return false, err
	}
	claimed = true
	if err := node.config.API.Validate(ctx, *lease); err != nil {
		return true, err
	}
	if !time.Now().Before(lease.ExpiresAt) {
		return true, errors.New("Execution Lease expired before Sandbox creation")
	}
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return true, errors.New("generate private Sandbox identity")
	}
	sandboxID := "sandbox-" + hex.EncodeToString(random)
	// Creation can succeed even if its response is lost. Cleanup therefore starts
	// before sending CreateSandbox, and never depends on the cancelled lease.
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), node.config.CleanupTimeout)
		defer cancel()
		response, cleanupErr := node.supervisor.DestroySandbox(cleanupCtx, sandboxsupervisor.DestroySandboxRequest{RequestID: "destroy-" + sandboxID, SandboxID: sandboxID})
		if cleanupErr == nil && response.Error != nil && response.Error.Code != sandboxsupervisor.ErrorCodeSandboxNotFound {
			cleanupErr = fmt.Errorf("Supervisor cleanup rejected: %s", response.Error.Code)
		}
		if cleanupErr != nil {
			returnedErr = errors.Join(returnedErr, fmt.Errorf("clean Worker Sandbox: %w", cleanupErr))
		}
	}()
	leaseCtx, cancelLease := context.WithDeadline(ctx, lease.ExpiresAt)
	defer cancelLease()
	created, err := node.supervisor.CreateSandbox(leaseCtx, sandboxsupervisor.CreateSandboxRequest{RequestID: "create-" + sandboxID, SandboxID: sandboxID, ProfileIdentity: node.config.ProfileIdentity})
	if err != nil {
		return true, fmt.Errorf("create Worker Sandbox: %w", err)
	}
	if created.Error != nil {
		return true, fmt.Errorf("create Worker Sandbox rejected: %s", created.Error.Code)
	}
	if err := node.config.API.Validate(leaseCtx, *lease); err != nil {
		return true, err
	}
	response, executionErr := node.supervisor.ExecutePython(leaseCtx, sandboxsupervisor.ExecutePythonRequest{
		RequestID: "execute-" + sandboxID, SandboxID: sandboxID, ExecutionID: lease.ExecutionID,
		Source: lease.Source, Stdin: lease.Stdin, OutputPaths: lease.OutputPaths,
	})
	result := response.Result
	if executionErr != nil || (response.Error != nil && response.Error.Code == sandboxsupervisor.ErrorCodeExecutionExists) {
		return true, errors.Join(ErrExecutionUncertain, executionErr)
	} else if response.Error != nil {
		return true, fmt.Errorf("Supervisor Execution rejected: %s", response.Error.Code)
	}
	if result == nil || result.ExecutionID != lease.ExecutionID {
		return true, ErrExecutionUncertain
	}
	// Client decoding owns these bytes. Keep the exact immutable snapshot until
	// acknowledgement; neither retries nor cleanup synthesize another result.
	reportCtx, cancelReport := context.WithTimeout(ctx, node.config.ReportTimeout)
	defer cancelReport()
	for {
		if err := node.config.API.Complete(reportCtx, *lease, *result); err == nil {
			return true, nil
		} else if !workerapi.Retryable(err) {
			return true, fmt.Errorf("report immutable Execution Result: %w", err)
		}
		if err := pause(reportCtx, node.config.RetryInterval); err != nil {
			return true, fmt.Errorf("report immutable Execution Result: %w", err)
		}
	}
}

func pause(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
