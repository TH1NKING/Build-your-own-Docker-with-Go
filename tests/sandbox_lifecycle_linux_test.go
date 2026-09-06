//go:build linux && sandbox_root && profilebundle_root

package workspace_test

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/TH1NKING/Build-your-own-Docker-with-Go/internal/sandboxsupervisor"
)

func TestSandboxLifecycleShutdownClosesIdleClients(t *testing.T) {
	fixture, identity := installedSandboxExecutionFixture(t)
	stop := startSandboxSupervisor(t, fixture, "--subuid-start", "200000", "--subgid-start", "300000", "--subid-count", "65536")
	assertSandboxCreated(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath,
		createSandboxWireRequest(t, "create", "run-shutdown", identity)), "run-shutdown")
	connection, err := net.Dial("unix", fixture.socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	// A partial frame leaves a real client waiting without a complete request.
	if _, err := connection.Write([]byte{0}); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	stop()
	if time.Since(started) > 2*time.Second {
		t.Fatal("shutdown waited for an idle client's framing timeout")
	}
	if pids := sandboxProcessesForSubordinateMapping(t, 200000, 65536); len(pids) != 0 {
		t.Fatalf("shutdown retained Sandbox processes: %v", pids)
	}
	if entries, err := os.ReadDir(fixture.sandboxRoot); err != nil || len(entries) != 0 {
		t.Fatalf("shutdown retained temporary directories: %v %v", entries, err)
	}
}

func TestSandboxLifecycleConcurrentDestroyTerminatesActiveExecution(t *testing.T) {
	fixture, identity := installedSandboxExecutionFixture(t)
	t.Cleanup(startSandboxSupervisor(t, fixture, "--subuid-start", "200000", "--subgid-start", "300000", "--subid-count", "65536", "--cgroup-root", os.Getenv("SANDBOX_CGROUP_ROOT")))
	assertSandboxCreated(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath,
		createSandboxWireRequest(t, "create", "run-active", identity)), "run-active")
	initPID := sandboxProcessForRoot(t, fixture, identity)
	client := sandboxsupervisor.NewClient(fixture.socketPath)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	finished := make(chan sandboxsupervisor.ExecutePythonResponse, 1)
	go func() {
		response, _ := client.ExecutePython(ctx, sandboxsupervisor.ExecutePythonRequest{
			RequestID: "active", SandboxID: "run-active", ExecutionID: "active",
			Source: "import signal; signal.signal(signal.SIGUSR1, lambda *_: None); signal.pause()",
		})
		finished <- response
	}()
	workloadPID := waitForSandboxSignalHandler(t, initPID)
	// A rejected request closes its own connection without owning cancellation
	// rights over the already running Execution or its Workspace.
	busy, err := client.ExecutePython(ctx, sandboxsupervisor.ExecutePythonRequest{RequestID: "busy", SandboxID: "run-active", ExecutionID: "busy", Source: "print(42)"})
	if err != nil || busy.Error == nil || busy.Error.Code != "sandbox_busy" {
		t.Fatalf("overlap: %+v %v", busy, err)
	}
	assertSandboxSupervisorErrorCode(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath, createSandboxWireRequest(t, "duplicate", "run-active", identity)), "sandbox_exists")
	if _, err := os.Stat(filepath.Join("/proc", strconv.Itoa(workloadPID))); err != nil {
		t.Fatalf("rejected request killed another request's Workload: %v", err)
	}
	var requests sync.WaitGroup
	for range 4 {
		requests.Go(func() {
			response, err := client.DestroySandbox(ctx, sandboxsupervisor.DestroySandboxRequest{RequestID: "destroy", SandboxID: "run-active"})
			if err != nil || response.Error != nil || response.Result == nil {
				t.Errorf("concurrent destruction: %+v %v", response, err)
			}
		})
	}
	requests.Wait()
	failed := <-finished
	if failed.Error == nil || failed.Error.Code != "execution_failed" || failed.Result != nil {
		t.Fatalf("destroy returned successful Execution: %+v", failed)
	}
	if pids := sandboxProcessesForSubordinateMapping(t, 200000, 65536); len(pids) != 0 {
		t.Fatalf("destroy acknowledged surviving Workload: %v", pids)
	}
	assertSandboxSupervisorErrorCode(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath, createSandboxWireRequest(t, "reuse", "run-active", identity)), "sandbox_exists")
}

func TestSandboxLifecycleDisconnectDestroysActiveSandbox(t *testing.T) {
	fixture, identity := installedSandboxExecutionFixture(t)
	t.Cleanup(startSandboxSupervisor(t, fixture, "--subuid-start", "200000", "--subgid-start", "300000", "--subid-count", "65536", "--cgroup-root", os.Getenv("SANDBOX_CGROUP_ROOT")))
	assertSandboxCreated(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath,
		createSandboxWireRequest(t, "create-abandoned", "run-abandoned", identity)), "run-abandoned")
	initPID := sandboxProcessForRoot(t, fixture, identity)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		_, err := sandboxsupervisor.NewClient(fixture.socketPath).ExecutePython(ctx, sandboxsupervisor.ExecutePythonRequest{
			RequestID: "active", SandboxID: "run-abandoned", ExecutionID: "active",
			Source: "import signal; signal.signal(signal.SIGUSR1, lambda *_: None); signal.pause()",
		})
		finished <- err
	}()
	workloadPID := waitForSandboxSignalHandler(t, initPID)
	cancel()
	if err := <-finished; err == nil {
		t.Fatal("cancelled client returned successful transport")
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		_, initErr := os.Stat(filepath.Join("/proc", initPID))
		_, workloadErr := os.Stat(filepath.Join("/proc", strconv.Itoa(workloadPID)))
		_, directoryErr := os.Stat(filepath.Join(fixture.sandboxRoot, "run-abandoned"))
		if os.IsNotExist(initErr) && os.IsNotExist(workloadErr) && os.IsNotExist(directoryErr) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("client disconnect retained Sandbox resources")
		}
		time.Sleep(10 * time.Millisecond)
	}
	assertSandboxSupervisorErrorCode(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath,
		createSandboxWireRequest(t, "stale", "run-abandoned", identity)), "sandbox_exists")
	assertSandboxCreated(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath,
		createSandboxWireRequest(t, "replacement", "run-replacement", identity)), "run-replacement")
}

func TestSandboxLifecycleDestroyReleasesResources(t *testing.T) {
	fixture, identity := installedSandboxExecutionFixture(t)
	t.Cleanup(startSandboxSupervisor(t, fixture, "--subuid-start", "200000", "--subgid-start", "300000", "--subid-count", "65536", "--cgroup-root", os.Getenv("SANDBOX_CGROUP_ROOT")))
	for _, id := range []string{"run-first", "run-second"} {
		assertSandboxCreated(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath,
			createSandboxWireRequest(t, "create-"+id, id, identity)), id)
		pid := sandboxProcessForRoot(t, fixture, identity)
		namespaces := map[string]bool{}
		for _, name := range []string{"user", "mnt", "pid", "net"} {
			target, err := os.Readlink(filepath.Join("/proc", pid, "ns", name))
			if err != nil {
				t.Fatal(err)
			}
			namespaces[target] = true
		}
		result := executeSandboxPython(t, fixture, id, "save", "open('state', 'w').write('42')")
		if result.ExitCode != 0 {
			t.Fatalf("write Workspace: %+v", result)
		}
		assertSandboxDestroyed(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath, destroySandboxWireRequest(t, id)), id)
		if _, err := os.Stat(filepath.Join("/proc", pid)); !os.IsNotExist(err) {
			t.Fatalf("destroy acknowledged before Init was reaped: %v", err)
		}
		if entries, err := os.ReadDir(fixture.sandboxRoot); err != nil || len(entries) != 0 {
			t.Fatalf("destroy retained temporary Sandbox directories: %v, %v", entries, err)
		}
		if pids := sandboxProcessesForRoot(t, fixture, identity); len(pids) != 0 {
			t.Fatalf("destroy retained processes rooted in the Sandbox: %v", pids)
		}
		assertNoSandboxNamespaceHandles(t, namespaces)
		assertSandboxDestroyed(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath, destroySandboxWireRequest(t, id)), id)
	}
}

func assertNoSandboxNamespaceHandles(t *testing.T, namespaces map[string]bool) {
	t.Helper()
	processes, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatal(err)
	}
	for _, process := range processes {
		if _, err := strconv.Atoi(process.Name()); err != nil {
			continue
		}
		directory := filepath.Join("/proc", process.Name(), "fd")
		fds, err := os.ReadDir(directory)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		for _, fd := range fds {
			target, _ := os.Readlink(filepath.Join(directory, fd.Name()))
			if namespaces[target] {
				t.Fatalf("process %s retains Sandbox namespace %s at FD %s", process.Name(), target, fd.Name())
			}
		}
	}
}

func TestSandboxLifecycleCleanupFailurePreservesUnownedState(t *testing.T) {
	for _, mutation := range []string{"unexpected-file", "replaced-directory"} {
		t.Run(mutation, func(t *testing.T) {
			fixture, identity := installedSandboxExecutionFixture(t)
			t.Cleanup(startSandboxSupervisor(t, fixture, "--subuid-start", "200000", "--subgid-start", "300000", "--subid-count", "65536"))
			id := "run-cleanup"
			assertSandboxCreated(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath,
				createSandboxWireRequest(t, "create", id, identity)), id)
			directory := filepath.Join(fixture.sandboxRoot, id)
			if mutation == "replaced-directory" {
				if err := os.Rename(directory, directory+"-original"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(directory, 0o755); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.WriteFile(filepath.Join(directory, "sentinel"), []byte("unowned"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			for attempt := 0; attempt < 2; attempt++ {
				assertSandboxSupervisorErrorCode(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath,
					destroySandboxWireRequest(t, id)), "cleanup_failed")
				if _, err := os.Stat(directory); err != nil {
					t.Fatalf("cleanup removed unowned state: %v", err)
				}
			}
			assertSandboxSupervisorErrorCode(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath,
				createSandboxWireRequest(t, "reuse", id, identity)), "sandbox_exists")
			if mutation == "replaced-directory" {
				if err := os.Remove(directory); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(directory+"-original", directory); err != nil {
					t.Fatal(err)
				}
			} else {
				if contents, err := os.ReadFile(filepath.Join(directory, "sentinel")); err != nil || string(contents) != "unowned" {
					t.Fatalf("changed sentinel: %q %v", contents, err)
				}
				if err := os.Remove(filepath.Join(directory, "sentinel")); err != nil {
					t.Fatal(err)
				}
			}
			assertSandboxDestroyed(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath, destroySandboxWireRequest(t, id)), id)
			if _, err := os.Stat(directory); !os.IsNotExist(err) {
				t.Fatalf("cleanup did not retry after repair: %v", err)
			}
		})
	}
}

func destroySandboxWireRequest(t *testing.T, id string) []byte {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"schema": "sandbox-supervisor-request/v1", "request_id": "destroy-" + id,
		"operation": "destroy_sandbox", "parameters": map[string]string{"sandbox_id": id},
	})
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func assertSandboxDestroyed(t *testing.T, payload []byte, id string) {
	t.Helper()
	var response struct {
		Result *struct {
			SandboxID string `json:"sandbox_id"`
		} `json:"result"`
		Error any `json:"error"`
	}
	if err := json.Unmarshal(payload, &response); err != nil || response.Result == nil || response.Result.SandboxID != id || response.Error != nil {
		t.Fatalf("destroy returned %s, want confirmed Sandbox %s: %v", payload, id, err)
	}
}
