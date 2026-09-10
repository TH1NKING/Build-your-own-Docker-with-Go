//go:build linux

package workspace_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestSandboxSupervisorExtractionAcceptsDeclaredPaths(t *testing.T) {
	fixture := startEmptySandboxSupervisor(t)
	payload := []byte(`{"schema":"sandbox-supervisor-request/v1","request_id":"declared","operation":"execute_python","parameters":{"sandbox_id":"run","execution_id":"step","source":"pass","output_paths":["reports/result.csv"]}}`)
	assertSandboxSupervisorErrorCode(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath, payload), "operation_unavailable")
}

func TestSandboxSupervisorExtractionRejectsPolicyOverridesAndTooManyFiles(t *testing.T) {
	fixture := newSafeSandboxSupervisorFixture(t)
	t.Cleanup(startSandboxSupervisor(t, fixture, "--extract-files", "1"))
	for _, field := range []string{"extract_file_bytes", "extract_execution_bytes", "extract_sandbox_bytes", "extract_files", "output_file_bytes", "output_total_bytes", "extraction"} {
		payload := []byte(fmt.Sprintf(`{"schema":"sandbox-supervisor-request/v1","request_id":"policy","operation":"execute_python","parameters":{"sandbox_id":"run","execution_id":"step","source":"pass","%s":0}}`, field))
		assertSandboxSupervisorErrorCode(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath, payload), "malformed_request")
	}
	payload := []byte(`{"schema":"sandbox-supervisor-request/v1","request_id":"count","operation":"execute_python","parameters":{"sandbox_id":"run","execution_id":"step","source":"pass","output_paths":["a","b"]}}`)
	assertSandboxSupervisorErrorCode(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath, payload), "invalid_reference")
	for _, paths := range []string{`"a"`, `[1]`, `[null]`, `{}`} {
		payload := []byte(fmt.Sprintf(`{"schema":"sandbox-supervisor-request/v1","request_id":"type","operation":"execute_python","parameters":{"sandbox_id":"run","execution_id":"step","source":"pass","output_paths":%s}}`, paths))
		code := "malformed_request"
		if paths == "[null]" {
			code = "invalid_reference"
		}
		assertSandboxSupervisorErrorCode(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath, payload), code)
	}
}

func TestSandboxSupervisorExtractionValidatesStartupBudgets(t *testing.T) {
	commandPath := sandboxdCommandPath(t)
	for _, arguments := range [][]string{
		{"--extract-file-bytes", "0"}, {"--extract-file-bytes", "-1"}, {"--extract-file-bytes", "33554433"},
		{"--extract-execution-bytes", "0"}, {"--extract-execution-bytes", "-1"}, {"--extract-execution-bytes", "33554433"},
		{"--extract-sandbox-bytes", "0"}, {"--extract-sandbox-bytes", "-1"},
		{"--extract-files", "0"}, {"--extract-files", "-1"}, {"--extract-files", "17"},
	} {
		t.Run(strings.Join(arguments, "="), func(t *testing.T) {
			fixture := newSafeSandboxSupervisorFixture(t)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			args := append([]string{"--socket", fixture.socketPath, "--profile-store", fixture.profileStore, "--sandbox-root", fixture.sandboxRoot}, arguments...)
			output, err := exec.CommandContext(ctx, commandPath, args...).CombinedOutput()
			if err == nil || ctx.Err() != nil || !strings.Contains(string(output), "Resource Budget") {
				t.Fatalf("invalid extraction budget did not fail at startup: %v\n%s", err, output)
			}
			if _, err := os.Lstat(fixture.socketPath); !os.IsNotExist(err) {
				t.Fatalf("invalid budget published socket: %v", err)
			}
		})
	}
	for _, value := range []string{"1", "33554432"} {
		t.Run("valid-"+value, func(t *testing.T) {
			fixture := newSafeSandboxSupervisorFixture(t)
			t.Cleanup(startSandboxSupervisor(t, fixture, "--extract-file-bytes", value, "--extract-execution-bytes", value, "--extract-sandbox-bytes", "9223372036854775807"))
		})
	}
}

func TestSandboxSupervisorExtractionRejectsAmbiguousPaths(t *testing.T) {
	fixture := startEmptySandboxSupervisor(t)
	for _, paths := range [][]string{{""}, {"."}, {".."}, {"../outside"}, {"/etc/passwd"}, {"a/../b"}, {"a//b"}, {"a/./b"}, {"a/"}, {`a\b`}, {"a\x00b"}, {"a", "a"}, {strings.Repeat("x", 1025)}} {
		t.Run(strings.Join(paths, ","), func(t *testing.T) {
			payload, err := json.Marshal(map[string]any{
				"schema": "sandbox-supervisor-request/v1", "request_id": "paths", "operation": "execute_python",
				"parameters": map[string]any{"sandbox_id": "run", "execution_id": "step", "source": "pass", "output_paths": paths},
			})
			if err != nil {
				t.Fatal(err)
			}
			assertSandboxSupervisorErrorCode(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath, payload), "invalid_reference")
		})
	}
}
