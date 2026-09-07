//go:build linux && sandbox_root && profilebundle_root

package workspace_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/TH1NKING/Build-your-own-Docker-with-Go/internal/sandboxsupervisor"
)

func TestSandboxExecutionOutputBudgetsDefault(t *testing.T) {
	fixture, identity := installedSandboxExecutionFixture(t)
	t.Cleanup(startSandboxSupervisor(t, fixture,
		"--subuid-start", "200000", "--subgid-start", "300000", "--subid-count", "65536",
		"--cgroup-root", os.Getenv("SANDBOX_CGROUP_ROOT")))
	assertSandboxCreated(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath,
		createSandboxWireRequest(t, "create", "run-output", identity)), "run-output")
	result := executeSandboxPython(t, fixture, "run-output", "flood", `import os, sys, threading
def write(fd, value):
    for _ in range(256):
        os.write(fd, value * 8192)
thread = threading.Thread(target=write, args=(2, b'e'))
thread.start()
write(1, b'o')
thread.join()
sys.exit(7)`)
	if result.ExitCode != 7 || result.TerminalReason != "exited" ||
		result.Stdout != strings.Repeat("o", 1<<20) || result.Stderr != strings.Repeat("e", 1<<20) ||
		!result.StdoutTruncated || !result.StderrTruncated || !result.Truncated {
		t.Fatalf("independent default output budgets: exit=%d reason=%s stdout=%d stderr=%d truncated=%t/%t",
			result.ExitCode, result.TerminalReason, len(result.Stdout), len(result.Stderr), result.StdoutTruncated, result.StderrTruncated)
	}
	clean := executeSandboxPython(t, fixture, "run-output", "clean", "print('usable')")
	if clean.Stdout != "usable\n" || clean.ExitCode != 0 || clean.Truncated {
		t.Fatalf("output flood prevented reuse: %+v", clean)
	}
}

func TestSandboxExecutionOutputEscapingFitsPublicResponses(t *testing.T) {
	fixture, identity := installedSandboxExecutionFixture(t)
	t.Cleanup(startSandboxSupervisor(t, fixture,
		"--subuid-start", "200000", "--subgid-start", "300000", "--subid-count", "65536",
		"--cgroup-root", os.Getenv("SANDBOX_CGROUP_ROOT"), "--stdout-bytes", "2097152"))
	assertSandboxCreated(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath,
		createSandboxWireRequest(t, "create", "run-escaped", identity)), "run-escaped")
	client := sandboxsupervisor.NewClient(fixture.socketPath)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	response, err := client.ExecutePython(ctx, sandboxsupervisor.ExecutePythonRequest{
		RequestID: "escaped", SandboxID: "run-escaped", ExecutionID: "escaped",
		Source: "import os; os.write(1, b'\\x00' * (2 * 1024 * 1024)); os.write(2, b'<' * (1024 * 1024))",
	})
	if err != nil || response.Error != nil || response.Result == nil {
		t.Fatalf("JSON expansion lost a valid bounded result: error=%v transport=%v", response.Error, err)
	}
	result := response.Result
	if result.ExitCode != 0 || result.Truncated || result.StdoutTruncated || result.StderrTruncated ||
		result.Stdout != strings.Repeat("\x00", 2<<20) || result.Stderr != strings.Repeat("<", 1<<20) {
		t.Fatalf("escaped text changed at the wire boundary: exit=%d stdout=%d stderr=%d truncated=%t", result.ExitCode, len(result.Stdout), len(result.Stderr), result.Truncated)
	}
	snapshot, err := client.GetExecutionResult(ctx, sandboxsupervisor.GetExecutionResultRequest{
		RequestID: "read-escaped", SandboxID: "run-escaped", ExecutionID: "escaped",
	})
	if err != nil || snapshot.Error != nil || snapshot.Result == nil || *snapshot.Result != *result {
		t.Fatalf("public client could not reread the large immutable result: error=%v transport=%v", snapshot.Error, err)
	}
}

func TestSandboxExecutionOutputBudgetsAreIndependent(t *testing.T) {
	fixture, identity := installedSandboxExecutionFixture(t)
	t.Cleanup(startSandboxSupervisor(t, fixture,
		"--subuid-start", "200000", "--subgid-start", "300000", "--subid-count", "65536",
		"--cgroup-root", os.Getenv("SANDBOX_CGROUP_ROOT"), "--stdout-bytes", "31", "--stderr-bytes", "47"))
	assertSandboxCreated(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath,
		createSandboxWireRequest(t, "create", "run-small-output", identity)), "run-small-output")
	for _, test := range []struct {
		name, source, stdout, stderr     string
		stdoutTruncated, stderrTruncated bool
	}{
		{"empty", "pass", "", "", false, false},
		{"exact", "os.write(1, b'o' * 31); os.write(2, b'e' * 47)", strings.Repeat("o", 31), strings.Repeat("e", 47), false, false},
		{"stdout", "os.write(1, b'o' * 32); os.write(2, b'error')", strings.Repeat("o", 31), "error", true, false},
		{"stderr", "os.write(1, b'ok'); os.write(2, b'e' * 48)", "ok", strings.Repeat("e", 47), false, true},
		{"utf8", "os.write(1, ('界' * 11).encode()); os.write(2, b'\\xffx' * 24)", strings.Repeat("界", 10), strings.Repeat("\uFFFDx", 11) + "\uFFFD", true, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := executeSandboxPython(t, fixture, "run-small-output", test.name, "import os\n"+test.source)
			if result.ExitCode != 0 || result.Stdout != test.stdout || result.Stderr != test.stderr ||
				result.StdoutTruncated != test.stdoutTruncated || result.StderrTruncated != test.stderrTruncated ||
				result.Truncated != (test.stdoutTruncated || test.stderrTruncated) {
				t.Fatalf("independent output policy for %s: %+v", test.name, result)
			}
		})
	}
	// Policy is supplied by trusted startup configuration, never by an Execution.
	for _, field := range []string{"stdout_bytes", "stderr_bytes"} {
		payload := []byte(fmt.Sprintf(`{"schema":"sandbox-supervisor-request/v1","request_id":"override","operation":"execute_python","parameters":{"sandbox_id":"run-small-output","execution_id":"override","source":"pass","%s":8388608}}`, field))
		assertSandboxSupervisorErrorCode(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath, payload), "malformed_request")
	}
}
