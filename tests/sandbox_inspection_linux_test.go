//go:build linux && sandbox_root && profilebundle_root

package workspace_test

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/TH1NKING/Build-your-own-Docker-with-Go/internal/sandboxsupervisor"
)

func TestSandboxInspectionRequiresTheOriginalLiveSandbox(t *testing.T) {
	fixture, identity := installedSandboxExecutionFixture(t)
	t.Cleanup(startSandboxSupervisor(t, fixture,
		"--subuid-start", "200000", "--subgid-start", "300000", "--subid-count", "65536",
		"--cgroup-root", os.Getenv("SANDBOX_CGROUP_ROOT")))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client := sandboxsupervisor.NewClient(fixture.socketPath)
	created, err := client.CreateSandbox(ctx, sandboxsupervisor.CreateSandboxRequest{
		RequestID: "create-inspection", SandboxID: "inspection-original", ProfileIdentity: identity,
	})
	if err != nil || created.Error != nil {
		t.Fatalf("create original Sandbox: %+v, %v", created, err)
	}
	request := sandboxsupervisor.InspectSandboxRequest{RequestID: "inspect-original", SandboxID: "inspection-original"}
	response, err := client.InspectSandbox(ctx, request)
	if err != nil || response.Error != nil || response.Result == nil || response.Result.SandboxID != request.SandboxID {
		t.Fatalf("live Sandbox inspection: %+v, %v", response, err)
	}
	destroyed, err := client.DestroySandbox(ctx, sandboxsupervisor.DestroySandboxRequest{RequestID: "destroy-inspection", SandboxID: request.SandboxID})
	if err != nil || destroyed.Error != nil {
		t.Fatalf("destroy original Sandbox: %+v, %v", destroyed, err)
	}
	response, err = client.InspectSandbox(ctx, request)
	if err != nil || response.Result != nil || response.Error == nil || response.Error.Code != sandboxsupervisor.ErrorCodeSandboxNotFound {
		t.Fatalf("destroyed Sandbox must not authorize recovery: %+v, %v", response, err)
	}
}

func TestSandboxInspectionDuringExecutionDoesNotCancelTheWorkload(t *testing.T) {
	fixture, identity := installedSandboxExecutionFixture(t)
	t.Cleanup(startSandboxSupervisor(t, fixture,
		"--subuid-start", "200000", "--subgid-start", "300000", "--subid-count", "65536",
		"--cgroup-root", os.Getenv("SANDBOX_CGROUP_ROOT")))
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	client := sandboxsupervisor.NewClient(fixture.socketPath)
	created, err := client.CreateSandbox(ctx, sandboxsupervisor.CreateSandboxRequest{
		RequestID: "create-busy-inspection", SandboxID: "inspection-running", ProfileIdentity: identity,
	})
	if err != nil || created.Error != nil {
		t.Fatalf("create inspected Sandbox: %+v, %v", created, err)
	}
	finished := make(chan error, 1)
	go func() {
		response, err := client.ExecutePython(ctx, sandboxsupervisor.ExecutePythonRequest{
			RequestID: "execute-inspection", SandboxID: "inspection-running", ExecutionID: "inspection-waiter",
			Source: "import signal, sys; signal.signal(signal.SIGUSR1, lambda *_: sys.exit(0)); signal.pause()",
		})
		if err == nil && (response.Error != nil || response.Result == nil || response.Result.ExitCode != 0) {
			err = fmt.Errorf("inspected Execution: %+v", response)
		}
		finished <- err
	}()
	pid := waitForSandboxSignalHandler(t, sandboxProcessForRoot(t, fixture, identity))
	released := false
	defer func() {
		if !released {
			_ = syscall.Kill(pid, syscall.SIGUSR1)
		}
	}()
	// Inspection must finish while the Execution is blocked, and closing its
	// independent public client connection must leave the executing one alive.
	inspectCtx, cancelInspection := context.WithTimeout(ctx, time.Second)
	response, err := client.InspectSandbox(inspectCtx, sandboxsupervisor.InspectSandboxRequest{
		RequestID: "inspect-running", SandboxID: "inspection-running",
	})
	cancelInspection()
	if err != nil || response.Result == nil || response.Error != nil {
		t.Fatalf("inspect must not wait for running Python: %+v, %v", response, err)
	}
	if err := syscall.Kill(pid, syscall.SIGUSR1); err != nil {
		t.Fatal("inspection destroyed the running Workload:", err)
	}
	released = true
	if err := <-finished; err != nil {
		t.Fatal("inspection interrupted the original Execution:", err)
	}
}

func TestSandboxInspectionRejectsLostInitDespiteRetainedResult(t *testing.T) {
	fixture, identity := installedSandboxExecutionFixture(t)
	t.Cleanup(startSandboxSupervisor(t, fixture,
		"--subuid-start", "200000", "--subgid-start", "300000", "--subid-count", "65536",
		"--cgroup-root", os.Getenv("SANDBOX_CGROUP_ROOT")))
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	client := sandboxsupervisor.NewClient(fixture.socketPath)
	created, err := client.CreateSandbox(ctx, sandboxsupervisor.CreateSandboxRequest{
		RequestID: "create-lost-inspection", SandboxID: "inspection-lost", ProfileIdentity: identity,
	})
	if err != nil || created.Error != nil {
		t.Fatalf("create inspected Sandbox: %+v, %v", created, err)
	}
	result := executeSandboxPython(t, fixture, "inspection-lost", "inspection-result", "print('retained')")
	if result.ExitCode != 0 || result.Stdout != "retained\n" {
		t.Fatalf("prepare retained result: %+v", result)
	}
	pid, err := strconv.Atoi(sandboxProcessForRoot(t, fixture, identity))
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	for {
		response, err := client.InspectSandbox(ctx, sandboxsupervisor.InspectSandboxRequest{RequestID: "inspect-lost", SandboxID: "inspection-lost"})
		if err != nil {
			t.Fatal(err)
		}
		if response.Error != nil {
			if response.Result != nil || response.Error.Code != sandboxsupervisor.ErrorCodeSandboxNotFound {
				t.Fatalf("lost Sandbox inspection: %+v", response)
			}
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("lost Init continued authorizing recovery")
		case <-time.After(time.Millisecond):
		}
	}
	retained, err := client.GetExecutionResult(ctx, sandboxsupervisor.GetExecutionResultRequest{
		RequestID: "read-lost-result", SandboxID: "inspection-lost", ExecutionID: "inspection-result",
	})
	if err != nil || retained.Error != nil || retained.Result == nil || retained.Result.Stdout != "retained\n" {
		t.Fatalf("retained result must remain independently readable: %+v, %v", retained, err)
	}
}
