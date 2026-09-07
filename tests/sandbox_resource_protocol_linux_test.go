//go:build linux

package workspace_test

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestSandboxSupervisorAcceptsTrustedResourceBudgetConfiguration(t *testing.T) {
	fixture := newSafeSandboxSupervisorFixture(t)
	t.Cleanup(startSandboxSupervisor(t, fixture,
		"--cpu-millis", "500", "--memory-bytes", "134217728", "--swap-bytes", "0", "--pids-limit", "32", "--execution-timeout", "90s"))
	assertSandboxSupervisorErrorCode(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath,
		[]byte(`{"schema":"sandbox-supervisor-request/v1","request_id":"budget-override","operation":"execute_python","parameters":{"sandbox_id":"run","execution_id":"step","source":"print(1)","cpu_millis":100000}}`)), "malformed_request")
	assertSandboxSupervisorErrorCode(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath,
		[]byte(`{"schema":"sandbox-supervisor-request/v1","request_id":"timeout-override","operation":"execute_python","parameters":{"sandbox_id":"run","execution_id":"step","source":"print(1)","execution_timeout":"0s"}}`)), "malformed_request")
}

func TestSandboxSupervisorRejectsInvalidResourceBudgetsBeforeServing(t *testing.T) {
	commandPath := sandboxdCommandPath(t)
	for _, arguments := range [][]string{
		{"--cpu-millis", "0"}, {"--cpu-millis", "9"}, {"--cpu-millis", "9223372036854775807"},
		{"--memory-bytes", "0"}, {"--memory-bytes", "-1"}, {"--memory-bytes", "4097"},
		{"--swap-bytes", "-1"}, {"--swap-bytes", "4097"}, {"--pids-limit", "0"}, {"--pids-limit", "-1"},
		{"--execution-timeout", "0"}, {"--execution-timeout", "-1s"},
	} {
		t.Run(strings.Join(arguments, "="), func(t *testing.T) {
			fixture := newSafeSandboxSupervisorFixture(t)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			args := []string{"--socket", fixture.socketPath, "--profile-store", fixture.profileStore, "--sandbox-root", fixture.sandboxRoot}
			command := exec.CommandContext(ctx, commandPath, append(args, arguments...)...)
			output, err := command.CombinedOutput()
			if err == nil || ctx.Err() != nil || !strings.Contains(string(output), "Resource Budget") {
				t.Fatalf("invalid policy must fail before serving with a Resource Budget diagnostic: %v\n%s", err, output)
			}
		})
	}
}
