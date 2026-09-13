package workspace_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TH1NKING/Build-your-own-Docker-with-Go/internal/executionqueue"
	"github.com/TH1NKING/Build-your-own-Docker-with-Go/internal/workerapi"
)

func TestWorkerAPIHeartbeatRetriesTruncatedCommittedAuthority(t *testing.T) {
	queue, credentials := workerQueueFixture(t, 250*time.Millisecond, 2*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	issued, err := credentials.Provision(ctx, "truncated-heartbeat-worker", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := queue.Enqueue(ctx, executionqueue.Execution{ExecutionID: "truncated-heartbeat", AgentRunID: "truncated-heartbeat-run", Source: "pass"}); err != nil {
		t.Fatal(err)
	}
	var dropped atomic.Bool
	handler := workerapi.NewHandler(queue)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/worker/v1/heartbeat" || dropped.Swap(true) {
			handler.ServeHTTP(w, r)
			return
		}
		committed := httptest.NewRecorder()
		handler.ServeHTTP(committed, r)
		if committed.Code != http.StatusOK {
			t.Errorf("real heartbeat did not commit before truncation: HTTP %d", committed.Code)
			http.Error(w, "heartbeat failed", http.StatusInternalServerError)
			return
		}
		connection, response, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer connection.Close()
		body := committed.Body.Bytes()
		// Commit the real renewal, then lose the latter half of its HTTPS body.
		_, _ = fmt.Fprintf(response, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(body), body[:len(body)/2])
		_ = response.Flush()
	}))
	defer server.Close()
	client, err := workerapi.NewClient(server.URL, issued.Token, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	lease, err := client.Claim(ctx, 0)
	if err != nil || lease == nil {
		t.Fatalf("claim: %+v %v", lease, err)
	}
	if err := client.BindSandbox(ctx, *lease, "sandbox-truncated-heartbeat"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Heartbeat(ctx, *lease, "sandbox-truncated-heartbeat"); !workerapi.Retryable(err) {
		t.Fatalf("truncated heartbeat must preserve retryable transport failure: %v", err)
	}
	committed, err := queue.Get(ctx, lease.ExecutionID)
	if err != nil || committed.State != "leased" || committed.ExpiresAt == nil || !committed.ExpiresAt.After(lease.ExpiresAt) {
		t.Fatalf("truncated response lost the committed authority: %+v %v", committed, err)
	}
	select {
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	case <-time.After(max(0, time.Until(*committed.ExpiresAt)+20*time.Millisecond)):
	}
	if err := queue.SweepRecovery(ctx); err != nil {
		t.Fatal(err)
	}
	recovered, err := client.Recover(ctx, *lease, "sandbox-truncated-heartbeat")
	if err != nil || recovered.State != "leased" || recovered.Lease.Generation != lease.Generation {
		t.Fatalf("same bound lease could not recover after the lost heartbeat response: %+v %v", recovered, err)
	}
}

func TestWorkerAPIResponseBodyTransportFailuresStayRetryable(t *testing.T) {
	lease, body := workerTransportAuthority(t)
	for _, scenario := range []string{"empty-body", "truncated-json", "truncated-after-json", "body-timeout"} {
		t.Run(scenario, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if scenario == "truncated-after-json" {
					connection, response, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					defer connection.Close()
					// The JSON itself is complete, but the declared HTTP response
					// ends early while the client checks that nothing follows it.
					_, _ = fmt.Fprintf(response, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(body)+8, body)
					_ = response.Flush()
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				if scenario != "empty-body" {
					_, _ = w.Write(body[:len(body)/2])
				}
				if scenario == "body-timeout" {
					w.(http.Flusher).Flush()
					<-r.Context().Done()
				}
			}))
			defer server.Close()
			httpClient := server.Client()
			httpClient.Timeout = 200 * time.Millisecond
			client, err := workerapi.NewClient(server.URL, "synthetic-transport-token", httpClient)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if _, err := client.Heartbeat(ctx, lease, "sandbox-transport"); !workerapi.Retryable(err) {
				t.Fatalf("response-body transport failure became permanent: %v", err)
			}
		})
	}
}

func TestWorkerAPIResponseBodyRejectsProtocolErrorsAndOversizedEnvelopes(t *testing.T) {
	lease, body := workerTransportAuthority(t)
	for _, scenario := range []struct {
		name string
		body string
	}{
		{"invalid-json", `{"lease":]}`},
		{"incomplete-authority", `{}`},
		{"wrong-identity", strings.Replace(string(body), `"transport-execution"`, `"other-execution"`, 1)},
		{"unknown-field", `{"unexpected":true,` + string(body[1:])},
		{"second-json-value", string(body) + `{}`},
		{"oversized-valid-json", string(body) + strings.Repeat(" ", 64<<10)},
		{"oversized-truncated-json", `{"lease":{"execution_id":"` + strings.Repeat("x", 64<<10)},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(scenario.body))
			}))
			defer server.Close()
			client, err := workerapi.NewClient(server.URL, "synthetic-transport-token", server.Client())
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if _, err := client.Heartbeat(ctx, lease, "sandbox-transport"); err == nil || workerapi.Retryable(err) {
				t.Fatalf("invalid complete response must be a permanent protocol failure: %v", err)
			}
		})
	}
}

func TestWorkerAPIResponseBodyPreservesCallerCancellation(t *testing.T) {
	lease, body := workerTransportAuthority(t)
	started := make(chan struct{})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body[:len(body)/2])
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
	}))
	defer server.Close()
	client, err := workerapi.NewClient(server.URL, "synthetic-transport-token", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := client.Heartbeat(ctx, lease, "sandbox-transport"); done <- err }()
	select {
	case <-started:
		cancel()
	case <-ctx.Done():
		t.Fatal("response body never started")
	}
	if err := <-done; !errors.Is(err, context.Canceled) || workerapi.Retryable(err) {
		t.Fatalf("caller cancellation became a retryable transport failure: %v", err)
	}
}

func workerTransportAuthority(t *testing.T) (workerapi.Lease, []byte) {
	t.Helper()
	now := time.Now().UTC()
	lease := workerapi.Lease{ExecutionID: "transport-execution", AgentRunID: "transport-run", Generation: 1, ExpiresAt: now.Add(time.Minute)}
	body, err := json.Marshal(workerapi.Authority{Lease: lease, State: "leased", ServerTime: now, RecoveryUntil: now.Add(2 * time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	return lease, body
}
