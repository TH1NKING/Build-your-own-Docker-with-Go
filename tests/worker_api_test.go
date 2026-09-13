package workspace_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TH1NKING/Build-your-own-Docker-with-Go/internal/executionqueue"
	"github.com/TH1NKING/Build-your-own-Docker-with-Go/internal/migrations"
	"github.com/TH1NKING/Build-your-own-Docker-with-Go/internal/sandboxsupervisor"
	"github.com/TH1NKING/Build-your-own-Docker-with-Go/internal/workerapi"
	"github.com/TH1NKING/Build-your-own-Docker-with-Go/internal/workercredential"
)

func workerQueueFixture(t *testing.T, duration time.Duration, recoveryWindow ...time.Duration) (*executionqueue.Store, *workercredential.Store) {
	t.Helper()
	databaseURL := migrationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := migrations.Run(ctx, databaseURL, migrations.Bundled(), false); err != nil {
		t.Fatal(err)
	}
	queue, err := executionqueue.NewStore(databaseURL, duration)
	if len(recoveryWindow) != 0 {
		queue, err = executionqueue.NewStoreWithRecovery(databaseURL, duration, recoveryWindow[0])
	}
	if err != nil {
		t.Fatal(err)
	}
	return queue, workercredential.NewStore(databaseURL)
}

func TestWorkerAPIRejectsForeignStaleAndExpiredLeases(t *testing.T) {
	queue, credentials := workerQueueFixture(t, 400*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	server := httptest.NewTLSServer(workerapi.NewHandler(queue))
	defer server.Close()
	var clients []*workerapi.Client
	for _, id := range []string{"owner", "outsider"} {
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
	if err := queue.Enqueue(ctx, executionqueue.Execution{ExecutionID: "lease-guard", AgentRunID: "run-guard", Source: "pass"}); err != nil {
		t.Fatal(err)
	}
	lease, err := clients[0].Claim(ctx, 0)
	if err != nil || lease == nil {
		t.Fatalf("claim: %v", err)
	}
	result := sandboxsupervisor.ExecutePythonResult{ExecutionID: lease.ExecutionID, TerminalReason: sandboxsupervisor.ExecutionExited}
	requireWorkerStatus(t, clients[1].Complete(ctx, *lease, result), http.StatusConflict)
	stale := *lease
	stale.Generation++
	requireWorkerStatus(t, clients[0].Validate(ctx, stale), http.StatusConflict)
	requireWorkerStatus(t, clients[0].Complete(ctx, stale, result), http.StatusConflict)
	for {
		record, err := queue.Get(ctx, lease.ExecutionID)
		if err != nil {
			t.Fatal(err)
		}
		if record.State == "recovering" {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("lease did not expire")
		case <-time.After(10 * time.Millisecond):
		}
	}
	requireWorkerStatus(t, clients[0].Validate(ctx, *lease), http.StatusConflict)
	requireWorkerStatus(t, clients[0].Complete(ctx, *lease, result), http.StatusConflict)
	if next, err := clients[1].Claim(ctx, 0); err != nil || next != nil {
		t.Fatalf("uncertain work was reassigned: %+v %v", next, err)
	}
}

func requireWorkerStatus(t *testing.T, err error, status int) {
	t.Helper()
	var response *workerapi.HTTPError
	if !errors.As(err, &response) || response.StatusCode != status {
		t.Fatalf("got %v, want HTTP %d", err, status)
	}
}

func TestWorkerAPIPreservesNULInputAndOutput(t *testing.T) {
	queue, credentials := workerQueueFixture(t, time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	issued, err := credentials.Provision(ctx, "binary-worker", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(workerapi.NewHandler(queue))
	defer server.Close()
	client, err := workerapi.NewClient(server.URL, issued.Token, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if err := queue.Enqueue(ctx, executionqueue.Execution{ExecutionID: "nul-execution", AgentRunID: "nul-run", Source: "print('binary output')", Stdin: "before\x00after"}); err != nil {
		t.Fatal(err)
	}
	lease, err := client.Claim(ctx, 0)
	if err != nil || lease == nil || lease.Stdin != "before\x00after" {
		t.Fatalf("NUL input: %+v %v", lease, err)
	}
	result := sandboxsupervisor.ExecutePythonResult{ExecutionID: lease.ExecutionID, Stdout: "a\x00b", Stderr: "\x00", TerminalReason: sandboxsupervisor.ExecutionExited}
	if err := client.Complete(ctx, *lease, result); err != nil {
		t.Fatal(err)
	}
	record, err := queue.Get(ctx, lease.ExecutionID)
	if err != nil || record.Result == nil || record.Result.Stdout != "a\x00b" || record.Result.Stderr != "\x00" {
		t.Fatalf("NUL output: %+v %v", record, err)
	}
}

func TestWorkerAPIRequiresCompleteDeclaredOutputSnapshots(t *testing.T) {
	queue, credentials := workerQueueFixture(t, time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	issued, err := credentials.Provision(ctx, "output-worker", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(workerapi.NewHandler(queue))
	defer server.Close()
	client, err := workerapi.NewClient(server.URL, issued.Token, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if err := queue.Enqueue(ctx, executionqueue.Execution{ExecutionID: "output-execution", AgentRunID: "output-run", Source: "pass", OutputPaths: []string{"out.bin"}}); err != nil {
		t.Fatal(err)
	}
	lease, err := client.Claim(ctx, 0)
	if err != nil || lease == nil {
		t.Fatalf("claim: %v", err)
	}
	result := sandboxsupervisor.ExecutePythonResult{ExecutionID: lease.ExecutionID, TerminalReason: sandboxsupervisor.ExecutionExited}
	requireWorkerStatus(t, client.Complete(ctx, *lease, result), http.StatusBadRequest)
	result.Outputs = []sandboxsupervisor.ExtractedOutput{{Path: "out.bin", Size: 1, Content: []byte{0}, SHA256: "forged"}}
	requireWorkerStatus(t, client.Complete(ctx, *lease, result), http.StatusBadRequest)
	result.Outputs = nil
	result.OutputError = sandboxsupervisor.OutputUnavailable
	if err := client.Complete(ctx, *lease, result); err != nil {
		t.Fatalf("explicit unavailable snapshot must be reportable: %v", err)
	}
}

func TestWorkerAPIRejectsOversizedOutputArrayBeforeItsEnd(t *testing.T) {
	queue, credentials := workerQueueFixture(t, time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	issued, err := credentials.Provision(ctx, "array-worker", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	// A deliberately unfinished body at the published HTTP seam verifies that
	// the seventeenth element is rejected before waiting for or decoding its body.
	prefix := `{"execution_id":"array-execution","generation":1,"result":{"outputs":[` + strings.Repeat(`{},`, 16) + `{`
	reader, writer := io.Pipe()
	request := httptest.NewRequest("POST", "https://worker.test/worker/v1/complete", reader).WithContext(ctx)
	request.Header.Set("Authorization", "Bearer "+issued.Token)
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { workerapi.NewHandler(queue).ServeHTTP(response, request); close(done) }()
	if _, err := writer.Write([]byte(prefix)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
		if response.Code != http.StatusBadRequest {
			t.Fatalf("unbounded array status: %d", response.Code)
		}
	case <-ctx.Done():
		writer.Close()
		t.Fatal("decoder waited for the rest of an already excessive output array")
	}
	writer.Close()
	reader.Close()
}

func TestWorkerAPIRevocationInterruptsLongPollAndBlocksReports(t *testing.T) {
	queue, credentials := workerQueueFixture(t, time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	issued, err := credentials.Provision(ctx, "revoked-worker", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(workerapi.NewHandler(queue))
	defer server.Close()
	client, err := workerapi.NewClient(server.URL, issued.Token, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if err := queue.Enqueue(ctx, executionqueue.Execution{ExecutionID: "revoked-report", AgentRunID: "revoked-run", Source: "pass"}); err != nil {
		t.Fatal(err)
	}
	lease, err := client.Claim(ctx, 0)
	if err != nil || lease == nil {
		t.Fatalf("claim: %v", err)
	}
	poll := make(chan error, 1)
	go func() { _, err := client.Claim(ctx, 5*time.Second); poll <- err }()
	if err := credentials.Revoke(ctx, "revoked-worker"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-poll:
		requireWorkerStatus(t, err, http.StatusUnauthorized)
	case <-ctx.Done():
		t.Fatal("revoked long poll did not terminate")
	}
	result := sandboxsupervisor.ExecutePythonResult{ExecutionID: lease.ExecutionID, TerminalReason: sandboxsupervisor.ExecutionExited}
	requireWorkerStatus(t, client.Complete(ctx, *lease, result), http.StatusUnauthorized)
}

func TestWorkerAPILongPollFindsNewWorkAndAcceptsCancellation(t *testing.T) {
	queue, credentials := workerQueueFixture(t, time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	issued, err := credentials.Provision(ctx, "poller", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(workerapi.NewHandler(queue))
	defer server.Close()
	client, err := workerapi.NewClient(server.URL, issued.Token, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	arrived := make(chan *workerapi.Lease, 1)
	failures := make(chan error, 1)
	go func() { lease, err := client.Claim(ctx, 5*time.Second); arrived <- lease; failures <- err }()
	if err := queue.Enqueue(ctx, executionqueue.Execution{ExecutionID: "new-work", AgentRunID: "new-run", Source: "pass"}); err != nil {
		t.Fatal(err)
	}
	select {
	case lease := <-arrived:
		if lease == nil || lease.ExecutionID != "new-work" {
			t.Fatalf("new work not returned: %+v", lease)
		}
	case <-ctx.Done():
		t.Fatal("new work did not wake polling")
	}
	if err := <-failures; err != nil {
		t.Fatal(err)
	}
	cancelled, stop := context.WithCancel(ctx)
	stop()
	if _, err := client.Claim(cancelled, time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
}

func TestWorkerAPIRejectsUntrustedRequestFieldsAndOperatorRoutes(t *testing.T) {
	queue, credentials := workerQueueFixture(t, time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	issued, err := credentials.Provision(ctx, "schema-worker", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(workerapi.NewHandler(queue))
	defer server.Close()
	for _, tc := range []struct {
		route, body string
		status      int
	}{
		{"claim", `{"wait_ms":0,"database_url":"secret"}`, 400},
		{"claim", `{"wait_ms":0} {}`, 400},
		{"claim", `{"wait_ms":25001}`, 400},
		{"claim", `{"wait_ms":0,"wait_ms":1}`, 400},
		{"claim", `null`, 400},
		{"validate", `{"execution_id":"missing","generation":1,"worker_id":"forged"}`, 400},
		{"../operator/v1/conversations", `{}`, 404},
	} {
		req, err := http.NewRequestWithContext(ctx, "POST", server.URL+"/worker/v1/"+tc.route, bytes.NewBufferString(tc.body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+issued.Token)
		response, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != tc.status {
			t.Errorf("%s status %d body %s", tc.route, response.StatusCode, body)
		}
	}
}

func TestWorkerAPIClientRejectsInsecureDefaultTransport(t *testing.T) {
	original := http.DefaultTransport
	http.DefaultTransport = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	t.Cleanup(func() { http.DefaultTransport = original })
	if _, err := workerapi.NewClient("https://localhost", "synthetic-test-token", nil); err == nil {
		t.Fatal("insecure default transport accepted")
	}
}

func TestWorkerAPIClientRejectsUnexpectedSuccessResponses(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer server.Close()
	client, err := workerapi.NewClient(server.URL, "synthetic-test-token", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := client.Claim(ctx, 0); err == nil {
		t.Fatal("empty successful response was treated as an Execution Lease")
	}
	lease := workerapi.Lease{ExecutionID: "example", Generation: 1}
	if err := client.Validate(ctx, lease); err == nil {
		t.Fatal("HTTP 200 was treated as a valid lease acknowledgement")
	}
	if err := client.Complete(ctx, lease, sandboxsupervisor.ExecutePythonResult{}); err == nil {
		t.Fatal("HTTP 200 was treated as a committed result acknowledgement")
	}
}

func TestWorkerAPIResultRetryIsImmutableAndDurable(t *testing.T) {
	queue, credentials := workerQueueFixture(t, 2*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	issued, err := credentials.Provision(ctx, "reporter", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := queue.Enqueue(ctx, executionqueue.Execution{ExecutionID: "execution-report", AgentRunID: "run-report", Source: "print(42)"}); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(workerapi.NewHandler(queue))
	defer server.Close()
	client, err := workerapi.NewClient(server.URL, issued.Token, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	lease, err := client.Claim(ctx, 0)
	if err != nil || lease == nil {
		t.Fatalf("claim: %v", err)
	}
	if err := client.Validate(ctx, *lease); err != nil {
		t.Fatal(err)
	}
	result := sandboxsupervisor.ExecutePythonResult{ExecutionID: lease.ExecutionID, Stdout: "42\n", TerminalReason: sandboxsupervisor.ExecutionExited}
	if err := client.Complete(ctx, *lease, result); err != nil {
		t.Fatalf("complete: %v", err)
	}
	server.Close()
	restarted := httptest.NewTLSServer(workerapi.NewHandler(queue))
	defer restarted.Close()
	client, err = workerapi.NewClient(restarted.URL, issued.Token, restarted.Client())
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Complete(ctx, *lease, result); err != nil {
		t.Fatalf("retry after API restart: %v", err)
	}
	// A committed result can still be acknowledged after the old lease expires;
	// this does not grant authority to execute or change that result again.
	select {
	case <-time.After(max(0, time.Until(lease.ExpiresAt)+50*time.Millisecond)):
	case <-ctx.Done():
		t.Fatal("waiting for completed lease expiry")
	}
	if err := client.Complete(ctx, *lease, result); err != nil {
		t.Fatalf("late identical acknowledgement: %v", err)
	}
	result.Stdout = "forged replacement"
	if err := client.Complete(ctx, *lease, result); err == nil {
		t.Fatal("conflicting retry replaced immutable result")
	}
	record, err := queue.Get(ctx, lease.ExecutionID)
	if err != nil || record.State != "completed" || record.Result == nil || record.Result.Stdout != "42\n" {
		t.Fatalf("durable result: %+v, %v", record, err)
	}
	if next, err := client.Claim(ctx, 0); err != nil || next != nil {
		t.Fatalf("completed work was dispatched again: %+v, %v", next, err)
	}
}

func TestWorkerAPICompetingClaimsHaveOneOwner(t *testing.T) {
	queue, credentials := workerQueueFixture(t, time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	request := executionqueue.Execution{ExecutionID: "execution-one", AgentRunID: "run-one", Source: "print(42)"}
	if err := queue.Enqueue(ctx, request); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(workerapi.NewHandler(queue))
	defer server.Close()
	var clients []*workerapi.Client
	for i := 0; i < 8; i++ {
		issued, err := credentials.Provision(ctx, fmt.Sprintf("worker-%d", i), time.Now().Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		client, err := workerapi.NewClient(server.URL, issued.Token, server.Client())
		if err != nil {
			t.Fatal(err)
		}
		clients = append(clients, client)
	}
	start := make(chan struct{})
	results := make(chan *workerapi.Lease, len(clients))
	errors := make(chan error, len(clients))
	var workers sync.WaitGroup
	for _, client := range clients {
		workers.Go(func() { <-start; lease, err := client.Claim(ctx, 0); results <- lease; errors <- err })
	}
	close(start)
	workers.Wait()
	close(results)
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	owners := 0
	for lease := range results {
		if lease == nil {
			continue
		}
		owners++
		if lease.ExecutionID != request.ExecutionID || lease.Generation != 1 || lease.Source != "print(42)" {
			t.Fatalf("incorrect lease: %+v", lease)
		}
	}
	if owners != 1 {
		t.Fatalf("got %d owners for one Execution generation", owners)
	}
}
