//go:build linux

package worker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/TH1NKING/Build-your-own-Docker-with-Go/internal/sandboxsupervisor"
	"github.com/TH1NKING/Build-your-own-Docker-with-Go/internal/workerapi"
)

var ErrWorkerLost = errors.New("Worker recovery authority expired or the original Sandbox was lost")

// leaseMonitor preserves the original local Execution RPC during an HTTPS
// outage. The watchdog bounds preservation even with no Control Plane response.
// No path here sends ExecutePython again.
type leaseMonitor struct {
	ctx           context.Context
	cancel        context.CancelCauseFunc
	stopHeartbeat context.CancelFunc
	done          chan error
	watchdog      *time.Timer
	disconnected  atomic.Bool
	completed     atomic.Bool
	wake          chan struct{}
	clockOrigin   time.Time
	lastContact   atomic.Int64
}

func (node *Node) maintainLease(ctx context.Context, lease workerapi.Lease, sandboxID string) (*leaseMonitor, error) {
	initial, cancelInitial := context.WithTimeout(ctx, node.config.ReportTimeout)
	defer cancelInitial()
	for {
		err := node.config.API.BindSandbox(initial, lease, sandboxID)
		if err == nil {
			break
		}
		if !workerapi.Retryable(err) {
			return nil, err
		}
		if err := pause(initial, node.config.RetryInterval); err != nil {
			return nil, err
		}
	}
	var authority workerapi.Authority
	var started time.Time
	for {
		started = time.Now()
		var err error
		authority, err = node.config.API.Heartbeat(initial, lease, sandboxID)
		if err == nil {
			break
		}
		if !workerapi.Retryable(err) {
			return nil, err
		}
		if err := pause(initial, node.config.RetryInterval); err != nil {
			return nil, err
		}
	}
	if !validAuthority(authority, lease) || authority.State != "leased" {
		return nil, ErrWorkerLost
	}
	workCtx, cancelWork := context.WithCancelCause(ctx)
	hbCtx, cancelHB := context.WithCancel(workCtx)
	monitor := &leaseMonitor{ctx: workCtx, cancel: cancelWork, stopHeartbeat: cancelHB, done: make(chan error, 1), wake: make(chan struct{}, 1), clockOrigin: started}
	// Database-relative durations and the local request START prevent clock
	// skew or a slow response from extending the granted recovery window.
	monitor.watchdog = time.AfterFunc(time.Until(started.Add(authority.RecoveryUntil.Sub(authority.ServerTime))), func() { cancelWork(ErrWorkerLost) })
	go func() {
		err := node.heartbeatLoop(hbCtx, monitor, lease, sandboxID, authority)
		if err != nil {
			cancelWork(err)
		}
		monitor.done <- err
	}()
	return monitor, nil
}

func validAuthority(authority workerapi.Authority, lease workerapi.Lease) bool {
	return authority.Lease.ExecutionID == lease.ExecutionID && authority.Lease.AgentRunID == lease.AgentRunID && authority.Lease.Generation == lease.Generation && !authority.ServerTime.IsZero() && (authority.State == "completed" || (authority.State == "leased" && authority.Lease.ExpiresAt.After(authority.ServerTime) && authority.RecoveryUntil.After(authority.Lease.ExpiresAt)))
}

func (node *Node) heartbeatLoop(ctx context.Context, monitor *leaseMonitor, lease workerapi.Lease, sandboxID string, authority workerapi.Authority) error {
	delay := heartbeatDelay(authority)
	for {
		if err := monitor.waitHeartbeat(ctx, delay); err != nil {
			if errors.Is(context.Cause(monitor.ctx), ErrWorkerLost) {
				return ErrWorkerLost
			}
			return nil
		}
		started := time.Now()
		next, err := node.config.API.Heartbeat(ctx, lease, sandboxID)
		if isLeaseConflict(err) {
			monitor.disconnected.Store(true)
			inspection, inspectErr := node.supervisor.InspectSandbox(ctx, sandboxsupervisor.InspectSandboxRequest{RequestID: "inspect-" + sandboxID, SandboxID: sandboxID})
			if ctx.Err() != nil && !errors.Is(context.Cause(monitor.ctx), ErrWorkerLost) {
				return nil
			}
			if inspectErr != nil || inspection.Error != nil || inspection.Result == nil || inspection.Result.SandboxID != sandboxID {
				return ErrWorkerLost
			}
			started = time.Now()
			next, err = node.config.API.Recover(ctx, lease, sandboxID)
		}
		if ctx.Err() != nil {
			if errors.Is(context.Cause(monitor.ctx), ErrWorkerLost) {
				return ErrWorkerLost
			}
			return nil
		}
		if err != nil {
			monitor.disconnected.Store(true)
			if !workerapi.Retryable(err) {
				return fmt.Errorf("restore Worker authority: %w", err)
			}
			delay = node.config.RetryInterval
			continue
		}
		if !validAuthority(next, lease) {
			return ErrWorkerLost
		}
		// The request start is a conservative contact time even if the valid
		// response was delayed. Keep its monotonic offset, not a wall clock.
		monitor.lastContact.Store(started.Sub(monitor.clockOrigin).Nanoseconds())
		if next.State == "completed" {
			monitor.completed.Store(true)
			monitor.watchdog.Stop()
			monitor.disconnected.Store(false)
			return nil
		}
		monitor.watchdog.Reset(time.Until(started.Add(next.RecoveryUntil.Sub(next.ServerTime))))
		monitor.disconnected.Store(false)
		delay = heartbeatDelay(next)
	}
}

func (monitor *leaseMonitor) waitHeartbeat(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	case <-monitor.wake:
		return nil
	}
}

func (monitor *leaseMonitor) reportUnavailable() {
	if monitor.completed.Load() {
		return
	}
	monitor.disconnected.Store(true)
	select {
	case monitor.wake <- struct{}{}:
	default:
	}
}

func (monitor *leaseMonitor) reportingTime(started time.Time) time.Duration {
	// Once the Control Plane confirmed completion, heartbeats and recovery
	// stop. Waiting for the exact result's ACK must still consume a hard budget.
	if monitor.completed.Load() || !monitor.disconnected.Load() {
		return time.Since(started)
	}
	// A report can discover a blackholed connection before the next heartbeat
	// times out. Charge only the portion confirmed healthy; the watchdog still
	// bounds all uncertain time. Healthy heartbeats prevent report-only failures
	// from suspending the reporting budget indefinitely.
	confirmed := monitor.clockOrigin.Add(time.Duration(monitor.lastContact.Load()))
	return max(0, confirmed.Sub(started))
}

func reportUnavailable(err error) bool {
	var status *workerapi.HTTPError
	return errors.Is(err, workerapi.ErrTransport) || errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &status) && status.StatusCode >= 500)
}

func (node *Node) waitForAuthority(ctx context.Context, lease workerapi.Lease) error {
	for {
		err := node.config.API.Validate(ctx, lease)
		if err == nil {
			return nil
		}
		if !workerapi.Retryable(err) && !isLeaseConflict(err) {
			return err
		}
		if err := pause(ctx, node.config.RetryInterval); err != nil {
			return err
		}
	}
}

func heartbeatDelay(authority workerapi.Authority) time.Duration {
	return max(time.Millisecond, min(10*time.Second, authority.Lease.ExpiresAt.Sub(authority.ServerTime)/3))
}

func isLeaseConflict(err error) bool {
	var status *workerapi.HTTPError
	return errors.As(err, &status) && status.StatusCode == http.StatusConflict
}

func (monitor *leaseMonitor) stop() error {
	monitor.stopHeartbeat()
	err := <-monitor.done
	monitor.watchdog.Stop()
	monitor.cancel(nil)
	return err
}

// A restarted process may clean terminal reservations. It must not touch live
// or recovering work, and the inventory never grants permission to replay it.
func (node *Node) reconcileTerminals(ctx context.Context) error {
	entries, err := node.config.API.Outstanding(ctx)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.CleanupUnknown {
			err := errors.New("legacy Sandbox cleanup is unknown; revoke this Worker, clean its old Supervisor resources, then run agentctl worker confirm-legacy-cleanup")
			node.mu.Lock()
			node.unsafe = err
			node.mu.Unlock()
			return err
		}
		if entry.State != "completed" && entry.State != "worker_lost" {
			continue
		}
		node.mu.Lock()
		_, owned := node.active[entry.Lease.ExecutionID]
		node.mu.Unlock()
		if owned {
			continue
		}
		if err := node.cleanup(entry.Lease, entry.SandboxID); err != nil {
			node.mu.Lock()
			node.unsafe = err
			node.mu.Unlock()
			return err
		}
	}
	return nil
}
