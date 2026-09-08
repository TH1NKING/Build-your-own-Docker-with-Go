//go:build linux

package workspace_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestSandboxSupervisorAcceptsTrustedResourceBudgetConfiguration(t *testing.T) {
	fixture := newSafeSandboxSupervisorFixture(t)
	t.Cleanup(startSandboxSupervisor(t, fixture,
		"--cpu-millis", "500", "--memory-bytes", "134217728", "--swap-bytes", "0", "--pids-limit", "32", "--execution-timeout", "90s",
		"--workspace-bytes", "8192", "--workspace-files", "3", "--temporary-bytes", "4096", "--temporary-files", "2"))
	assertSandboxSupervisorErrorCode(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath,
		[]byte(`{"schema":"sandbox-supervisor-request/v1","request_id":"budget-override","operation":"execute_python","parameters":{"sandbox_id":"run","execution_id":"step","source":"print(1)","cpu_millis":100000}}`)), "malformed_request")
	assertSandboxSupervisorErrorCode(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath,
		[]byte(`{"schema":"sandbox-supervisor-request/v1","request_id":"timeout-override","operation":"execute_python","parameters":{"sandbox_id":"run","execution_id":"step","source":"print(1)","execution_timeout":"0s"}}`)), "malformed_request")
	for _, field := range []string{"workspace_bytes", "workspace_files", "temporary_bytes", "temporary_files", "storage"} {
		for _, operation := range []string{"create_sandbox", "execute_python"} {
			parameters := `"sandbox_id":"run","execution_id":"step","source":"print(1)"`
			if operation == "create_sandbox" {
				parameters = `"sandbox_id":"run","profile_identity":"sha256:` + strings.Repeat("a", 64) + `"`
			}
			request := fmt.Sprintf(`{"schema":"sandbox-supervisor-request/v1","request_id":"storage-override","operation":%q,"parameters":{%s,%q:0}}`, operation, parameters, field)
			assertSandboxSupervisorErrorCode(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath, []byte(request)), "malformed_request")
		}
	}
}

func TestSandboxSupervisorRejectsInvalidResourceBudgetsBeforeServing(t *testing.T) {
	commandPath := sandboxdCommandPath(t)
	for _, arguments := range [][]string{
		{"--cpu-millis", "0"}, {"--cpu-millis", "9"}, {"--cpu-millis", "9223372036854775807"},
		{"--memory-bytes", "0"}, {"--memory-bytes", "-1"}, {"--memory-bytes", "4097"},
		{"--swap-bytes", "-1"}, {"--swap-bytes", "4097"}, {"--pids-limit", "0"}, {"--pids-limit", "-1"},
		{"--execution-timeout", "0"}, {"--execution-timeout", "-1s"},
		{"--workspace-bytes", "0"}, {"--workspace-bytes", "-1"}, {"--workspace-bytes", "4097"},
		{"--temporary-bytes", "0"}, {"--temporary-bytes", "-1"}, {"--temporary-bytes", "4097"},
		{"--workspace-files", "0"}, {"--workspace-files", "-1"}, {"--workspace-files", "9223372036854775807"},
		{"--temporary-files", "0"}, {"--temporary-files", "-1"}, {"--temporary-files", "9223372036854775807"},
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
			if _, err := os.Lstat(fixture.socketPath); !os.IsNotExist(err) {
				t.Fatalf("invalid policy published a socket: %v", err)
			}
		})
	}
}
