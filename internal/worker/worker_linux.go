//go:build linux

// Package worker runs bounded concurrent Executions through the local Sandbox
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
	Capacity         int
}

type Node struct {
	config     Config
	supervisor *sandboxsupervisor.Client
	mu         sync.Mutex
	slots      chan struct{}
	unsafe     error
	active     map[string]struct{}
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
	if config.Capacity == 0 {
		config.Capacity = 2
	}
	if config.Capacity < 1 || config.Capacity > 64 {
		return nil, errors.New("Worker Sandbox capacity must be between 1 and 64")
	}
	if config.PollWait < 0 || config.PollWait > 25*time.Second || config.RetryInterval < time.Millisecond || config.RetryInterval > time.Minute || config.ReportTimeout < time.Millisecond || config.ReportTimeout > 5*time.Minute || config.CleanupTimeout < time.Millisecond || config.CleanupTimeout > time.Minute {
		return nil, errors.New("invalid bounded Worker polling, reporting or cleanup duration")
	}
	return &Node{config: config, supervisor: sandboxsupervisor.NewClient(config.SupervisorSocket), slots: make(chan struct{}, config.Capacity), active: make(map[string]struct{})}, nil
}

// Run stops after any uncertain Execution or cleanup error. Only a failed
// long-poll request may be retried here; RunOnce owns result-report retries.
func (node *Node) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	failures := make(chan error, node.config.Capacity)
	var runners sync.WaitGroup
	for range node.config.Capacity {
		runners.Go(func() {
			err := node.runSlot(ctx)
			failures <- err
			cancel()
		})
	}
	runners.Wait()
	close(failures)
	var result error
	for err := range failures {
		if !errors.Is(err, context.Canceled) {
			result = errors.Join(result, err)
		}
	}
	if result != nil {
		return result
	}
	return ctx.Err()
}

func (node *Node) runSlot(ctx context.Context) error {
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
	select {
	case node.slots <- struct{}{}:
		defer func() { <-node.slots }()
	case <-ctx.Done():
		return false, ctx.Err()
	}
	node.mu.Lock()
	unsafe := node.unsafe
	node.mu.Unlock()
	if unsafe != nil {
		return false, unsafe
	}
	if err := node.reconcileTerminals(ctx); err != nil {
		return false, err
	}
	if err := node.config.API.ConfigureCapacity(ctx, node.config.Capacity); err != nil {
		return false, err
	}
	lease, err := node.config.API.Claim(ctx, node.config.PollWait)
	if err != nil || lease == nil {
		return false, err
	}
	claimed = true
	node.mu.Lock()
	node.active[lease.ExecutionID] = struct{}{}
	node.mu.Unlock()
	var sandboxID string
	// A claimed slot remains occupied until local cleanup AND its durable
	// acknowledgement succeed. A completed result alone never frees capacity.
	defer func() {
		cleanupErr := node.cleanup(*lease, sandboxID)
		if cleanupErr != nil {
			node.mu.Lock()
			node.unsafe = cleanupErr
			node.mu.Unlock()
			returnedErr = errors.Join(returnedErr, cleanupErr)
		}
		node.mu.Lock()
		delete(node.active, lease.ExecutionID)
		node.mu.Unlock()
	}()
	if err := node.config.API.Validate(ctx, *lease); err != nil {
		return true, err
	}
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return true, errors.New("generate private Sandbox identity")
	}
	sandboxID = "sandbox-" + hex.EncodeToString(random)
	monitor, err := node.maintainLease(ctx, *lease, sandboxID)
	if err != nil {
		return true, err
	}
	defer func() { returnedErr = errors.Join(returnedErr, monitor.stop()) }()
	leaseCtx := monitor.ctx
	created, err := node.supervisor.CreateSandbox(leaseCtx, sandboxsupervisor.CreateSandboxRequest{RequestID: "create-" + sandboxID, SandboxID: sandboxID, ProfileIdentity: node.config.ProfileIdentity})
	if err != nil {
		return true, fmt.Errorf("create Worker Sandbox: %w", err)
	}
	if created.Error != nil {
		return true, fmt.Errorf("create Worker Sandbox rejected: %s", created.Error.Code)
	}
	if err := node.waitForAuthority(leaseCtx, *lease); err != nil {
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
	// Healthy reporting has its own budget. Disconnected time is bounded by
	// the independent recovery watchdog instead of destroying recoverable work.
	remaining := node.config.ReportTimeout
	for {
		started := time.Now()
		reportCtx, cancelReport := context.WithTimeout(leaseCtx, remaining)
		err := node.config.API.Complete(reportCtx, *lease, *result)
		cancelReport()
		if reportUnavailable(err) {
			monitor.reportUnavailable()
		}
		if err == nil {
			return true, nil
		} else if !workerapi.Retryable(err) && !(isLeaseConflict(err) && !monitor.completed.Load()) && !(errors.Is(err, context.DeadlineExceeded) && monitor.disconnected.Load()) {
			return true, fmt.Errorf("report immutable Execution Result: %w", err)
		}
		if err := pause(leaseCtx, node.config.RetryInterval); err != nil {
			return true, fmt.Errorf("report immutable Execution Result: %w", err)
		}
		remaining -= monitor.reportingTime(started)
		if remaining <= 0 {
			return true, errors.New("immutable Execution Result reporting budget exhausted")
		}
	}
}

func (node *Node) cleanup(lease workerapi.Lease, sandboxID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), node.config.CleanupTimeout)
	defer cancel()
	if sandboxID != "" {
		response, err := node.supervisor.DestroySandbox(ctx, sandboxsupervisor.DestroySandboxRequest{RequestID: "destroy-" + sandboxID, SandboxID: sandboxID})
		if err != nil {
			return fmt.Errorf("clean Worker Sandbox: %w", err)
		}
		if response.Error != nil && response.Error.Code != sandboxsupervisor.ErrorCodeSandboxNotFound {
			return fmt.Errorf("Supervisor cleanup rejected: %s", response.Error.Code)
		}
	}
	for {
		err := node.config.API.Release(ctx, lease)
		if err == nil {
			return nil
		}
		if !workerapi.Retryable(err) {
			return fmt.Errorf("acknowledge Sandbox capacity release: %w", err)
		}
		if err := pause(ctx, node.config.RetryInterval); err != nil {
			return err
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
