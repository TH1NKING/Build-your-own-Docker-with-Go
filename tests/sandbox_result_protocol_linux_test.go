//go:build linux

package workspace_test

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/TH1NKING/Build-your-own-Docker-with-Go/internal/sandboxsupervisor"
)

func TestSandboxSupervisorResultReadAcceptsOnlyStrictReferences(t *testing.T) {
	fixture := startEmptySandboxSupervisor(t)
	for _, test := range []struct {
		name       string
		parameters string
	}{
		{"missing-sandbox", `{"execution_id":"step"}`},
		{"missing-execution", `{"sandbox_id":"run"}`},
		{"empty-sandbox", `{"sandbox_id":"","execution_id":"step"}`},
		{"empty-execution", `{"sandbox_id":"run","execution_id":""}`},
		{"null-sandbox", `{"sandbox_id":null,"execution_id":"step"}`},
		{"numeric-execution", `{"sandbox_id":"run","execution_id":1}`},
		{"duplicate-reference", `{"sandbox_id":"run","execution_id":"first","execution_id":"second"}`},
		{"noncanonical-case", `{"sandbox_id":"run","Execution_ID":"step"}`},
		{"execution-source", `{"sandbox_id":"run","execution_id":"step","source":"print('must not run')"}`},
		{"execution-stdin", `{"sandbox_id":"run","execution_id":"step","stdin":"input"}`},
		{"host-path", `{"sandbox_id":"run","execution_id":"step","host_path":"/etc/passwd"}`},
		{"output-budget", `{"sandbox_id":"run","execution_id":"step","stdout_bytes":1}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			payload := []byte(`{"schema":"sandbox-supervisor-request/v1","request_id":"read","operation":"get_execution_result","parameters":` + test.parameters + `}`)
			assertSandboxSupervisorErrorCode(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath, payload), "malformed_request")
		})
	}
	for _, field := range []string{"sandbox_id", "execution_id"} {
		for _, value := range []string{"/etc/passwd", "../outside", `run\outside`, "run\x00outside", strings.Repeat("a", 65)} {
			t.Run(field+"/"+value, func(t *testing.T) {
				parameters := map[string]string{"sandbox_id": "run", "execution_id": "step"}
				parameters[field] = value
				payload, err := json.Marshal(map[string]any{
					"schema": "sandbox-supervisor-request/v1", "request_id": "read", "operation": "get_execution_result", "parameters": parameters,
				})
				if err != nil {
					t.Fatal(err)
				}
				assertSandboxSupervisorErrorCode(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath, payload), "invalid_reference")
			})
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	response, err := sandboxsupervisor.NewClient(fixture.socketPath).GetExecutionResult(ctx, sandboxsupervisor.GetExecutionResultRequest{
		RequestID: "read-valid", SandboxID: "run", ExecutionID: "step",
	})
	if err != nil || response.Result != nil || response.Error == nil || response.Error.Code != "operation_unavailable" {
		t.Fatalf("valid result reference should reach the disabled operation: %+v, %v", response, err)
	}
}

func TestSandboxSupervisorRejectsInvalidOutputBudgetsBeforeServing(t *testing.T) {
	commandPath := sandboxdCommandPath(t)
	for _, flag := range []string{"--stdout-bytes", "--stderr-bytes"} {
		for _, value := range []string{"0", "-1", "8388609"} {
			t.Run(flag+"="+value, func(t *testing.T) {
				fixture := newSafeSandboxSupervisorFixture(t)
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				command := exec.CommandContext(ctx, commandPath,
					"--socket", fixture.socketPath, "--profile-store", fixture.profileStore, "--sandbox-root", fixture.sandboxRoot, flag, value)
				output, err := command.CombinedOutput()
				if err == nil || ctx.Err() != nil || !strings.Contains(string(output), "Resource Budget") {
					t.Fatalf("invalid output policy must fail before serving with a Resource Budget diagnostic: %v\n%s", err, output)
				}
			})
		}
	}
}

func TestSandboxSupervisorAcceptsOutputBudgetBoundariesWithoutCallerOverrides(t *testing.T) {
	for _, budget := range []string{"1", "8388608"} {
		t.Run(budget, func(t *testing.T) {
			fixture := newSafeSandboxSupervisorFixture(t)
			t.Cleanup(startSandboxSupervisor(t, fixture, "--stdout-bytes", budget, "--stderr-bytes", budget))
			for _, field := range []string{"stdout_bytes", "stderr_bytes"} {
				payload, err := json.Marshal(map[string]any{
					"schema": "sandbox-supervisor-request/v1", "request_id": "override", "operation": "execute_python",
					"parameters": map[string]any{"sandbox_id": "run", "execution_id": "step", "source": "print(1)", field: 8388608},
				})
				if err != nil {
					t.Fatal(err)
				}
				assertSandboxSupervisorErrorCode(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath, payload), "malformed_request")
			}
		})
	}
}
