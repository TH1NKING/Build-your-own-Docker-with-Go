package workspace_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TH1NKING/Build-your-own-Docker-with-Go/internal/executionqueue"
	"github.com/TH1NKING/Build-your-own-Docker-with-Go/internal/sandboxsupervisor"
	"github.com/TH1NKING/Build-your-own-Docker-with-Go/internal/workerapi"
)

func TestWorkerAPIRecoveryBindsOneSandboxToExecution(t *testing.T) {
	queue, credentials := workerQueueFixture(t, time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	issued, err := credentials.Provision(ctx, "bound-worker", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(workerapi.NewHandler(queue))
	defer server.Close()
	client, err := workerapi.NewClient(server.URL, issued.Token, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if err := queue.Enqueue(ctx, executionqueue.Execution{ExecutionID: "bound-execution", AgentRunID: "bound-run", Source: "pass"}); err != nil {
		t.Fatal(err)
	}
	lease, err := client.Claim(ctx, 0)
	if err != nil || lease == nil {
		t.Fatalf("claim: %+v %v", lease, err)
	}
	for range 2 {
		body := fmt.Sprintf(`{"execution_id":%q,"generation":%d,"sandbox_id":"sandbox-one"}`, lease.ExecutionID, lease.Generation)
		if code := capacityAPIPost(t, ctx, server, issued.Token, "bind-sandbox", body, nil); code != http.StatusNoContent {
			t.Fatalf("bind Sandbox idempotently: HTTP %d, want 204", code)
		}
	}
	body := fmt.Sprintf(`{"execution_id":%q,"generation":%d,"sandbox_id":"sandbox-other"}`, lease.ExecutionID, lease.Generation)
	if code := capacityAPIPost(t, ctx, server, issued.Token, "bind-sandbox", body, nil); code != http.StatusConflict {
		t.Fatalf("replaced Sandbox binding: HTTP %d, want 409", code)
	}
}

func TestWorkerAPIRecoveryOutstandingIncludesUnreleasedIdentitiesOnly(t *testing.T) {
	queue, credentials := workerQueueFixture(t, time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	issued, err := credentials.Provision(ctx, "outstanding-worker", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	other, err := credentials.Provision(ctx, "unrelated-worker", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(workerapi.NewHandler(queue))
	defer server.Close()
	client, err := workerapi.NewClient(server.URL, issued.Token, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	var leases []workerapi.Lease
	for i := range 2 {
		if err := queue.Enqueue(ctx, executionqueue.Execution{ExecutionID: fmt.Sprintf("outstanding-execution-%d", i), AgentRunID: fmt.Sprintf("outstanding-run-%d", i), Source: "#" + strings.Repeat("source", 5000), Stdin: "private input"}); err != nil {
			t.Fatal(err)
		}
		lease, err := client.Claim(ctx, 0)
		if err != nil || lease == nil {
			t.Fatalf("claim: %+v %v", lease, err)
		}
		leases = append(leases, *lease)
	}
	if err := client.BindSandbox(ctx, leases[0], "sandbox-outstanding"); err != nil {
		t.Fatal(err)
	}
	if err := client.Complete(ctx, leases[0], sandboxsupervisor.ExecutePythonResult{ExecutionID: leases[0].ExecutionID, TerminalReason: sandboxsupervisor.ExecutionExited}); err != nil {
		t.Fatal(err)
	}
	var entries []struct {
		Lease     workerapi.Lease `json:"lease"`
		SandboxID string          `json:"sandbox_id"`
		State     string          `json:"state"`
	}
	if code := capacityAPIPost(t, ctx, server, issued.Token, "outstanding", `{}`, &entries); code != http.StatusOK {
		t.Fatalf("outstanding: HTTP %d, want 200", code)
	}
	if len(entries) != 2 || entries[0].State != "completed" || entries[0].SandboxID != "sandbox-outstanding" || entries[1].State != "leased" || entries[1].SandboxID != "" {
		t.Fatalf("outstanding reservations: %+v", entries)
	}
	for _, entry := range entries {
		if entry.Lease.Source != "" || entry.Lease.Stdin != "" || len(entry.Lease.OutputPaths) != 0 || entry.Lease.ExecutionID == "" || entry.Lease.Generation != 1 {
			t.Fatalf("outstanding exposed Workload or omitted identity: %+v", entry)
		}
	}
	if code := capacityAPIPost(t, ctx, server, other.Token, "outstanding", `{}`, &entries); code != http.StatusOK || len(entries) != 0 {
		t.Fatalf("foreign outstanding visibility: HTTP %d %+v", code, entries)
	}
}

func TestWorkerAPIRecoveryHeartbeatExtendsOnlyBoundLease(t *testing.T) {
	queue, credentials := workerQueueFixture(t, time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	issued, err := credentials.Provision(ctx, "heartbeat-worker", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(workerapi.NewHandler(queue))
	defer server.Close()
	client, err := workerapi.NewClient(server.URL, issued.Token, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if err := queue.Enqueue(ctx, executionqueue.Execution{ExecutionID: "heartbeat-execution", AgentRunID: "heartbeat-run", Source: "pass"}); err != nil {
		t.Fatal(err)
	}
	lease, err := client.Claim(ctx, 0)
	if err != nil || lease == nil {
		t.Fatalf("claim: %+v %v", lease, err)
	}
	if err := client.BindSandbox(ctx, *lease, "sandbox-heartbeat"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	case <-time.After(300 * time.Millisecond):
	}
	var authority struct {
		Lease         workerapi.Lease `json:"lease"`
		State         string          `json:"state"`
		ServerTime    time.Time       `json:"server_time"`
		RecoveryUntil time.Time       `json:"recovery_until"`
	}
	body := fmt.Sprintf(`{"execution_id":%q,"generation":%d,"sandbox_id":"sandbox-heartbeat"}`, lease.ExecutionID, lease.Generation)
	if code := capacityAPIPost(t, ctx, server, issued.Token, "heartbeat", body, &authority); code != http.StatusOK {
		t.Fatalf("heartbeat: HTTP %d, want 200", code)
	}
	if authority.State != "leased" || authority.Lease.ExecutionID != lease.ExecutionID || authority.Lease.Generation != lease.Generation || !authority.Lease.ExpiresAt.After(lease.ExpiresAt) || !authority.Lease.ExpiresAt.After(authority.ServerTime) || !authority.RecoveryUntil.After(authority.Lease.ExpiresAt) {
		t.Fatalf("renewed authority: %+v", authority)
	}
	select {
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	case <-time.After(max(0, time.Until(lease.ExpiresAt)+40*time.Millisecond)):
	}
	if err := client.Validate(ctx, authority.Lease); err != nil {
		t.Fatalf("heartbeat did not preserve current authority beyond original expiry: %v", err)
	}
}

func TestWorkerAPIRecoveryResumesOnlySameWorkerAndSandboxWithinWindow(t *testing.T) {
	queue, credentials := workerQueueFixture(t, 400*time.Millisecond, 2*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	owner, err := credentials.Provision(ctx, "recovering-worker", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	other, err := credentials.Provision(ctx, "other-worker", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(workerapi.NewHandler(queue))
	defer server.Close()
	client, err := workerapi.NewClient(server.URL, owner.Token, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if err := queue.Enqueue(ctx, executionqueue.Execution{ExecutionID: "recovering-execution", AgentRunID: "recovering-run", Source: "pass"}); err != nil {
		t.Fatal(err)
	}
	lease, err := client.Claim(ctx, 0)
	if err != nil || lease == nil {
		t.Fatalf("claim: %+v %v", lease, err)
	}
	if err := client.BindSandbox(ctx, *lease, "sandbox-survives"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	case <-time.After(max(0, time.Until(lease.ExpiresAt)+40*time.Millisecond)):
	}
	if _, err := client.Heartbeat(ctx, *lease, "sandbox-survives"); err == nil {
		t.Fatal("late heartbeat bypassed recovery")
	}
	body := fmt.Sprintf(`{"execution_id":%q,"generation":%d,"sandbox_id":"sandbox-survives"}`, lease.ExecutionID, lease.Generation)
	if code := capacityAPIPost(t, ctx, server, other.Token, "recover", body, nil); code != http.StatusConflict {
		t.Fatalf("other Worker recovery: HTTP %d, want 409", code)
	}
	wrong := fmt.Sprintf(`{"execution_id":%q,"generation":%d,"sandbox_id":"sandbox-replacement"}`, lease.ExecutionID, lease.Generation)
	if code := capacityAPIPost(t, ctx, server, owner.Token, "recover", wrong, nil); code != http.StatusConflict {
		t.Fatalf("replacement Sandbox recovery: HTTP %d, want 409", code)
	}
	var authority workerapi.Authority
	if code := capacityAPIPost(t, ctx, server, owner.Token, "recover", body, &authority); code != http.StatusOK {
		t.Fatalf("same Worker recovery: HTTP %d, want 200", code)
	}
	if authority.State != "leased" || authority.Lease.Generation != lease.Generation || !authority.Lease.ExpiresAt.After(lease.ExpiresAt) {
		t.Fatalf("recovered authority: %+v", authority)
	}
	if err := client.Validate(ctx, authority.Lease); err != nil {
		t.Fatalf("recovered lease: %v", err)
	}
}

func TestWorkerAPIRecoveryWindowExpiresWithoutReassignmentOrCapacityRelease(t *testing.T) {
	queue, credentials := workerQueueFixture(t, 250*time.Millisecond, 500*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	issued, err := credentials.Provision(ctx, "lost-worker", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(workerapi.NewHandler(queue))
	defer server.Close()
	client, err := workerapi.NewClient(server.URL, issued.Token, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if err := queue.Enqueue(ctx, executionqueue.Execution{ExecutionID: "lost-execution", AgentRunID: "lost-run", Source: "pass"}); err != nil {
		t.Fatal(err)
	}
	lease, err := client.Claim(ctx, 0)
	if err != nil || lease == nil {
		t.Fatalf("claim: %+v %v", lease, err)
	}
	if err := client.BindSandbox(ctx, *lease, "sandbox-lost"); err != nil {
		t.Fatal(err)
	}
	before, err := queue.Get(ctx, lease.ExecutionID)
	if err != nil || before.RecoveryUntil == nil {
		t.Fatalf("recovery boundary: %+v %v", before, err)
	}
	select {
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	case <-time.After(max(0, time.Until(lease.ExpiresAt)+30*time.Millisecond)):
	}
	if err := queue.SweepRecovery(ctx); err != nil {
		t.Fatal(err)
	}
	record, err := queue.Get(ctx, lease.ExecutionID)
	if err != nil || record.State != "recovering" {
		t.Fatalf("disconnected state: %+v %v", record, err)
	}
	server.Close()
	restarted := httptest.NewTLSServer(workerapi.NewHandler(queue))
	defer restarted.Close()
	client, err = workerapi.NewClient(restarted.URL, issued.Token, restarted.Client())
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	case <-time.After(max(0, time.Until(*before.RecoveryUntil)+30*time.Millisecond)):
	}
	if err := queue.SweepRecovery(ctx); err != nil {
		t.Fatal(err)
	}
	record, err = queue.Get(ctx, lease.ExecutionID)
	if err != nil || record.State != "worker_lost" || record.RecoveryUntil == nil || !record.RecoveryUntil.Equal(*before.RecoveryUntil) {
		t.Fatalf("terminal state after restart: %+v %v", record, err)
	}
	_, err = client.Recover(ctx, *lease, "sandbox-lost")
	requireWorkerStatus(t, err, http.StatusConflict)
	if next, err := client.Claim(ctx, 0); err != nil || next != nil {
		t.Fatalf("lost Execution was reassigned: %+v %v", next, err)
	}
	if capacity, err := client.Capacity(ctx); err != nil || capacity.Occupied != 1 {
		t.Fatalf("lost Sandbox capacity was released before cleanup: %+v %v", capacity, err)
	}
}

func TestWorkerAPIRecoveryCompletedHeartbeatPreservesLostAcknowledgement(t *testing.T) {
	queue, credentials := workerQueueFixture(t, time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	issued, err := credentials.Provision(ctx, "completed-heartbeat", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(workerapi.NewHandler(queue))
	defer server.Close()
	client, err := workerapi.NewClient(server.URL, issued.Token, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if err := queue.Enqueue(ctx, executionqueue.Execution{ExecutionID: "completed-heartbeat-execution", AgentRunID: "completed-heartbeat-run", Source: "print(42)"}); err != nil {
		t.Fatal(err)
	}
	lease, err := client.Claim(ctx, 0)
	if err != nil || lease == nil {
		t.Fatalf("claim: %+v %v", lease, err)
	}
	if err := client.BindSandbox(ctx, *lease, "sandbox-completed"); err != nil {
		t.Fatal(err)
	}
	result := sandboxsupervisor.ExecutePythonResult{ExecutionID: lease.ExecutionID, TerminalReason: sandboxsupervisor.ExecutionExited, Stdout: "42\n"}
	if err := client.Complete(ctx, *lease, result); err != nil {
		t.Fatal(err)
	}
	for _, operation := range []func(context.Context, workerapi.Lease, string) (workerapi.Authority, error){client.Heartbeat, client.Recover} {
		authority, err := operation(ctx, *lease, "sandbox-completed")
		if err != nil || authority.State != "completed" || !authority.Lease.ExpiresAt.Equal(lease.ExpiresAt) {
			t.Fatalf("completed acknowledgement authority: %+v %v", authority, err)
		}
	}
	if err := client.Complete(ctx, *lease, result); err != nil {
		t.Fatalf("immutable report retry after heartbeat: %v", err)
	}
}

func TestWorkerAPIRecoveryCleanupEndsUnfinishedExecution(t *testing.T) {
	queue, credentials := workerQueueFixture(t, time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	issued, err := credentials.Provision(ctx, "cleanup-recovery-worker", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(workerapi.NewHandler(queue))
	defer server.Close()
	client, err := workerapi.NewClient(server.URL, issued.Token, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if err := queue.Enqueue(ctx, executionqueue.Execution{ExecutionID: "cleanup-recovery-execution", AgentRunID: "cleanup-recovery-run", Source: "pass"}); err != nil {
		t.Fatal(err)
	}
	lease, err := client.Claim(ctx, 0)
	if err != nil || lease == nil {
		t.Fatalf("claim: %+v %v", lease, err)
	}
	if err := client.BindSandbox(ctx, *lease, "sandbox-cleaned"); err != nil {
		t.Fatal(err)
	}
	if err := client.Release(ctx, *lease); err != nil {
		t.Fatal(err)
	}
	record, err := queue.Get(ctx, lease.ExecutionID)
	if err != nil || record.State != "worker_lost" {
		t.Fatalf("cleaned unfinished Execution remains recoverable: %+v %v", record, err)
	}
	_, err = client.Recover(ctx, *lease, "sandbox-cleaned")
	requireWorkerStatus(t, err, http.StatusConflict)
	entries, err := client.Outstanding(ctx)
	if err != nil || len(entries) != 0 {
		t.Fatalf("released cleanup remains outstanding: %+v %v", entries, err)
	}
}

func TestWorkerAPIRecoveryRejectsUntrustedProtocolAndRevocation(t *testing.T) {
	queue, credentials := workerQueueFixture(t, time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	issued, err := credentials.Provision(ctx, "guarded-recovery", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	other, err := credentials.Provision(ctx, "foreign-recovery", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(workerapi.NewHandler(queue))
	defer server.Close()
	client, err := workerapi.NewClient(server.URL, issued.Token, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if err := queue.Enqueue(ctx, executionqueue.Execution{ExecutionID: "guarded-recovery-execution", AgentRunID: "guarded-recovery-run", Source: "pass"}); err != nil {
		t.Fatal(err)
	}
	lease, err := client.Claim(ctx, 0)
	if err != nil || lease == nil {
		t.Fatalf("claim: %+v %v", lease, err)
	}
	if err := client.BindSandbox(ctx, *lease, "sandbox-guarded"); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"execution_id":%q,"generation":%d,"sandbox_id":"sandbox-guarded"}`, lease.ExecutionID, lease.Generation)
	stale := fmt.Sprintf(`{"execution_id":%q,"generation":%d,"sandbox_id":"sandbox-guarded"}`, lease.ExecutionID, lease.Generation+1)
	for _, route := range []string{"bind-sandbox", "heartbeat", "recover"} {
		for _, invalid := range []string{`null`, `{}`, `{"execution_id":null,"generation":1,"sandbox_id":"sandbox-guarded"}`, `{"execution_id":"missing","generation":null,"sandbox_id":"sandbox-guarded"}`, `{"execution_id":"missing","generation":1,"sandbox_id":null}`, `{"execution_id":"missing","generation":1,"sandbox_id":"sandbox-guarded","worker_id":"other"}`, `{"execution_id":"missing","generation":1,"sandbox_id":"sandbox-guarded","sandbox_id":"sandbox-other"}`, body + ` {}`, strings.Repeat(" ", 1024) + body} {
			if code := capacityAPIPost(t, ctx, server, issued.Token, route, invalid, nil); code != http.StatusBadRequest {
				t.Fatalf("%s invalid protocol: HTTP %d, want 400", route, code)
			}
		}
		if code := capacityAPIPost(t, ctx, server, other.Token, route, body, nil); code != http.StatusConflict {
			t.Fatalf("%s foreign Worker: HTTP %d, want 409", route, code)
		}
		if code := capacityAPIPost(t, ctx, server, issued.Token, route, stale, nil); code != http.StatusConflict {
			t.Fatalf("%s stale generation: HTTP %d, want 409", route, code)
		}
	}
	if err := credentials.Revoke(ctx, "guarded-recovery"); err != nil {
		t.Fatal(err)
	}
	for _, route := range []string{"bind-sandbox", "heartbeat", "recover"} {
		if code := capacityAPIPost(t, ctx, server, issued.Token, route, body, nil); code != http.StatusUnauthorized {
			t.Fatalf("revoked %s: HTTP %d, want 401", route, code)
		}
	}
	if code := capacityAPIPost(t, ctx, server, issued.Token, "outstanding", `{}`, nil); code != http.StatusUnauthorized {
		t.Fatalf("revoked outstanding: HTTP %d, want 401", code)
	}
}

func TestWorkerAPIRecoverySweepAndReconnectPreserveOneAuthority(t *testing.T) {
	queue, credentials := workerQueueFixture(t, 400*time.Millisecond, 2*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	issued, err := credentials.Provision(ctx, "racing-recovery", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(workerapi.NewHandler(queue))
	defer server.Close()
	client, err := workerapi.NewClient(server.URL, issued.Token, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if err := queue.Enqueue(ctx, executionqueue.Execution{ExecutionID: "racing-recovery-execution", AgentRunID: "racing-recovery-run", Source: "pass"}); err != nil {
		t.Fatal(err)
	}
	lease, err := client.Claim(ctx, 0)
	if err != nil || lease == nil {
		t.Fatalf("claim: %+v %v", lease, err)
	}
	if err := client.BindSandbox(ctx, *lease, "sandbox-racing"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	case <-time.After(max(0, time.Until(lease.ExpiresAt)+30*time.Millisecond)):
	}
	start := make(chan struct{})
	failures := make(chan error, 9)
	var concurrent sync.WaitGroup
	for range 8 {
		concurrent.Go(func() { <-start; failures <- queue.SweepRecovery(ctx) })
	}
	concurrent.Go(func() {
		<-start
		authority, err := client.Recover(ctx, *lease, "sandbox-racing")
		if err == nil && (authority.State != "leased" || authority.Lease.Generation != lease.Generation) {
			err = fmt.Errorf("changed authority: %+v", authority)
		}
		failures <- err
	})
	close(start)
	concurrent.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := client.Validate(ctx, *lease); err != nil {
		t.Fatalf("sweep revoked freshly recovered authority: %v", err)
	}
	if next, err := client.Claim(ctx, 0); err != nil || next != nil {
		t.Fatalf("recovery duplicated assignment: %+v %v", next, err)
	}
}
