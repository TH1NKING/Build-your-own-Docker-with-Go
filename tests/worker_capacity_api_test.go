package workspace_test

import (
	"bytes"
	"context"
	"encoding/json"
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

func TestWorkerAPICapacityBoundsConcurrentClaimsForOneWorker(t *testing.T) {
	queue, credentials := workerQueueFixture(t, time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	issued, err := credentials.Provision(ctx, "capacity-worker", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(workerapi.NewHandler(queue))
	defer server.Close()
	client, err := workerapi.NewClient(server.URL, issued.Token, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	for i := range 8 {
		if err := queue.Enqueue(ctx, executionqueue.Execution{ExecutionID: fmt.Sprintf("capacity-execution-%d", i), AgentRunID: fmt.Sprintf("capacity-run-%d", i), Source: "pass"}); err != nil {
			t.Fatal(err)
		}
	}
	start := make(chan struct{})
	leases := make(chan *workerapi.Lease, 8)
	failures := make(chan error, 8)
	var claims sync.WaitGroup
	for range 8 {
		claims.Go(func() {
			<-start
			lease, err := client.Claim(ctx, 0)
			leases <- lease
			failures <- err
		})
	}
	close(start)
	claims.Wait()
	close(leases)
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	claimed := map[string]bool{}
	for lease := range leases {
		if lease == nil {
			continue
		}
		if claimed[lease.ExecutionID] {
			t.Fatalf("Execution %s was claimed twice", lease.ExecutionID)
		}
		claimed[lease.ExecutionID] = true
	}
	if len(claimed) != 2 {
		t.Fatalf("one Worker claimed %d Executions concurrently, want capacity 2", len(claimed))
	}
}

func capacityAPIPost(t *testing.T, ctx context.Context, server *httptest.Server, token, route, body string, target any) int {
	t.Helper()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/worker/v1/"+route, bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if target != nil && response.StatusCode == http.StatusOK {
		if err := json.NewDecoder(response.Body).Decode(target); err != nil {
			t.Fatal(err)
		}
	}
	return response.StatusCode
}

func TestWorkerAPICapacityStatusSurvivesAPIRestart(t *testing.T) {
	queue, credentials := workerQueueFixture(t, time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	issued, err := credentials.Provision(ctx, "persistent-capacity", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(workerapi.NewHandler(queue))
	defer server.Close()
	client, err := workerapi.NewClient(server.URL, issued.Token, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		if err := queue.Enqueue(ctx, executionqueue.Execution{ExecutionID: fmt.Sprintf("persistent-execution-%d", i), AgentRunID: fmt.Sprintf("persistent-run-%d", i), Source: "pass"}); err != nil {
			t.Fatal(err)
		}
		if lease, err := client.Claim(ctx, 0); err != nil || lease == nil {
			t.Fatalf("claim: %+v %v", lease, err)
		}
	}
	server.Close()
	restarted := httptest.NewTLSServer(workerapi.NewHandler(queue))
	defer restarted.Close()
	var status struct {
		WorkerID   string `json:"worker_id"`
		Configured int    `json:"configured"`
		Occupied   int    `json:"occupied"`
		Available  int    `json:"available"`
	}
	if code := capacityAPIPost(t, ctx, restarted, issued.Token, "capacity", `{}`, &status); code != http.StatusOK {
		t.Fatalf("capacity status: HTTP %d, want 200", code)
	}
	if status.WorkerID != "persistent-capacity" || status.Configured != 2 || status.Occupied != 2 || status.Available != 0 {
		t.Fatalf("capacity after API restart: %+v", status)
	}
}

func TestWorkerAPICapacityWaitsForCleanupRelease(t *testing.T) {
	queue, credentials := workerQueueFixture(t, time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	issued, err := credentials.Provision(ctx, "cleanup-worker", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(workerapi.NewHandler(queue))
	defer server.Close()
	client, err := workerapi.NewClient(server.URL, issued.Token, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		if err := queue.Enqueue(ctx, executionqueue.Execution{ExecutionID: fmt.Sprintf("cleanup-execution-%d", i), AgentRunID: fmt.Sprintf("cleanup-run-%d", i), Source: "pass"}); err != nil {
			t.Fatal(err)
		}
	}
	first, err := client.Claim(ctx, 0)
	if err != nil || first == nil {
		t.Fatalf("first claim: %+v %v", first, err)
	}
	if second, err := client.Claim(ctx, 0); err != nil || second == nil {
		t.Fatalf("second claim: %+v %v", second, err)
	}
	result := sandboxsupervisor.ExecutePythonResult{ExecutionID: first.ExecutionID, TerminalReason: sandboxsupervisor.ExecutionExited}
	if err := client.Complete(ctx, *first, result); err != nil {
		t.Fatal(err)
	}
	if next, err := client.Claim(ctx, 0); err != nil || next != nil {
		t.Fatalf("completion released capacity before cleanup: %+v %v", next, err)
	}
	body := fmt.Sprintf(`{"execution_id":%q,"generation":%d}`, first.ExecutionID, first.Generation)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/worker/v1/release", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+issued.Token)
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("release after cleanup: HTTP %d, want 204", response.StatusCode)
	}
	if err := client.Complete(ctx, *first, result); err != nil {
		t.Fatalf("released completed result lost its immutable acknowledgement: %v", err)
	}
	if next, err := client.Claim(ctx, 0); err != nil || next == nil || next.ExecutionID != "cleanup-execution-2" {
		t.Fatalf("claim after cleanup: %+v %v", next, err)
	}
}

func TestWorkerAPICapacityRejectsInvalidProtocolAndRevokedCredentials(t *testing.T) {
	queue, credentials := workerQueueFixture(t, time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	issued, err := credentials.Provision(ctx, "guarded-capacity", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(workerapi.NewHandler(queue))
	defer server.Close()
	for _, request := range []struct{ route, body string }{
		{"configure-capacity", `{}`},
		{"configure-capacity", `null`},
		{"configure-capacity", `{"capacity":null}`},
		{"configure-capacity", `{"capacity":0}`},
		{"configure-capacity", `{"capacity":65}`},
		{"configure-capacity", `{"capacity":-1}`},
		{"configure-capacity", `{"capacity":1.5}`},
		{"configure-capacity", `{"capacity":2,"capacity":1}`},
		{"configure-capacity", `{"capacity":2,"worker_id":"someone-else"}`},
		{"configure-capacity", `{"capacity":2} {}`},
		{"configure-capacity", strings.Repeat(" ", 1024) + `{"capacity":2}`},
		{"capacity", `null`},
		{"capacity", `{"worker_id":"someone-else"}`},
		{"release", `{"execution_id":null,"generation":1}`},
		{"release", `{"execution_id":"missing","generation":null}`},
		{"release", `{"execution_id":"missing","generation":1,"generation":2}`},
		{"release", `{"execution_id":"missing","generation":1,"worker_id":"other"}`},
	} {
		if code := capacityAPIPost(t, ctx, server, issued.Token, request.route, request.body, nil); code != http.StatusBadRequest {
			t.Fatalf("%s %s: HTTP %d, want 400", request.route, request.body, code)
		}
	}
	if err := credentials.Revoke(ctx, "guarded-capacity"); err != nil {
		t.Fatal(err)
	}
	for _, request := range []struct{ route, body string }{
		{"configure-capacity", `{"capacity":2}`}, {"capacity", `{}`}, {"release", `{"execution_id":"missing","generation":1}`},
	} {
		if code := capacityAPIPost(t, ctx, server, issued.Token, request.route, request.body, nil); code != http.StatusUnauthorized {
			t.Fatalf("revoked %s: HTTP %d, want 401", request.route, code)
		}
	}
}

func TestWorkerAPICapacityLeaseExpiryKeepsReservationUntilCleanup(t *testing.T) {
	queue, credentials := workerQueueFixture(t, 400*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	issued, err := credentials.Provision(ctx, "expired-capacity", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(workerapi.NewHandler(queue))
	defer server.Close()
	client, err := workerapi.NewClient(server.URL, issued.Token, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if err := client.ConfigureCapacity(ctx, 1); err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		if err := queue.Enqueue(ctx, executionqueue.Execution{ExecutionID: fmt.Sprintf("expired-execution-%d", i), AgentRunID: fmt.Sprintf("expired-run-%d", i), Source: "pass"}); err != nil {
			t.Fatal(err)
		}
	}
	lease, err := client.Claim(ctx, 0)
	if err != nil || lease == nil {
		t.Fatalf("claim: %+v %v", lease, err)
	}
	select {
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	case <-time.After(max(0, time.Until(lease.ExpiresAt)+50*time.Millisecond)):
	}
	if next, err := client.Claim(ctx, 0); err != nil || next != nil {
		t.Fatalf("expired lease released capacity without cleanup: %+v %v", next, err)
	}
	if status, err := client.Capacity(ctx); err != nil || status.Occupied != 1 || status.Available != 0 {
		t.Fatalf("expired reservation: %+v %v", status, err)
	}
	if err := client.Release(ctx, *lease); err != nil {
		t.Fatalf("cleanup could not release an expired lease: %v", err)
	}
	if next, err := client.Claim(ctx, 0); err != nil || next == nil || next.ExecutionID != "expired-execution-1" {
		t.Fatalf("claim after expired cleanup: %+v %v", next, err)
	}
}

func TestWorkerAPICapacityReleaseFencesExecutionAuthority(t *testing.T) {
	queue, credentials := workerQueueFixture(t, time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	server := httptest.NewTLSServer(workerapi.NewHandler(queue))
	defer server.Close()
	var clients []*workerapi.Client
	for _, id := range []string{"release-owner", "release-outsider"} {
		issued, err := credentials.Provision(ctx, id, time.Now().Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		client, err := workerapi.NewClient(server.URL, issued.Token, server.Client())
		if err != nil {
			t.Fatal(err)
		}
		clients = append(clients, client)
	}
	if err := queue.Enqueue(ctx, executionqueue.Execution{ExecutionID: "released-execution", AgentRunID: "released-run", Source: "pass"}); err != nil {
		t.Fatal(err)
	}
	lease, err := clients[0].Claim(ctx, 0)
	if err != nil || lease == nil {
		t.Fatalf("claim: %+v %v", lease, err)
	}
	requireWorkerStatus(t, clients[1].Release(ctx, *lease), http.StatusConflict)
	stale := *lease
	stale.Generation++
	requireWorkerStatus(t, clients[0].Release(ctx, stale), http.StatusConflict)
	if err := clients[0].Validate(ctx, *lease); err != nil {
		t.Fatalf("rejected release changed ownership: %v", err)
	}
	for range 2 {
		if err := clients[0].Release(ctx, *lease); err != nil {
			t.Fatalf("idempotent cleanup release: %v", err)
		}
	}
	requireWorkerStatus(t, clients[0].Validate(ctx, *lease), http.StatusConflict)
	result := sandboxsupervisor.ExecutePythonResult{ExecutionID: lease.ExecutionID, TerminalReason: sandboxsupervisor.ExecutionExited}
	requireWorkerStatus(t, clients[0].Complete(ctx, *lease, result), http.StatusConflict)
	if next, err := clients[1].Claim(ctx, 0); err != nil || next != nil {
		t.Fatalf("released uncertain Execution was reassigned: %+v %v", next, err)
	}
}

func TestWorkerAPICapacityConfigurationRequiresNoReservations(t *testing.T) {
	queue, credentials := workerQueueFixture(t, time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	issued, err := credentials.Provision(ctx, "configured-capacity", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(workerapi.NewHandler(queue))
	defer server.Close()
	client, err := workerapi.NewClient(server.URL, issued.Token, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if code := capacityAPIPost(t, ctx, server, issued.Token, "configure-capacity", `{"capacity":3}`, nil); code != http.StatusNoContent {
		t.Fatalf("configure capacity: HTTP %d, want 204", code)
	}
	if err := queue.Enqueue(ctx, executionqueue.Execution{ExecutionID: "configured-execution", AgentRunID: "configured-run", Source: "pass"}); err != nil {
		t.Fatal(err)
	}
	lease, err := client.Claim(ctx, 0)
	if err != nil || lease == nil {
		t.Fatalf("claim: %+v %v", lease, err)
	}
	if code := capacityAPIPost(t, ctx, server, issued.Token, "configure-capacity", `{"capacity":2}`, nil); code != http.StatusConflict {
		t.Fatalf("changed capacity while a Sandbox reservation exists: HTTP %d", code)
	}
	if code := capacityAPIPost(t, ctx, server, issued.Token, "configure-capacity", `{"capacity":3}`, nil); code != http.StatusNoContent {
		t.Fatalf("same capacity is not idempotent: HTTP %d", code)
	}
	if status, err := client.Capacity(ctx); err != nil || status.Configured != 3 || status.Occupied != 1 || status.Available != 2 {
		t.Fatalf("configured capacity: %+v %v", status, err)
	}
	if err := client.Release(ctx, *lease); err != nil {
		t.Fatal(err)
	}
	if code := capacityAPIPost(t, ctx, server, issued.Token, "configure-capacity", `{"capacity":1}`, nil); code != http.StatusNoContent {
		t.Fatalf("configure after cleanup: HTTP %d", code)
	}
	if status, err := client.Capacity(ctx); err != nil || status.Configured != 1 || status.Occupied != 0 || status.Available != 1 {
		t.Fatalf("capacity after cleanup: %+v %v", status, err)
	}
}
