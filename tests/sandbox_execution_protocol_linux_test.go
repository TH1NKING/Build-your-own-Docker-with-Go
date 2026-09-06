//go:build linux

package workspace_test

import (
	"encoding/json"
	"testing"
)

func TestSandboxSupervisorExecutionRejectsCallerPolicyOverrides(t *testing.T) {
	fixture := startEmptySandboxSupervisor(t)
	for _, field := range []string{"command", "host_path", "uid", "environment", "network", "profile_identity", "cgroup_path"} {
		t.Run(field, func(t *testing.T) {
			payload, err := json.Marshal(map[string]any{
				"schema": "sandbox-supervisor-request/v1", "request_id": "req-override", "operation": "execute_python",
				"parameters": map[string]any{"sandbox_id": "run-safe", "execution_id": "execution-safe", "source": "print(42)", field: "forbidden"},
			})
			if err != nil {
				t.Fatal(err)
			}
			assertSandboxSupervisorErrorCode(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath, payload), "malformed_request")
		})
	}
	valid := []byte(`{"schema":"sandbox-supervisor-request/v1","request_id":"req-disabled","operation":"execute_python","parameters":{"sandbox_id":"run-safe","execution_id":"execution-safe","source":"print(42)"}}`)
	assertSandboxSupervisorErrorCode(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath, valid), "operation_unavailable")
}
