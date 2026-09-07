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

func TestSandboxExecutionDeadlinePreservesInitAndWorkspace(t *testing.T) {
	fixture, identity := installedSandboxExecutionFixture(t)
	t.Cleanup(startSandboxSupervisor(t, fixture,
		"--subuid-start", "200000", "--subgid-start", "300000", "--subid-count", "65536",
		"--cgroup-root", os.Getenv("SANDBOX_CGROUP_ROOT"), "--execution-timeout", "500ms"))
	assertSandboxCreated(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath,
		createSandboxWireRequest(t, "create", "run-deadline", identity)), "run-deadline")
	initPID := sandboxProcessForRoot(t, fixture, identity)
	initGroup := observedResourceCgroup(t, initPID)
	client := sandboxsupervisor.NewClient(fixture.socketPath)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	started := time.Now()
	response, err := client.ExecutePython(ctx, sandboxsupervisor.ExecutePythonRequest{
		RequestID: "loop", SandboxID: "run-deadline", ExecutionID: "loop",
		Source: "open('state', 'w').write('preserved'); print('started', flush=True)\nwhile True: pass",
	})
	elapsed := time.Since(started)
	if err != nil || response.Error != nil || response.Result == nil {
		t.Fatalf("deadline must return an Execution Result while the client remains connected: %+v, %v", response, err)
	}
	if result := response.Result; result.TerminalReason != "timed_out" || result.ExitCode != 137 || result.Stdout != "started\n" {
		t.Fatalf("deadline lost its reason or captured output: %+v", result)
	}
	if elapsed < 500*time.Millisecond || elapsed > 5*time.Second || ctx.Err() != nil {
		t.Fatalf("trusted 500ms deadline was not enforced independently of the 10s client context: %s, %v", elapsed, ctx.Err())
	}
	children, err := os.ReadDir(filepath.Dir(initGroup))
	if err != nil {
		t.Fatal(err)
	}
	for _, child := range children {
		if child.IsDir() && child.Name() != filepath.Base(initGroup) {
			t.Fatalf("deadline result acknowledged before removing Execution cgroup %s", child.Name())
		}
	}
	clean := executeSandboxPython(t, fixture, "run-deadline", "clean", `import os
assert open('state').read() == 'preserved'
assert set(n for n in os.listdir('/proc') if n.isdigit()) == {'1', str(os.getpid())}
print('clean')`)
	if clean.ExitCode != 0 || clean.TerminalReason != "exited" || clean.Stdout != "clean\n" {
		t.Fatalf("timeout poisoned sequential reuse: %+v", clean)
	}
	if pid := sandboxProcessForRoot(t, fixture, identity); pid != initPID {
		t.Fatalf("timeout replaced Init %s with %s", initPID, pid)
	}
	ordinary := executeSandboxPython(t, fixture, "run-deadline", "exit-137", "import sys; sys.exit(137)")
	if ordinary.ExitCode != 137 || ordinary.TerminalReason != "exited" {
		t.Fatalf("ordinary exit code was mistaken for deadline termination: %+v", ordinary)
	}
}

func TestSandboxExecutionDeadlineKillsDetachedOutputWritersBeforeRepeatedReuse(t *testing.T) {
	fixture, identity := installedSandboxExecutionFixture(t)
	t.Cleanup(startSandboxSupervisor(t, fixture,
		"--subuid-start", "200000", "--subgid-start", "300000", "--subid-count", "65536",
		"--cgroup-root", os.Getenv("SANDBOX_CGROUP_ROOT"), "--execution-timeout", "1s"))
	assertSandboxCreated(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath,
		createSandboxWireRequest(t, "create", "run-writers", identity)), "run-writers")
	for range 3 {
		result := executeSandboxPython(t, fixture, "run-writers", "flood", `import os, signal, time
r, w = os.pipe()
if os.fork() == 0:
    os.close(r)
    os.setsid()
    if os.fork() != 0: os._exit(0)
    signal.signal(signal.SIGTERM, signal.SIG_IGN)
    os.write(w, b'R')
    os.close(w)
    while True:
        os.write(1, b'x' * 8192)
        os.write(2, b'y' * 8192)
os.close(w)
assert os.read(r, 1) == b'R'
os.close(r)
while True: time.sleep(60)`)
		if result.TerminalReason != "timed_out" || result.ExitCode != 137 || !result.Truncated ||
			len(result.Stdout) != 4096 || len(result.Stderr) != 4096 {
			t.Fatalf("continuous detached writers escaped the deadline or lost output: %+v", result)
		}
		clean := executeSandboxPython(t, fixture, "run-writers", "clean", `import os
assert set(n for n in os.listdir('/proc') if n.isdigit()) == {'1', str(os.getpid())}
print('no-descendants-or-zombies')`)
		if clean.TerminalReason != "exited" || clean.ExitCode != 0 || clean.Stdout != "no-descendants-or-zombies\n" {
			t.Fatalf("old timeout, reader, or descendants disturbed a later Execution: %+v", clean)
		}
	}
}

func TestSandboxExecutionDeadlineDefaultAndLongerTrustedBudget(t *testing.T) {
	for _, test := range []struct {
		name     string
		flags    []string
		source   string
		minimum  time.Duration
		reason   sandboxsupervisor.ExecutionTerminalReason
		exitCode int
	}{
		{"default-60s", nil, "print('ready', flush=True); import time; time.sleep(90)", 60 * time.Second, "timed_out", 137},
		{"beyond-old-transport-guard", []string{"--execution-timeout", "75s"}, "import time; time.sleep(66); print('ready')", 66 * time.Second, "exited", 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture, identity := installedSandboxExecutionFixture(t)
			flags := []string{"--subuid-start", "200000", "--subgid-start", "300000", "--subid-count", "65536", "--cgroup-root", os.Getenv("SANDBOX_CGROUP_ROOT")}
			t.Cleanup(startSandboxSupervisor(t, fixture, append(flags, test.flags...)...))
			assertSandboxCreated(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath,
				createSandboxWireRequest(t, "create", "run-long", identity)), "run-long")
			ctx, cancel := context.WithTimeout(context.Background(), 85*time.Second)
			defer cancel()
			started := time.Now()
			response, err := sandboxsupervisor.NewClient(fixture.socketPath).ExecutePython(ctx, sandboxsupervisor.ExecutePythonRequest{
				RequestID: "long", SandboxID: "run-long", ExecutionID: "long", Source: test.source,
			})
			elapsed := time.Since(started)
			if err != nil || response.Error != nil || response.Result == nil {
				t.Fatalf("configured deadline was confused with transport failure: %+v, %v", response, err)
			}
			if result := response.Result; result.TerminalReason != test.reason || result.ExitCode != test.exitCode || result.Stdout != "ready\n" {
				t.Fatalf("unexpected long Execution outcome: %+v", result)
			}
			if elapsed < test.minimum || elapsed > test.minimum+10*time.Second {
				t.Fatalf("Execution lasted %s, want at least %s with bounded cleanup", elapsed, test.minimum)
			}
			clean := executeSandboxPython(t, fixture, "run-long", "clean", "print('still-ready')")
			if clean.ExitCode != 0 || clean.Stdout != "still-ready\n" {
				t.Fatalf("response delivery invalidated the long-lived Sandbox: %+v", clean)
			}
		})
	}
}

func TestSandboxExecutionDeadlineInvalidatesUnresponsiveInit(t *testing.T) {
	fixture, identity := installedSandboxExecutionFixture(t)
	t.Cleanup(startSandboxSupervisor(t, fixture,
		"--subuid-start", "200000", "--subgid-start", "300000", "--subid-count", "65536",
		"--cgroup-root", os.Getenv("SANDBOX_CGROUP_ROOT"), "--execution-timeout", "1s"))
	assertSandboxCreated(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath,
		createSandboxWireRequest(t, "create", "run-unresponsive", identity)), "run-unresponsive")
	initPID := sandboxProcessForRoot(t, fixture, identity)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client := sandboxsupervisor.NewClient(fixture.socketPath)
	finished := make(chan sandboxsupervisor.ExecutePythonResponse, 1)
	go func() {
		response, err := client.ExecutePython(ctx, sandboxsupervisor.ExecutePythonRequest{
			RequestID: "wait", SandboxID: "run-unresponsive", ExecutionID: "wait", Source: detachedCancellationWorkload,
		})
		if err != nil {
			t.Errorf("unresponsive Init response: %v", err)
		}
		finished <- response
	}()
	workloadPID := waitForSandboxSignalHandler(t, initPID)
	workloadGroup := observedResourceCgroup(t, strconv.Itoa(workloadPID))
	pid, err := strconv.Atoi(initPID)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(pid, syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	response := <-finished
	if response.Error != nil || response.Result == nil || response.Result.TerminalReason != "timed_out" ||
		response.Result.ExitCode != -1 || !response.Result.Truncated || response.Result.Stdout != "" || response.Result.Stderr != "" {
		t.Fatalf("lost Init acknowledgement invented complete output or lost timeout evidence: %+v", response)
	}
	assertSandboxSupervisorErrorCode(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath,
		createSandboxWireRequest(t, "stale", "run-unresponsive", identity)), "sandbox_exists")
	// Destroy waits for any in-progress teardown before asserting host state.
	assertSandboxDestroyed(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath,
		destroySandboxWireRequest(t, "run-unresponsive")), "run-unresponsive")
	if _, err := os.Stat(workloadGroup); !os.IsNotExist(err) {
		t.Fatalf("unresponsive Init left the Execution cgroup behind: %v", err)
	}
	assertNoResourceCgroups(t)
	if pids := sandboxProcessesForSubordinateMapping(t, 200000, 65536); len(pids) != 0 {
		t.Fatalf("unresponsive Init retained descendants: %v", pids)
	}
	assertSandboxCreated(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath,
		createSandboxWireRequest(t, "replacement", "run-replacement", identity)), "run-replacement")
	clean := executeSandboxPython(t, fixture, "run-replacement", "clean", "print('healthy')")
	if clean.ExitCode != 0 || clean.Stdout != "healthy\n" {
		t.Fatalf("unresponsive Init poisoned a replacement Sandbox: %+v", clean)
	}
}
