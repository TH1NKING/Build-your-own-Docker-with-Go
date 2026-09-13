//go:build linux && sandbox_root && profilebundle_root

package workspace_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TH1NKING/Build-your-own-Docker-with-Go/internal/executionqueue"
	"github.com/TH1NKING/Build-your-own-Docker-with-Go/internal/sandboxsupervisor"
	"github.com/TH1NKING/Build-your-own-Docker-with-Go/internal/worker"
	"github.com/TH1NKING/Build-your-own-Docker-with-Go/internal/workerapi"
)

func TestWorkerExecutionRecoversTheSameSandboxAfterHTTPSDisconnection(t *testing.T) {
	for _, scenario := range []struct {
		name      string
		delay     time.Duration
		blackhole bool
	}{{"during_execution", 2 * time.Second, false}, {"pending_result", 100 * time.Millisecond, false}, {"pending_result_blackhole", 100 * time.Millisecond, true}} {
		t.Run(scenario.name, func(t *testing.T) { verifyWorkerHTTPSRecovery(t, scenario.delay, scenario.blackhole) })
	}
}

func TestWorkerExecutionHealthyHeartbeatsDoNotHideReportFailure(t *testing.T) {
	fixture, identity := installedSandboxExecutionFixture(t)
	t.Cleanup(startSandboxSupervisor(t, fixture, "--subuid-start", "200000", "--subgid-start", "300000", "--subid-count", "65536", "--cgroup-root", os.Getenv("SANDBOX_CGROUP_ROOT")))
	api, _ := workerExecutionControlPlane(t, "print('one result')", func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/worker/v1/complete" {
				http.Error(w, "report unavailable", 503)
				return
			}
			next.ServeHTTP(w, r)
		})
	})
	node, err := worker.New(worker.Config{API: api, SupervisorSocket: fixture.socketPath, ProfileIdentity: identity, ReportTimeout: 500 * time.Millisecond, RetryInterval: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	claimed, err := node.RunOnce(ctx)
	if !claimed || err == nil || ctx.Err() != nil || (!strings.Contains(err.Error(), "reporting budget exhausted") && !errors.Is(err, context.DeadlineExceeded)) {
		t.Fatalf("healthy authority must not suspend reporting forever: claimed=%v err=%v", claimed, err)
	}
	assertNoResourceCgroups(t)
}

func TestWorkerExecutionCommittedResultAcknowledgementHasABoundedWait(t *testing.T) {
	fixture, identity := installedSandboxExecutionFixture(t)
	t.Cleanup(startSandboxSupervisor(t, fixture, "--subuid-start", "200000", "--subgid-start", "300000", "--subid-count", "65536", "--cgroup-root", os.Getenv("SANDBOX_CGROUP_ROOT")))
	var committed atomic.Bool
	api, readResult := workerExecutionControlPlane(t, "print('durable original result')", func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/worker/v1/complete" {
				next.ServeHTTP(w, r)
				return
			}
			if committed.CompareAndSwap(false, true) {
				recorder := httptest.NewRecorder()
				next.ServeHTTP(recorder, r)
				if recorder.Code != http.StatusNoContent {
					t.Errorf("result was not committed before ACK loss: %d", recorder.Code)
				}
				connection, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				_ = connection.Close()
				return
			}
			http.Error(w, "acknowledgement unavailable", 503)
		})
	})
	node, err := worker.New(worker.Config{API: api, SupervisorSocket: fixture.socketPath, ProfileIdentity: identity, ReportTimeout: 500 * time.Millisecond, RetryInterval: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	claimed, err := node.RunOnce(ctx)
	if !claimed || err == nil || ctx.Err() != nil || (!strings.Contains(err.Error(), "reporting budget exhausted") && !errors.Is(err, context.DeadlineExceeded)) {
		t.Fatalf("committed result ACK wait was not bounded by its report budget: claimed=%v err=%v", claimed, err)
	}
	result := readResult()
	if result.ExitCode != 0 || result.Stdout != "durable original result\n" {
		t.Fatalf("ACK failure changed durable result: %+v", result)
	}
	capacity, err := api.Capacity(context.Background())
	if err != nil || capacity.Occupied != 0 {
		t.Fatalf("committed result kept cleaned capacity: %+v %v", capacity, err)
	}
	assertNoResourceCgroups(t)
}

func verifyWorkerHTTPSRecovery(t *testing.T, workloadDelay time.Duration, blackhole bool) {
	t.Helper()
	fixture, identity := installedSandboxExecutionFixture(t)
	t.Cleanup(startSandboxSupervisor(t, fixture,
		"--subuid-start", "200000", "--subgid-start", "300000", "--subid-count", "65536",
		"--cgroup-root", os.Getenv("SANDBOX_CGROUP_ROOT")))
	queue, credentials := workerQueueFixture(t, time.Second, 3*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	issued, err := credentials.Provision(ctx, "recover-worker", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := queue.Enqueue(ctx, executionqueue.Execution{ExecutionID: "recover-execution", AgentRunID: "recover-run", Source: fmt.Sprintf("import time\nopen('once', 'x').write('original sandbox')\ntime.sleep(%f)\nprint(open('once').read())", workloadDelay.Seconds())}); err != nil {
		t.Fatal(err)
	}
	var disconnected atomic.Bool
	restored := make(chan struct{})
	var restoreOnce sync.Once
	restore := func() { disconnected.Store(false); restoreOnce.Do(func() { close(restored) }) }
	handler := workerapi.NewHandler(queue)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if disconnected.Load() {
			if !blackhole {
				http.Error(w, "connection unavailable", http.StatusServiceUnavailable)
				return
			}
			select {
			case <-r.Context().Done():
				return
			case <-restored:
			}
		}
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	defer restore()
	api, err := workerapi.NewClient(server.URL, issued.Token, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	node, err := worker.New(worker.Config{API: api, SupervisorSocket: fixture.socketPath, ProfileIdentity: identity, RetryInterval: 20 * time.Millisecond, ReportTimeout: 500 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	sweepCtx, stopSweep := context.WithCancel(ctx)
	swept := make(chan struct{})
	go func() {
		defer close(swept)
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-sweepCtx.Done():
				return
			case <-ticker.C:
				if err := queue.SweepRecovery(sweepCtx); err != nil && sweepCtx.Err() == nil {
					t.Errorf("recovery sweep: %v", err)
					return
				}
			}
		}
	}()
	defer func() { stopSweep(); <-swept }()
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { _, err := node.RunOnce(runCtx); done <- err }()
	defer stop()
	local := sandboxsupervisor.NewClient(fixture.socketPath)
	var original string
	for {
		record, err := queue.Get(ctx, "recover-execution")
		if err != nil {
			t.Fatal(err)
		}
		if record.SandboxID != "" {
			response, err := local.GetExecutionResult(ctx, sandboxsupervisor.GetExecutionResultRequest{RequestID: "observe-running", SandboxID: record.SandboxID, ExecutionID: "recover-execution"})
			if err == nil && response.Error != nil && response.Error.Code == sandboxsupervisor.ErrorCodeResultNotReady {
				original = record.SandboxID
				break
			}
		}
		select {
		case <-ctx.Done():
			t.Fatal("Execution never became observable in its original Sandbox")
		case err := <-done:
			t.Fatalf("Worker stopped before disconnect: %v", err)
		case <-time.After(10 * time.Millisecond):
		}
	}
	disconnected.Store(true)
	for {
		record, err := queue.Get(ctx, "recover-execution")
		if err != nil {
			t.Fatal(err)
		}
		if record.State == "recovering" {
			break
		}
		if record.State == "worker_lost" {
			t.Fatal("temporary HTTPS disconnect destroyed recoverable work")
		}
		select {
		case <-ctx.Done():
			t.Fatal("missing heartbeat never entered durable recovery")
		case <-time.After(10 * time.Millisecond):
		}
	}
	inspection, err := local.InspectSandbox(ctx, sandboxsupervisor.InspectSandboxRequest{RequestID: "verify-original", SandboxID: original})
	if err != nil || inspection.Error != nil || inspection.Result == nil || inspection.Result.SandboxID != original {
		t.Fatalf("HTTPS outage lost the original Sandbox: %+v %v", inspection, err)
	}
	restore()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("same Worker failed to recover: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("Worker did not finish recovered Execution")
	}
	record, err := queue.Get(ctx, "recover-execution")
	if err != nil || record.State != "completed" || record.SandboxID != original || record.Result == nil || record.Result.ExitCode != 0 || record.Result.Stdout != "original sandbox\n" {
		t.Fatalf("recovery changed the Execution or Sandbox: %+v %v", record, err)
	}
	capacity, err := api.Capacity(ctx)
	if err != nil || capacity.Occupied != 0 {
		t.Fatalf("recovered Sandbox did not release capacity after cleanup: %+v %v", capacity, err)
	}
	assertNoResourceCgroups(t)
}

func TestWorkerExecutionRecoveryExpiryCleansUpWithoutReplayingWork(t *testing.T) {
	fixture, identity := installedSandboxExecutionFixture(t)
	t.Cleanup(startSandboxSupervisor(t, fixture, "--subuid-start", "200000", "--subgid-start", "300000", "--subid-count", "65536", "--cgroup-root", os.Getenv("SANDBOX_CGROUP_ROOT")))
	queue, credentials := workerQueueFixture(t, 500*time.Millisecond, 500*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	issued, err := credentials.Provision(ctx, "expiry-worker", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := queue.Enqueue(ctx, executionqueue.Execution{ExecutionID: "expiry-execution", AgentRunID: "expiry-run", Source: "import time\nopen('once','x').write('never replay')\ntime.sleep(5)\nprint('must not finish')"}); err != nil {
		t.Fatal(err)
	}
	var beats atomic.Int32
	var restored atomic.Bool
	handler := workerapi.NewHandler(queue)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !restored.Load() && ((r.URL.Path == "/worker/v1/heartbeat" && beats.Add(1) > 1) || r.URL.Path == "/worker/v1/recover") {
			http.Error(w, "heartbeat unavailable", 503)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	api, err := workerapi.NewClient(server.URL, issued.Token, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	config := worker.Config{API: api, SupervisorSocket: fixture.socketPath, ProfileIdentity: identity, RetryInterval: 20 * time.Millisecond, PollWait: 20 * time.Millisecond}
	node, err := worker.New(config)
	if err != nil {
		t.Fatal(err)
	}
	if claimed, err := node.RunOnce(ctx); !claimed || !errors.Is(err, worker.ErrWorkerLost) {
		t.Fatalf("recovery expiry must stop the original Workload: claimed=%v err=%v", claimed, err)
	}
	record, err := queue.Get(ctx, "expiry-execution")
	if err != nil || record.State != "worker_lost" || record.Result != nil {
		t.Fatalf("expired recovery accepted a result: %+v %v", record, err)
	}
	assertNoResourceCgroups(t)
	restored.Store(true)
	if next, err := api.Claim(ctx, 0); err != nil || next != nil {
		t.Fatalf("lost Execution was automatically replayed: %+v %v", next, err)
	}
	if err := queue.Enqueue(ctx, executionqueue.Execution{ExecutionID: "explicit-retry", AgentRunID: "explicit-new-run", Source: "print('explicit new run')"}); err != nil {
		t.Fatal(err)
	}
	replacement, err := worker.New(config)
	if err != nil {
		t.Fatal(err)
	}
	if claimed, err := replacement.RunOnce(ctx); !claimed || err != nil {
		t.Fatalf("explicit replacement Agent Run failed: %v %v", claimed, err)
	}
	result, err := queue.Get(ctx, "explicit-retry")
	if err != nil || result.Result == nil || result.Result.Stdout != "explicit new run\n" {
		t.Fatalf("replacement run did not complete: %+v %v", result, err)
	}
	assertNoResourceCgroups(t)
}
