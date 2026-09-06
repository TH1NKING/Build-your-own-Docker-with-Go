//go:build linux

package workspace_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/TH1NKING/Build-your-own-Docker-with-Go/internal/sandboxsupervisor"
)

func TestSandboxSupervisorDestroyValidatesClosedParameters(t *testing.T) {
	fixture := startEmptySandboxSupervisor(t)
	for _, test := range []struct{ name, parameters, code string }{
		{"host path", `{"sandbox_id":"run-safe","host_path":"/etc"}`, "malformed_request"},
		{"pid", `{"sandbox_id":"run-safe","pid":1}`, "malformed_request"},
		{"recursive", `{"sandbox_id":"run-safe","recursive":true}`, "malformed_request"},
		{"traversal", `{"sandbox_id":"../escape"}`, "invalid_reference"},
		{"missing", `{}`, "malformed_request"},
		{"duplicate", `{"sandbox_id":"run-safe","sandbox_id":"run-other"}`, "malformed_request"},
		{"case", `{"Sandbox_ID":"run-safe"}`, "malformed_request"},
	} {
		t.Run(test.name, func(t *testing.T) {
			payload, err := json.Marshal(map[string]any{"schema": "sandbox-supervisor-request/v1", "request_id": "req-destroy", "operation": "destroy_sandbox", "parameters": json.RawMessage(test.parameters)})
			if err != nil {
				t.Fatal(err)
			}
			assertSandboxSupervisorErrorCode(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath, payload), test.code)
		})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	response, err := sandboxsupervisor.NewClient(fixture.socketPath).DestroySandbox(ctx, sandboxsupervisor.DestroySandboxRequest{RequestID: "req-client", SandboxID: "run-safe"})
	if err != nil || response.Schema != "sandbox-supervisor-response/v1" || response.RequestID != "req-client" || response.Result != nil || response.Error == nil || response.Error.Code != "operation_unavailable" {
		t.Fatalf("protocol-only destroy through public client: %+v %v", response, err)
	}
}
