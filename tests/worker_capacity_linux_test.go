//go:build linux && sandbox_root && profilebundle_root

package workspace_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/TH1NKING/Build-your-own-Docker-with-Go/internal/executionqueue"
	"github.com/TH1NKING/Build-your-own-Docker-with-Go/internal/worker"
	"github.com/TH1NKING/Build-your-own-Docker-with-Go/internal/workerapi"
)

func TestWorkerExecutionCapacityRunsTwoSandboxesAndQueuesTheThird(t *testing.T) {
	fixture, identity := installedSandboxExecutionFixture(t)
	t.Cleanup(startSandboxSupervisor(t, fixture,
		"--subuid-start", "200000", "--subgid-start", "400000", "--subid-count", "131072",
		"--cgroup-root", os.Getenv("SANDBOX_CGROUP_ROOT")))
	queue, credentials := workerQueueFixture(t, time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	issued, err := credentials.Provision(ctx, "capacity-worker", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	permits := map[string]chan struct{}{}
	for i := 1; i <= 3; i++ {
		id := fmt.Sprintf("capacity-%d", i)
		permits[id] = make(chan struct{})
		if err := queue.Enqueue(ctx, executionqueue.Execution{ExecutionID: id, AgentRunID: "run-" + id, Source: "open('exclusive', 'x').write('one execution')\nprint('capacity-ok')"}); err != nil {
			t.Fatal(err)
		}
	}
	arrived := make(chan string, 3)
	handler := workerapi.NewHandler(queue)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/worker/v1/complete" {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			var report struct {
				ExecutionID string `json:"execution_id"`
			}
			if json.Unmarshal(body, &report) != nil || permits[report.ExecutionID] == nil {
				http.Error(w, "invalid report", 400)
				return
			}
			arrived <- report.ExecutionID
			select {
			case <-permits[report.ExecutionID]:
			case <-r.Context().Done():
				return
			}
		}
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	api, err := workerapi.NewClient(server.URL, issued.Token, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	node, err := worker.New(worker.Config{API: api, SupervisorSocket: fixture.socketPath, ProfileIdentity: identity, PollWait: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- node.Run(runCtx) }()
	defer func() {
		stop()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("stop capacity Worker: %v", err)
		}
	}()
	receive := func() string {
		t.Helper()
		select {
		case id := <-arrived:
			return id
		case <-time.After(8 * time.Second):
			t.Fatal("two Sandbox slots did not make independent progress")
			return ""
		}
	}
	first, second := receive(), receive()
	if first == second || first == "capacity-3" || second == "capacity-3" {
		t.Fatalf("unexpected initial reports %s, %s", first, second)
	}
	third, err := queue.Get(ctx, "capacity-3")
	if err != nil || third.State != "queued" {
		t.Fatalf("third Execution bypassed occupied Sandbox capacity: %+v %v", third, err)
	}
	close(permits[first])
	if next := receive(); next != "capacity-3" {
		t.Fatalf("released slot did not admit third Execution: %s", next)
	}
	close(permits[second])
	close(permits["capacity-3"])
	for {
		complete := true
		for id := range permits {
			record, err := queue.Get(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if record.Result == nil {
				complete = false
				continue
			}
			if record.Result.ExitCode != 0 || record.Result.Stdout != "capacity-ok\n" {
				t.Fatalf("unexpected result: %+v", record.Result)
			}
		}
		capacity, err := api.Capacity(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if complete && capacity.Occupied == 0 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("capacity executions did not finish")
		case <-time.After(20 * time.Millisecond):
		}
	}
}
