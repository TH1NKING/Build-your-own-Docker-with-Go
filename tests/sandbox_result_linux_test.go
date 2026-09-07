//go:build linux && sandbox_root && profilebundle_root

package workspace_test

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/TH1NKING/Build-your-own-Docker-with-Go/internal/sandboxsupervisor"
)

func TestSandboxExecutionResultSnapshotSurvivesLaterExecutions(t *testing.T) {
	fixture, identity := installedSandboxExecutionFixture(t)
	t.Cleanup(startSandboxSupervisor(t, fixture,
		"--subuid-start", "200000", "--subgid-start", "300000", "--subid-count", "65536",
		"--cgroup-root", os.Getenv("SANDBOX_CGROUP_ROOT")))
	assertSandboxCreated(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath,
		createSandboxWireRequest(t, "create", "run-results", identity)), "run-results")
	first := executeSandboxPython(t, fixture, "run-results", "first", "open('state', 'w').write('first'); print('first')")
	if got := readSandboxExecutionResult(t, fixture, "run-results", "first"); got != first {
		t.Fatalf("first snapshot changed: %+v", got)
	}
	second := executeSandboxPython(t, fixture, "run-results", "second", "open('state', 'w').write('second'); print('second')")
	for range 3 {
		if got := readSandboxExecutionResult(t, fixture, "run-results", "first"); got != first {
			t.Fatalf("later Execution changed the first snapshot: %+v", got)
		}
		if got := readSandboxExecutionResult(t, fixture, "run-results", "second"); got != second {
			t.Fatalf("repeated read changed the second snapshot: %+v", got)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	duplicate, err := sandboxsupervisor.NewClient(fixture.socketPath).ExecutePython(ctx, sandboxsupervisor.ExecutePythonRequest{
		RequestID: "duplicate", SandboxID: "run-results", ExecutionID: "first", Source: "open('state', 'w').write('duplicated')",
	})
	if err != nil || duplicate.Result != nil || duplicate.Error == nil || duplicate.Error.Code != "execution_exists" {
		t.Fatalf("duplicate Execution must not overwrite its result or repeat side effects: %+v, %v", duplicate, err)
	}
	probe := executeSandboxPython(t, fixture, "run-results", "probe", "assert open('state').read() == 'second'; print('unchanged')")
	if probe.ExitCode != 0 || probe.Stdout != "unchanged\n" {
		t.Fatalf("result read or duplicate repeated Workload side effects: %+v", probe)
	}
}

func executionResultWireRequest(t *testing.T, sandboxID, executionID string) []byte {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"schema": "sandbox-supervisor-request/v1", "request_id": "read-result", "operation": "get_execution_result",
		"parameters": map[string]string{"sandbox_id": sandboxID, "execution_id": executionID},
	})
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func readSandboxExecutionResult(t *testing.T, fixture sandboxSupervisorFixture, sandboxID, executionID string) sandboxExecutionResult {
	t.Helper()
	payload := exchangeSandboxSupervisorMessage(t, fixture.socketPath, executionResultWireRequest(t, sandboxID, executionID))
	var response struct {
		Result *sandboxExecutionResult          `json:"result"`
		Error  *sandboxsupervisor.ProtocolError `json:"error"`
	}
	if err := json.Unmarshal(payload, &response); err != nil || response.Error != nil || response.Result == nil {
		t.Fatalf("read immutable Execution Result: %s, %v", payload, err)
	}
	return *response.Result
}
