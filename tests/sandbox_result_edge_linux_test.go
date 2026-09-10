//go:build linux && sandbox_root && profilebundle_root

package workspace_test

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"reflect"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/TH1NKING/Build-your-own-Docker-with-Go/internal/sandboxsupervisor"
)

func TestSandboxExecutionResultReadsDoNotOwnRunningExecution(t *testing.T) {
	fixture, identity := installedSandboxExecutionFixture(t)
	t.Cleanup(startSandboxSupervisor(t, fixture,
		"--subuid-start", "200000", "--subgid-start", "300000", "--subid-count", "65536",
		"--cgroup-root", os.Getenv("SANDBOX_CGROUP_ROOT")))
	const sandboxID = "run-readers"
	assertSandboxCreated(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath,
		createSandboxWireRequest(t, "create", sandboxID, identity)), sandboxID)
	initPID := sandboxProcessForRoot(t, fixture, identity)
	// A large snapshot lets the disconnect probe close during response delivery.
	first := executeSandboxPython(t, fixture, sandboxID, "first", "import os; os.write(1, b'x' * 1048576)")
	client := sandboxsupervisor.NewClient(fixture.socketPath)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	snapshot, err := client.GetExecutionResult(ctx, sandboxsupervisor.GetExecutionResultRequest{
		RequestID: "first-read", SandboxID: sandboxID, ExecutionID: "first",
	})
	if err != nil || snapshot.Error != nil || snapshot.Result == nil || snapshot.Result.Stdout != first.Stdout {
		t.Fatalf("prepare published snapshot: %+v, %v", snapshot, err)
	}
	finished := startResultWaitingExecution(ctx, client, sandboxID)
	pid := waitForSandboxSignalHandler(t, initPID)
	released := false
	defer func() {
		if !released {
			_ = syscall.Kill(pid, syscall.SIGUSR1)
		}
	}()
	assertPublicExecutionResultError(t, client, sandboxID, "pending", "execution_result_not_ready")
	assertPublicExecutionResultError(t, client, sandboxID, "unknown", "execution_result_not_found")

	const readers = 6
	reads := make(chan error, readers)
	for i := range readers {
		go func() {
			readCtx, stop := context.WithTimeout(ctx, 3*time.Second)
			defer stop()
			response, err := client.GetExecutionResult(readCtx, sandboxsupervisor.GetExecutionResultRequest{
				RequestID: fmt.Sprintf("concurrent-%d", i), SandboxID: sandboxID, ExecutionID: "first",
			})
			if err == nil && (response.Error != nil || response.Result == nil || !reflect.DeepEqual(response.Result, snapshot.Result)) {
				err = fmt.Errorf("concurrent read changed the immutable snapshot or waited on Init: %+v", response.Error)
			}
			reads <- err
		}()
	}
	for range readers {
		if err := <-reads; err != nil {
			t.Fatal(err)
		}
	}
	disconnectSandboxResultReader(t, fixture, sandboxID, "first")
	assertPublicExecutionResultError(t, client, sandboxID, "pending", "execution_result_not_ready")
	if err := syscall.Kill(pid, syscall.SIGUSR1); err != nil {
		t.Fatal(err)
	}
	released = true
	outcome := <-finished
	if outcome.err != nil || outcome.response.Error != nil || outcome.response.Result == nil ||
		outcome.response.Result.ExitCode != 0 || outcome.response.Result.Stdout != "resumed\n" || outcome.response.Result.TerminalReason != "exited" {
		t.Fatalf("result readers cancelled or disturbed the running Execution: %+v, %v", outcome.response, outcome.err)
	}
	if got := readSandboxExecutionResult(t, fixture, sandboxID, "first"); got != first {
		t.Fatal("running Execution or reader disconnect changed the old snapshot")
	}
	clean := executeSandboxPython(t, fixture, sandboxID, "clean", "print('still-ready')")
	if clean.ExitCode != 0 || clean.Stdout != "still-ready\n" || sandboxProcessForRoot(t, fixture, identity) != initPID {
		t.Fatalf("result read acquired Sandbox cancellation rights: %+v", clean)
	}
}

func TestSandboxExecutionResultSurvivesInitDeathUntilExplicitDestroy(t *testing.T) {
	fixture, identity := installedSandboxExecutionFixture(t)
	t.Cleanup(startSandboxSupervisor(t, fixture,
		"--subuid-start", "200000", "--subgid-start", "300000", "--subid-count", "65536",
		"--cgroup-root", os.Getenv("SANDBOX_CGROUP_ROOT")))
	const sandboxID = "run-lost-results"
	assertSandboxCreated(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath,
		createSandboxWireRequest(t, "create", sandboxID, identity)), sandboxID)
	first := executeSandboxPython(t, fixture, sandboxID, "first", "import sys; print('saved'); print('detail', file=sys.stderr); sys.exit(7)")
	initPID := sandboxProcessForRoot(t, fixture, identity)
	client := sandboxsupervisor.NewClient(fixture.socketPath)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	finished := startResultWaitingExecution(ctx, client, sandboxID)
	waitForSandboxSignalHandler(t, initPID)
	pid, err := strconv.Atoi(initPID)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	outcome := <-finished
	if outcome.err != nil || outcome.response.Result != nil || outcome.response.Error == nil || outcome.response.Error.Code != "execution_failed" {
		t.Fatalf("Init death must fail its active Execution: %+v, %v", outcome.response, outcome.err)
	}
	for range 3 {
		if got := readSandboxExecutionResult(t, fixture, sandboxID, "first"); got != first {
			t.Fatalf("Init death erased or altered a published result: %+v", got)
		}
	}
	assertPublicExecutionResultError(t, client, sandboxID, "pending", "execution_result_not_found")
	assertSandboxDestroyed(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath,
		destroySandboxWireRequest(t, sandboxID)), sandboxID)
	assertPublicExecutionResultError(t, client, sandboxID, "first", "execution_result_not_found")
}

func TestSandboxExecutionResultCapacityIsSharedAndReleasedByDestroy(t *testing.T) {
	for _, test := range []struct {
		name   string
		budget string
		slots  int
	}{
		{"reserved-output-bytes", "8388608", 8},
		{"metadata-slots", "1", 64},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture, identity := installedSandboxExecutionFixture(t)
			t.Cleanup(startSandboxSupervisor(t, fixture,
				"--subuid-start", "200000", "--subgid-start", "300000", "--subid-count", "131072",
				"--cgroup-root", os.Getenv("SANDBOX_CGROUP_ROOT"), "--stdout-bytes", test.budget, "--stderr-bytes", test.budget))
			for _, sandboxID := range []string{"run-a", "run-b"} {
				assertSandboxCreated(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath,
					createSandboxWireRequest(t, "create-"+sandboxID, sandboxID, identity)), sandboxID)
			}
			type retained struct {
				sandboxID string
				result    sandboxExecutionResult
			}
			results := make([]retained, 0, test.slots)
			for i := range test.slots {
				sandboxID := []string{"run-a", "run-b"}[i%2]
				// Both Sandboxes use the same Execution IDs. Empty output still
				// reserves the full trusted budget before any Workload starts.
				result := executeSandboxPython(t, fixture, sandboxID, fmt.Sprintf("step-%d", i/2), "open('state', 'w').write('healthy')")
				if result.ExitCode != 0 || result.Stdout != "" || result.Stderr != "" {
					t.Fatalf("prepare empty-output snapshot: %+v", result)
				}
				results = append(results, retained{sandboxID, result})
			}
			client := sandboxsupervisor.NewClient(fixture.socketPath)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			rejected, err := client.ExecutePython(ctx, sandboxsupervisor.ExecutePythonRequest{
				RequestID: "capacity", SandboxID: "run-b", ExecutionID: "over-capacity", Source: "open('state', 'w').write('corrupted')",
			})
			if err != nil || rejected.Result != nil || rejected.Error == nil || rejected.Error.Code != "execution_result_capacity" {
				t.Fatalf("global capacity must reject the next Execution before it starts: %+v, %v", rejected, err)
			}
			assertPublicExecutionResultError(t, client, "run-b", "over-capacity", "execution_result_not_found")
			for _, saved := range results {
				if got := readSandboxExecutionResult(t, fixture, saved.sandboxID, saved.result.ExecutionID); got != saved.result {
					t.Fatalf("capacity pressure evicted or changed %s/%s", saved.sandboxID, saved.result.ExecutionID)
				}
			}
			assertSandboxDestroyed(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath,
				destroySandboxWireRequest(t, "run-a")), "run-a")
			assertPublicExecutionResultError(t, client, "run-a", "step-0", "execution_result_not_found")
			// Retry the rejected ID: admission failure must neither consume it,
			// cancel run-b, nor execute its earlier Workspace mutation.
			resumed := executeSandboxPython(t, fixture, "run-b", "over-capacity", "assert open('state').read() == 'healthy'")
			if resumed.ExitCode != 0 {
				t.Fatalf("capacity rejection disturbed the Sandbox or failed to return capacity: %+v", resumed)
			}
			assertSandboxCreated(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath,
				createSandboxWireRequest(t, "replacement", "run-c", identity)), "run-c")
			fresh := executeSandboxPython(t, fixture, "run-c", "first", "import os; assert not os.path.exists('state')")
			if fresh.ExitCode != 0 {
				t.Fatalf("Destroy did not return capacity for a new Sandbox: %+v", fresh)
			}
			for _, saved := range results {
				if saved.sandboxID == "run-b" && readSandboxExecutionResult(t, fixture, saved.sandboxID, saved.result.ExecutionID) != saved.result {
					t.Fatalf("destroying another Sandbox changed %s/%s", saved.sandboxID, saved.result.ExecutionID)
				}
			}
		})
	}
}

type resultWaitingOutcome struct {
	response sandboxsupervisor.ExecutePythonResponse
	err      error
}

func startResultWaitingExecution(ctx context.Context, client *sandboxsupervisor.Client, sandboxID string) <-chan resultWaitingOutcome {
	finished := make(chan resultWaitingOutcome, 1)
	go func() {
		response, err := client.ExecutePython(ctx, sandboxsupervisor.ExecutePythonRequest{
			RequestID: "waiting", SandboxID: sandboxID, ExecutionID: "pending",
			Source: "import signal, sys; signal.signal(signal.SIGUSR1, lambda *_: (print('resumed', flush=True), sys.exit(0))); signal.pause()",
		})
		finished <- resultWaitingOutcome{response, err}
	}()
	return finished
}

func assertPublicExecutionResultError(t *testing.T, client *sandboxsupervisor.Client, sandboxID, executionID string, code sandboxsupervisor.ErrorCode) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	response, err := client.GetExecutionResult(ctx, sandboxsupervisor.GetExecutionResultRequest{
		RequestID: "read-error", SandboxID: sandboxID, ExecutionID: executionID,
	})
	if err != nil || response.Result != nil || response.Error == nil || response.Error.Code != code {
		t.Fatalf("read %s/%s: got %+v, %v; want %s", sandboxID, executionID, response, err, code)
	}
}

func disconnectSandboxResultReader(t *testing.T, fixture sandboxSupervisorFixture, sandboxID, executionID string) {
	t.Helper()
	connection, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: fixture.socketPath, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if err := connection.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := connection.SetReadBuffer(4096); err != nil {
		t.Fatal(err)
	}
	payload := executionResultWireRequest(t, sandboxID, executionID)
	frame := make([]byte, 4, 4+len(payload))
	binary.BigEndian.PutUint32(frame, uint32(len(payload)))
	frame = append(frame, payload...)
	if _, err := connection.Write(frame); err != nil {
		t.Fatal(err)
	}
	var header [4]byte
	if _, err := io.ReadFull(connection, header[:]); err != nil {
		t.Fatal("result lookup did not respond while an Execution was running:", err)
	}
	if size := binary.BigEndian.Uint32(header[:]); size < 1<<20 {
		t.Fatalf("disconnect probe needs a published large result, got a %d-byte response", size)
	}
	// Header receipt proves the read was handled. Closing without its large
	// body tests cancellation during delivery, instead of a pre-request EOF.
	if err := connection.Close(); err != nil {
		t.Fatal(err)
	}
}
