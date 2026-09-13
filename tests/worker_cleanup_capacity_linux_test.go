//go:build linux && sandbox_root && profilebundle_root

package workspace_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TH1NKING/Build-your-own-Docker-with-Go/internal/executionqueue"
	"github.com/TH1NKING/Build-your-own-Docker-with-Go/internal/sandboxsupervisor"
	"github.com/TH1NKING/Build-your-own-Docker-with-Go/internal/worker"
	"github.com/TH1NKING/Build-your-own-Docker-with-Go/internal/workerapi"
)

func TestWorkerExecutionCleanupFailureRetainsCapacityAndStopsTheNode(t *testing.T) {
	fixture, identity := installedSandboxExecutionFixture(t)
	t.Cleanup(startSandboxSupervisor(t, fixture,
		"--subuid-start", "200000", "--subgid-start", "300000", "--subid-count", "65536",
		"--cgroup-root", os.Getenv("SANDBOX_CGROUP_ROOT")))
	queue, credentials := workerQueueFixture(t, time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	issued, err := credentials.Provision(ctx, "cleanup-failure-worker", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"cleanup-failure-first", "cleanup-failure-next"} {
		if err := queue.Enqueue(ctx, executionqueue.Execution{
			ExecutionID: id, AgentRunID: "run-" + id, Source: "print('completed-before-cleanup')",
		}); err != nil {
			t.Fatal(err)
		}
	}
	var injection struct {
		sync.Mutex
		once      sync.Once
		sandboxID string
		obstacle  string
		err       error
	}
	handler := workerapi.NewHandler(queue)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/worker/v1/complete" {
			injection.once.Do(func() {
				injection.Lock()
				defer injection.Unlock()
				// A trusted host-side fault leaves an unowned file in the runtime
				// directory after real Python has finished. The Supervisor must
				// refuse to recursively delete it when Worker requests cleanup.
				entries, err := os.ReadDir(fixture.sandboxRoot)
				if err != nil || len(entries) != 1 || !entries[0].IsDir() {
					injection.err = fmt.Errorf("find one live Sandbox for cleanup fault: %v (entries=%d)", err, len(entries))
					return
				}
				injection.sandboxID = entries[0].Name()
				obstacle := filepath.Join(fixture.sandboxRoot, injection.sandboxID, "cleanup-fault-sentinel")
				file, err := os.OpenFile(obstacle, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
				if err != nil {
					injection.err = err
					return
				}
				injection.obstacle = obstacle
				_, writeErr := file.WriteString("preserve unowned host state")
				injection.err = errors.Join(writeErr, file.Close())
			})
			injection.Lock()
			failed := injection.err != nil
			injection.Unlock()
			if failed {
				http.Error(w, "cleanup fault setup failed", http.StatusInternalServerError)
				return
			}
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	api, err := workerapi.NewClient(server.URL, issued.Token, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	repairAndRelease := func() error {
		injection.Lock()
		sandboxID, obstacle := injection.sandboxID, injection.obstacle
		injection.Unlock()
		if obstacle != "" {
			if err := os.Remove(obstacle); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
		if sandboxID == "" {
			return nil
		}
		cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancelCleanup()
		destroyed, err := sandboxsupervisor.NewClient(fixture.socketPath).DestroySandbox(cleanupCtx, sandboxsupervisor.DestroySandboxRequest{
			RequestID: "repair-cleanup-failure", SandboxID: sandboxID,
		})
		if err != nil || destroyed.Error != nil {
			return fmt.Errorf("repair Sandbox cleanup: %+v, %v", destroyed, err)
		}
		record, err := queue.Get(cleanupCtx, "cleanup-failure-first")
		if err != nil {
			return err
		}
		return api.Release(cleanupCtx, workerapi.Lease{ExecutionID: record.Execution.ExecutionID, Generation: record.Generation})
	}
	t.Cleanup(func() {
		if err := repairAndRelease(); err != nil {
			t.Error("remove this test's cleanup obstacle and release confirmed-clean capacity:", err)
		}
	})
	node, err := worker.New(worker.Config{
		API: api, SupervisorSocket: fixture.socketPath, ProfileIdentity: identity,
		Capacity: 1, PollWait: 100 * time.Millisecond, RetryInterval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, runErr := node.RunOnce(ctx)
	injection.Lock()
	setupErr, obstacle := injection.err, injection.obstacle
	injection.Unlock()
	if setupErr != nil || obstacle == "" {
		t.Fatalf("cleanup fault was not installed: %v", setupErr)
	}
	if !claimed || runErr == nil || !strings.Contains(runErr.Error(), "cleanup_failed") {
		t.Fatalf("Worker must surface the real Supervisor cleanup failure: claimed=%v, err=%v", claimed, runErr)
	}
	if contents, err := os.ReadFile(obstacle); err != nil || string(contents) != "preserve unowned host state" {
		t.Fatalf("Supervisor removed or changed the cleanup obstacle: %q %v", contents, err)
	}
	record, err := queue.Get(ctx, "cleanup-failure-first")
	if err != nil || record.State != "completed" || record.Result == nil || record.Result.Stdout != "completed-before-cleanup\n" {
		t.Fatalf("cleanup failure changed the already committed Execution Result: %+v %v", record, err)
	}
	capacity, err := api.Capacity(ctx)
	if err != nil || capacity.Configured != 1 || capacity.Occupied != 1 || capacity.Available != 0 {
		t.Fatalf("failed cleanup released an occupied Sandbox slot: %+v %v", capacity, err)
	}
	if next, err := api.Claim(ctx, 0); err != nil || next != nil {
		t.Fatalf("another claim bypassed the failed cleanup reservation: %+v %v", next, err)
	}
	if claimed, err := node.RunOnce(ctx); claimed || err == nil {
		t.Fatalf("Worker continued accepting work after failed cleanup: claimed=%v err=%v", claimed, err)
	}
	if err := repairAndRelease(); err != nil {
		t.Fatal(err)
	}
	capacity, err = api.Capacity(ctx)
	if err != nil || capacity.Occupied != 0 || capacity.Available != 1 {
		t.Fatalf("confirmed cleanup did not restore capacity: %+v %v", capacity, err)
	}
	if claimed, err := node.RunOnce(ctx); claimed || err == nil {
		t.Fatalf("unsafe Worker resumed merely because capacity became available: claimed=%v err=%v", claimed, err)
	}
	next, err := queue.Get(ctx, "cleanup-failure-next")
	if err != nil || next.State != "queued" {
		t.Fatalf("next Execution ran despite the stopped Worker: %+v %v", next, err)
	}
	assertNoResourceCgroups(t)
}
