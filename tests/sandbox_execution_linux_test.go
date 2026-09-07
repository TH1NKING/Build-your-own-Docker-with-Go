//go:build linux && sandbox_root && profilebundle_root

package workspace_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/TH1NKING/Build-your-own-Docker-with-Go/internal/sandboxsupervisor"
)

func TestSandboxExecutionKeepsInitAndWorkspaceAcrossSequentialPython(t *testing.T) {
	fixture, identity := installedSandboxExecutionFixture(t)
	t.Cleanup(startSandboxSupervisor(t, fixture,
		"--subuid-start", "200000", "--subgid-start", "300000", "--subid-count", "65536", "--cgroup-root", os.Getenv("SANDBOX_CGROUP_ROOT")))
	assertSandboxCreated(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath,
		createSandboxWireRequest(t, "create", "run-sequential", identity)), "run-sequential")
	initPID := sandboxProcessForRoot(t, fixture, identity)
	first := executeSandboxPython(t, fixture, "run-sequential", "first", `import os
assert os.getuid() == 1000 and os.getgid() == 1000
assert os.getppid() == 1
assert os.readlink('/proc/self') == str(os.getpid())
assert open('/proc/1/comm').read().strip() == 'sandbox-init'
open('answer.txt', 'w').write('42')
print('first-ok')`)
	if first.ExitCode != 0 || first.Stdout != "first-ok\n" {
		t.Fatalf("first Python Execution: %+v", first)
	}
	second := executeSandboxPython(t, fixture, "run-sequential", "second", `import os
assert os.getuid() == 1000 and os.getppid() == 1
assert open('answer.txt').read() == '42'
print('second-ok')`)
	if second.ExitCode != 0 || second.Stdout != "second-ok\n" {
		t.Fatalf("second Python Execution: %+v", second)
	}
	if got := sandboxProcessForRoot(t, fixture, identity); got != initPID {
		t.Fatalf("Sandbox Init changed from %s to %s", initPID, got)
	}
}

func TestSandboxExecutionRejectsOverlapThroughPublicClient(t *testing.T) {
	fixture, identity := installedSandboxExecutionFixture(t)
	t.Cleanup(startSandboxSupervisor(t, fixture, "--subuid-start", "200000", "--subgid-start", "300000", "--subid-count", "65536", "--cgroup-root", os.Getenv("SANDBOX_CGROUP_ROOT")))
	assertSandboxCreated(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath,
		createSandboxWireRequest(t, "create", "run-busy", identity)), "run-busy")
	initPID := sandboxProcessForRoot(t, fixture, identity)
	client := sandboxsupervisor.NewClient(fixture.socketPath)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		response, err := client.ExecutePython(ctx, sandboxsupervisor.ExecutePythonRequest{
			RequestID: "req-wait", SandboxID: "run-busy", ExecutionID: "wait",
			Source: "import signal, sys; signal.signal(signal.SIGUSR1, lambda *_: sys.exit(0)); signal.pause()",
		})
		if err == nil && (response.Error != nil || response.Result == nil || response.Result.ExitCode != 0) {
			err = fmt.Errorf("waiting Execution: %+v", response)
		}
		finished <- err
	}()
	pid := waitForSandboxSignalHandler(t, initPID)
	released := false
	defer func() {
		if !released {
			_ = syscall.Kill(pid, syscall.SIGUSR1)
		}
	}()
	response, err := client.ExecutePython(ctx, sandboxsupervisor.ExecutePythonRequest{
		RequestID: "req-overlap", SandboxID: "run-busy", ExecutionID: "overlap", Source: "raise AssertionError('must not run')",
	})
	if err != nil || response.Error == nil || response.Error.Code != "sandbox_busy" || response.Result != nil {
		t.Fatalf("overlap not rejected: %+v, %v", response, err)
	}
	if err := syscall.Kill(pid, syscall.SIGUSR1); err != nil {
		t.Fatal(err)
	}
	released = true
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	response, err = client.ExecutePython(ctx, sandboxsupervisor.ExecutePythonRequest{
		RequestID: "req-stdin", SandboxID: "run-busy", ExecutionID: "stdin", Source: "import sys; print(sys.stdin.read().upper())", Stdin: "hello",
	})
	if err != nil || response.Result == nil || response.Result.Stdout != "HELLO\n" {
		t.Fatalf("reuse through public client: %+v, %v", response, err)
	}
}

// A caught SIGUSR1 is a kernel-observable readiness event for this Workload;
// release it with a signal after checking the overlap, without sleep races.
func waitForSandboxSignalHandler(t *testing.T, initPID string) int {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		tasks, _ := filepath.Glob(filepath.Join("/proc", initPID, "task", "*", "children"))
		for _, task := range tasks {
			children, _ := os.ReadFile(task)
			for _, child := range strings.Fields(string(children)) {
				status, _ := os.ReadFile(filepath.Join("/proc", child, "status"))
				if !strings.Contains(string(status), "Name:\tpython3\n") {
					continue
				}
				for _, line := range strings.Split(string(status), "\n") {
					fields := strings.Fields(line)
					if len(fields) == 2 && fields[0] == "SigCgt:" {
						mask, _ := strconv.ParseUint(fields[1], 16, 64)
						if mask&(1<<(uint(syscall.SIGUSR1)-1)) != 0 {
							pid, _ := strconv.Atoi(child)
							return pid
						}
					}
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("Python Workload did not install its signal handler")
	return 0
}

func TestSandboxExecutionReapsDetachedDescendantsBeforeReuse(t *testing.T) {
	fixture, identity := installedSandboxExecutionFixture(t)
	t.Cleanup(startSandboxSupervisor(t, fixture, "--subuid-start", "200000", "--subgid-start", "300000", "--subid-count", "65536", "--cgroup-root", os.Getenv("SANDBOX_CGROUP_ROOT")))
	assertSandboxCreated(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath,
		createSandboxWireRequest(t, "create", "run-descendants", identity)), "run-descendants")
	first := executeSandboxPython(t, fixture, "run-descendants", "forked", `import os, time
r, w = os.pipe()
if os.fork() == 0:
    os.close(r)
    os.setsid()
    if os.fork() != 0:
        os._exit(0)
    os.write(w, b'R')
    os.close(w)
    while True: time.sleep(60)
os.close(w)
assert os.read(r, 1) == b'R'
os.close(r)
print('detached-descendant-started', flush=True)`)
	if first.ExitCode != 0 || first.Stdout != "detached-descendant-started\n" {
		t.Fatalf("forking Execution did not finish with descendant-held output pipes: %+v", first)
	}
	second := executeSandboxPython(t, fixture, "run-descendants", "clean", `import os
assert set(n for n in os.listdir('/proc') if n.isdigit()) == {'1', str(os.getpid())}
print('no-descendants-or-zombies')`)
	if second.ExitCode != 0 || second.Stdout != "no-descendants-or-zombies\n" {
		t.Fatalf("Execution inherited processes: %+v", second)
	}
	// The Sandbox budget and Init leaf intentionally survive between
	// Executions. Only full destruction should remove that persistent tree.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	destroyed, err := sandboxsupervisor.NewClient(fixture.socketPath).DestroySandbox(ctx,
		sandboxsupervisor.DestroySandboxRequest{RequestID: "destroy", SandboxID: "run-descendants"})
	if err != nil || destroyed.Error != nil || destroyed.Result == nil {
		t.Fatalf("destroy descendant fixture: %+v, %v", destroyed, err)
	}
	assertNoResourceCgroups(t)
}

func TestSandboxExecutionHasOnlyStdioAndReadOnlyProfile(t *testing.T) {
	fixture, identity := installedSandboxExecutionFixture(t)
	t.Cleanup(startSandboxSupervisor(t, fixture, "--subuid-start", "200000", "--subgid-start", "300000", "--subid-count", "65536", "--cgroup-root", os.Getenv("SANDBOX_CGROUP_ROOT")))
	assertSandboxCreated(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath,
		createSandboxWireRequest(t, "create", "run-boundary", identity)), "run-boundary")
	result := executeSandboxPython(t, fixture, "run-boundary", "probe", `import os, errno
assert os.getgroups() == []
for fd in range(3, 128):
    try: os.fstat(fd)
    except OSError as e: assert e.errno == errno.EBADF
    else: raise AssertionError('inherited descriptor %s' % fd)
for path in ['/sandbox-init', '/opt/python/bin/python3', '/etc/passwd']:
    try: open(path, 'wb')
    except OSError as e: assert e.errno in [errno.EROFS, errno.ENOENT, errno.EACCES]
    else: raise AssertionError('writable Profile')
assert not os.path.exists('/proc/1/root/etc/hostname')
assert os.environ['HOME'] == '/workspace/output'
print('boundary-ok')`)
	if result.ExitCode != 0 || result.Stdout != "boundary-ok\n" {
		t.Fatalf("Workload boundary: %+v", result)
	}
}

func TestSandboxExecutionReportsFailureAndBoundsOutputWithoutLosingInit(t *testing.T) {
	fixture, identity := installedSandboxExecutionFixture(t)
	t.Cleanup(startSandboxSupervisor(t, fixture, "--subuid-start", "200000", "--subgid-start", "300000", "--subid-count", "65536", "--cgroup-root", os.Getenv("SANDBOX_CGROUP_ROOT")))
	assertSandboxCreated(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath,
		createSandboxWireRequest(t, "create", "run-failure", identity)), "run-failure")
	failed := executeSandboxPython(t, fixture, "run-failure", "failed", `import sys
print('failure detail', file=sys.stderr)
sys.exit(7)`)
	if failed.ExitCode != 7 || failed.Stderr != "failure detail\n" {
		t.Fatalf("missing failure status: %+v", failed)
	}
	flood := executeSandboxPython(t, fixture, "run-failure", "flood", `import os
os.write(1, b'x' * 100000)
os.write(2, b'y' * 100000)`)
	if flood.ExitCode != 0 || len(flood.Stdout) != 4096 || len(flood.Stderr) != 4096 || !flood.Truncated {
		t.Fatalf("unbounded or incomplete output: %+v", flood)
	}
	clean := executeSandboxPython(t, fixture, "run-failure", "after-failure", "print('still-usable')")
	if clean.ExitCode != 0 || clean.Stdout != "still-usable\n" {
		t.Fatalf("Init did not survive failed Execution: %+v", clean)
	}
}

func TestSandboxExecutionRejectsNulBeforeDisturbingWorkspace(t *testing.T) {
	fixture, identity := installedSandboxExecutionFixture(t)
	t.Cleanup(startSandboxSupervisor(t, fixture, "--subuid-start", "200000", "--subgid-start", "300000", "--subid-count", "65536", "--cgroup-root", os.Getenv("SANDBOX_CGROUP_ROOT")))
	assertSandboxCreated(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath,
		createSandboxWireRequest(t, "create", "run-invalid", identity)), "run-invalid")
	first := executeSandboxPython(t, fixture, "run-invalid", "saved", "open('state', 'w').write('42')")
	if first.ExitCode != 0 {
		t.Fatalf("prepare state: %+v", first)
	}
	payload, err := json.Marshal(map[string]any{
		"schema": "sandbox-supervisor-request/v1", "request_id": "req-nul", "operation": "execute_python",
		"parameters": map[string]string{"sandbox_id": "run-invalid", "execution_id": "nul", "source": "print(1)\x00"},
	})
	if err != nil {
		t.Fatal(err)
	}
	assertSandboxSupervisorErrorCode(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath, payload), "malformed_request")
	after := executeSandboxPython(t, fixture, "run-invalid", "after-invalid", "assert open('state').read() == '42'; print('preserved')")
	if after.ExitCode != 0 || after.Stdout != "preserved\n" {
		t.Fatalf("invalid request destroyed Workspace: %+v", after)
	}
}

type sandboxExecutionResult struct {
	ExecutionID    string `json:"execution_id"`
	ExitCode       int    `json:"exit_code"`
	Stdout         string `json:"stdout"`
	Stderr         string `json:"stderr"`
	Truncated      bool   `json:"truncated"`
	TerminalReason string `json:"terminal_reason"`
	ResourceUsage  struct {
		OOMEvents           uint64 `json:"oom_events"`
		PIDLimitEvents      uint64 `json:"pid_limit_events"`
		CPUUsec             uint64 `json:"cpu_usec"`
		CPUThrottledPeriods uint64 `json:"cpu_throttled_periods"`
		CPUThrottledUsec    uint64 `json:"cpu_throttled_usec"`
	} `json:"resource_usage"`
}

func executeSandboxPython(t *testing.T, fixture sandboxSupervisorFixture, sandboxID, executionID, source string) sandboxExecutionResult {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"schema": "sandbox-supervisor-request/v1", "request_id": "req-" + executionID,
		"operation": "execute_python", "parameters": map[string]string{
			"sandbox_id": sandboxID, "execution_id": executionID, "source": source, "stdin": "",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	response := exchangeSandboxSupervisorMessage(t, fixture.socketPath, payload)
	var envelope struct {
		Result *sandboxExecutionResult `json:"result"`
		Error  any                     `json:"error"`
	}
	if err := json.Unmarshal(response, &envelope); err != nil || envelope.Result == nil || envelope.Error != nil {
		t.Fatalf("Execution returned %s, want an Execution Result: %v", response, err)
	}
	if envelope.Result.ExecutionID != executionID {
		t.Fatalf("wrong Execution identity: %s", response)
	}
	return *envelope.Result
}

func installedSandboxExecutionFixture(t *testing.T) (sandboxSupervisorFixture, string) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Fatal("Sandbox Execution tests require root in a disposable Linux environment")
	}
	bundle := os.Getenv("SANDBOX_EXECUTION_BUNDLE")
	contents, err := os.ReadFile(bundle)
	if err != nil {
		t.Fatalf("SANDBOX_EXECUTION_BUNDLE must name the real Python Profile Bundle: %v", err)
	}
	digest := independentSHA256(contents)
	fixture := newSafeSandboxSupervisorFixture(t)
	if err := os.Chmod(filepath.Join(fixture.profileStore, "sha256"), 0o755); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(os.Getenv("PROFILE_BUNDLE_CLI"), "install", "--bundle", bundle,
		"--expected-sha256", digest, "--store", fixture.profileStore)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("install Python Profile Bundle: %v\n%s", err, output)
	}
	return fixture, "sha256:" + strings.TrimSpace(digest)
}
