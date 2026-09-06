//go:build linux && sandbox_root && profilebundle_root

package workspace_test

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/TH1NKING/Build-your-own-Docker-with-Go/internal/sandboxsupervisor"
)

func TestSandboxExecutionInitLossInvalidatesStateAndTerminatesWorkload(t *testing.T) {
	fixture, identity := installedSandboxExecutionFixture(t)
	t.Cleanup(startSandboxSupervisor(t, fixture, "--subuid-start", "200000", "--subgid-start", "300000", "--subid-count", "65536", "--cgroup-root", os.Getenv("SANDBOX_CGROUP_ROOT")))
	assertSandboxCreated(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath,
		createSandboxWireRequest(t, "create-lost", "run-lost", identity)), "run-lost")
	initPID := sandboxProcessForRoot(t, fixture, identity)
	result := executeSandboxPython(t, fixture, "run-lost", "saved", "open('old-state', 'w').write('old-run')")
	if result.ExitCode != 0 {
		t.Fatalf("prepare Workspace: %+v", result)
	}
	client := sandboxsupervisor.NewClient(fixture.socketPath)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	type outcome struct {
		response sandboxsupervisor.ExecutePythonResponse
		err      error
	}
	finished := make(chan outcome, 1)
	go func() {
		response, err := client.ExecutePython(ctx, sandboxsupervisor.ExecutePythonRequest{
			RequestID: "req-active", SandboxID: "run-lost", ExecutionID: "active",
			Source: "import signal; signal.signal(signal.SIGUSR1, lambda *_: None); signal.pause()",
		})
		finished <- outcome{response, err}
	}()
	workloadPID := waitForSandboxSignalHandler(t, initPID)
	pid, err := strconv.Atoi(initPID)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	failed := <-finished
	if failed.err != nil || failed.response.Result != nil || failed.response.Error == nil || failed.response.Error.Code != "execution_failed" {
		t.Fatalf("Init loss returned false completion: %+v, %v", failed.response, failed.err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		_, err := os.Stat(filepath.Join("/proc", strconv.Itoa(workloadPID)))
		if os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Workload survived Init death")
		}
		time.Sleep(10 * time.Millisecond)
	}
	response, err := client.ExecutePython(ctx, sandboxsupervisor.ExecutePythonRequest{
		RequestID: "req-stale", SandboxID: "run-lost", ExecutionID: "stale", Source: "print('must not run')",
	})
	if err != nil || response.Result != nil || response.Error == nil || response.Error.Code != "sandbox_not_found" {
		t.Fatalf("dead Sandbox remained executable: %+v, %v", response, err)
	}
	// A replacement Agent Run gets a new Sandbox and no previous Workspace.
	assertSandboxSupervisorErrorCode(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath,
		createSandboxWireRequest(t, "create-stale", "run-lost", identity)), "sandbox_exists")
	assertSandboxCreated(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath,
		createSandboxWireRequest(t, "create-replacement", "run-replacement", identity)), "run-replacement")
	replacement := executeSandboxPython(t, fixture, "run-replacement", "replacement", "import os; assert not os.path.exists('old-state'); print('fresh')")
	if replacement.ExitCode != 0 || replacement.Stdout != "fresh\n" {
		t.Fatalf("replacement inherited old state: %+v", replacement)
	}
}
