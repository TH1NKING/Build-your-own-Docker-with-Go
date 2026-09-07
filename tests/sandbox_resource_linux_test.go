//go:build linux && sandbox_root && profilebundle_root

package workspace_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TH1NKING/Build-your-own-Docker-with-Go/internal/sandboxsupervisor"
)

func TestSandboxExecutionResourceMemoryExhaustion(t *testing.T) {
	fixture, identity := installedSandboxExecutionFixture(t)
	t.Cleanup(startSandboxSupervisor(t, fixture,
		"--subuid-start", "200000", "--subgid-start", "300000", "--subid-count", "65536",
		"--cgroup-root", os.Getenv("SANDBOX_CGROUP_ROOT"), "--memory-bytes", "67108864"))
	assertSandboxCreated(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath,
		createSandboxWireRequest(t, "create", "run-memory", identity)), "run-memory")
	result := executeSandboxPython(t, fixture, "run-memory", "allocate",
		"chunks = [bytearray(8 * 1024 * 1024) for _ in range(24)]; print('allocation-escaped-budget')")
	if result.TerminalReason != "memory_limit" || result.Stdout != "" || result.ResourceUsage.OOMEvents == 0 {
		t.Fatalf("memory exhaustion was not identified from kernel evidence: %+v", result)
	}
	client := sandboxsupervisor.NewClient(fixture.socketPath)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	response, err := client.DestroySandbox(ctx, sandboxsupervisor.DestroySandboxRequest{RequestID: "destroy", SandboxID: "run-memory"})
	if err != nil || response.Error != nil || response.Result == nil {
		t.Fatalf("destroy exhausted Sandbox: %+v, %v", response, err)
	}
	assertNoResourceCgroups(t)
	assertSandboxCreated(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath,
		createSandboxWireRequest(t, "create-clean", "run-clean", identity)), "run-clean")
	clean := executeSandboxPython(t, fixture, "run-clean", "clean", "print('healthy')")
	if clean.ExitCode != 0 || clean.Stdout != "healthy\n" || clean.TerminalReason != "exited" || clean.ResourceUsage.OOMEvents != 0 {
		t.Fatalf("resource exhaustion poisoned a later Sandbox: %+v", clean)
	}
}

func assertNoResourceCgroups(t *testing.T) {
	t.Helper()
	groups, err := os.ReadDir(os.Getenv("SANDBOX_CGROUP_ROOT"))
	if err != nil {
		t.Fatal(err)
	}
	for _, group := range groups {
		if group.IsDir() {
			t.Fatalf("Sandbox teardown retained cgroup %s", group.Name())
		}
	}
}

func TestSandboxExecutionResourceCPUThrottlesWithoutFailing(t *testing.T) {
	fixture, identity := installedSandboxExecutionFixture(t)
	t.Cleanup(startSandboxSupervisor(t, fixture,
		"--subuid-start", "200000", "--subgid-start", "300000", "--subid-count", "65536",
		"--cgroup-root", os.Getenv("SANDBOX_CGROUP_ROOT"), "--cpu-millis", "250"))
	assertSandboxCreated(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath,
		createSandboxWireRequest(t, "create", "run-cpu", identity)), "run-cpu")
	result := executeSandboxPython(t, fixture, "run-cpu", "compute", `import os, time
children = []
for _ in range(2):
    pid = os.fork()
    if pid == 0:
        end = time.process_time() + 0.2
        while time.process_time() < end: pass
        os._exit(0)
    children.append(pid)
for pid in children:
    assert os.waitpid(pid, 0)[1] == 0
print('quota-work-complete')`)
	if result.ExitCode != 0 || result.TerminalReason != "exited" || result.Stdout != "quota-work-complete\n" ||
		result.ResourceUsage.CPUUsec < 300000 || result.ResourceUsage.CPUThrottledPeriods == 0 || result.ResourceUsage.CPUThrottledUsec == 0 {
		t.Fatalf("CPU quota must throttle aggregate work without making it fail: %+v", result)
	}
}

func TestSandboxExecutionResourcePIDExhaustionKillsCaughtFailureAndDescendants(t *testing.T) {
	fixture, identity := installedSandboxExecutionFixture(t)
	t.Cleanup(startSandboxSupervisor(t, fixture,
		"--subuid-start", "200000", "--subgid-start", "300000", "--subid-count", "65536",
		"--cgroup-root", os.Getenv("SANDBOX_CGROUP_ROOT"), "--pids-limit", "32"))
	assertSandboxCreated(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath,
		createSandboxWireRequest(t, "create", "run-pids", identity)), "run-pids")
	result := executeSandboxPython(t, fixture, "run-pids", "fork", `import errno, os, time
for _ in range(64):
    try:
        pid = os.fork()
    except OSError as error:
        assert error.errno == errno.EAGAIN
        while True: time.sleep(1)
    if pid == 0:
        os.setsid()
        time.sleep(2)
        os._exit(0)
print('fork-escaped-budget')`)
	if result.TerminalReason != "pids_limit" || result.ExitCode != 137 || result.ResourceUsage.PIDLimitEvents == 0 || result.ResourceUsage.OOMEvents != 0 {
		t.Fatalf("caught fork refusal must terminate the entire Execution: %+v", result)
	}
	clean := executeSandboxPython(t, fixture, "run-pids", "clean", `import os
assert set(n for n in os.listdir('/proc') if n.isdigit()) == {'1', str(os.getpid())}
print('clean')`)
	if clean.ExitCode != 0 || clean.Stdout != "clean\n" || clean.TerminalReason != "exited" || clean.ResourceUsage.PIDLimitEvents != 0 {
		t.Fatalf("PID exhaustion left descendants or contaminated a later result: %+v", clean)
	}
}

func TestSandboxExecutionResourceMemoryIncludesRetainedWorkspace(t *testing.T) {
	fixture, identity := installedSandboxExecutionFixture(t)
	t.Cleanup(startSandboxSupervisor(t, fixture,
		"--subuid-start", "200000", "--subgid-start", "300000", "--subid-count", "65536",
		"--cgroup-root", os.Getenv("SANDBOX_CGROUP_ROOT"), "--memory-bytes", "67108864"))
	assertSandboxCreated(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath,
		createSandboxWireRequest(t, "create", "run-retained", identity)), "run-retained")
	const allocation = "data = bytearray(40 * 1024 * 1024); print('fits')"
	first := executeSandboxPython(t, fixture, "run-retained", "baseline", allocation)
	if first.ExitCode != 0 || first.Stdout != "fits\n" {
		t.Fatalf("baseline allocation did not fit: %+v", first)
	}
	retained := executeSandboxPython(t, fixture, "run-retained", "retain", `with open('retained', 'wb') as file:
    block = b'x' * (1024 * 1024)
    for _ in range(32): file.write(block)`)
	if retained.ExitCode != 0 || retained.TerminalReason != "exited" {
		t.Fatalf("retain Workspace state: %+v", retained)
	}
	limited := executeSandboxPython(t, fixture, "run-retained", "allocate", allocation)
	if limited.TerminalReason != "memory_limit" || limited.ResourceUsage.OOMEvents == 0 {
		t.Fatalf("prior Execution tmpfs charges escaped the Sandbox budget: %+v", limited)
	}
	released := executeSandboxPython(t, fixture, "run-retained", "release", "import os; os.unlink('retained')")
	if released.ExitCode != 0 || released.TerminalReason != "exited" || released.ResourceUsage.OOMEvents != 0 {
		t.Fatalf("previous OOM was attributed to a later clean Execution: %+v", released)
	}
	last := executeSandboxPython(t, fixture, "run-retained", "after-release", allocation)
	if last.ExitCode != 0 || last.Stdout != "fits\n" || last.TerminalReason != "exited" {
		t.Fatalf("deleting retained Workspace did not release its memory charge: %+v", last)
	}
}

func TestSandboxExecutionResourceDefaultsAndTrustedOverrides(t *testing.T) {
	for _, test := range []struct {
		name  string
		flags []string
		want  map[string]string
	}{
		{"defaults", nil, map[string]string{"cpu.max": "200000 100000", "memory.max": "1073741824", "memory.swap.max": "0", "pids.max": "64"}},
		{"overrides", []string{"--cpu-millis", "500", "--memory-bytes", "134217728", "--swap-bytes", "16777216", "--pids-limit", "32"},
			map[string]string{"cpu.max": "50000 100000", "memory.max": "134217728", "memory.swap.max": "16777216", "pids.max": "32"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture, identity := installedSandboxExecutionFixture(t)
			flags := []string{"--subuid-start", "200000", "--subgid-start", "300000", "--subid-count", "65536", "--cgroup-root", os.Getenv("SANDBOX_CGROUP_ROOT")}
			t.Cleanup(startSandboxSupervisor(t, fixture, append(flags, test.flags...)...))
			assertSandboxCreated(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath,
				createSandboxWireRequest(t, "create", "run-policy", identity)), "run-policy")
			initPID := sandboxProcessForRoot(t, fixture, identity)
			budget := filepath.Dir(observedResourceCgroup(t, initPID))
			for file, want := range test.want {
				contents, err := os.ReadFile(filepath.Join(budget, file))
				if err != nil || strings.TrimSpace(string(contents)) != want {
					t.Fatalf("kernel %s = %q (%v), want %q", file, contents, err, want)
				}
			}
			result := executeSandboxPython(t, fixture, "run-policy", "observe", `assert open('/proc/self/oom_score_adj').read().strip() == '0'
assert open('/proc/1/oom_score_adj').read().strip() == '-1000'
print('workload-not-oom-exempt')`)
			if result.ExitCode != 0 || result.Stdout != "workload-not-oom-exempt\n" {
				t.Fatalf("Workload inherited trusted Init OOM protection: %+v", result)
			}
			children, err := os.ReadDir(budget)
			if err != nil {
				t.Fatal(err)
			}
			groups := 0
			for _, child := range children {
				if child.IsDir() {
					groups++
				}
			}
			if groups != 1 {
				t.Fatalf("completed Execution left %d child cgroups; only Init should remain", groups)
			}
		})
	}
}

// Observe the kernel's membership and mount tables, never generated directory
// names or private Supervisor helpers. This also supports a delegated subtree.
func observedResourceCgroup(t *testing.T, pid string) string {
	t.Helper()
	contents, err := os.ReadFile(filepath.Join("/proc", pid, "cgroup"))
	if err != nil {
		t.Fatal(err)
	}
	var membership string
	for _, line := range strings.Split(string(contents), "\n") {
		if strings.HasPrefix(line, "0::/") {
			membership = strings.TrimPrefix(line, "0::")
		}
	}
	mounts, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(mounts), "\n") {
		parts := strings.Split(line, " - cgroup2 ")
		if len(parts) != 2 {
			continue
		}
		fields := strings.Fields(parts[0])
		if len(fields) < 5 {
			continue
		}
		relative, err := filepath.Rel(fields[3], membership)
		if err != nil || !filepath.IsLocal(relative) {
			continue
		}
		candidate := filepath.Join(fields[4], relative)
		below, err := filepath.Rel(os.Getenv("SANDBOX_CGROUP_ROOT"), candidate)
		if err == nil && filepath.IsLocal(below) {
			return candidate
		}
	}
	t.Fatalf("no test cgroup mount contains process %s membership %q", pid, membership)
	return ""
}

func TestSandboxExecutionResourceRequiresAllControllersBeforeCreation(t *testing.T) {
	fixture, identity := installedSandboxExecutionFixture(t)
	disabled := filepath.Join(os.Getenv("SANDBOX_CGROUP_ROOT"), "disabled")
	leaf := filepath.Join(disabled, "no-controllers")
	if err := os.Mkdir(disabled, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Remove(disabled); err != nil {
			t.Error(err)
		}
	})
	if err := os.Mkdir(leaf, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Remove(leaf); err != nil {
			t.Error(err)
		}
	})
	t.Cleanup(startSandboxSupervisor(t, fixture,
		"--subuid-start", "200000", "--subgid-start", "300000", "--subid-count", "65536", "--cgroup-root", leaf))
	assertSandboxSupervisorErrorCode(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath,
		createSandboxWireRequest(t, "create", "run-unavailable", identity)), "operation_unavailable")
	entries, err := os.ReadDir(fixture.sandboxRoot)
	if err != nil || len(entries) != 0 {
		t.Fatalf("unavailable controllers leaked a Sandbox reservation: %v, %v", entries, err)
	}
}

func TestSandboxExecutionResourceCleanupRetainsIdentityUntilTreeIsEmpty(t *testing.T) {
	fixture, identity := installedSandboxExecutionFixture(t)
	stop := startSandboxSupervisor(t, fixture,
		"--subuid-start", "200000", "--subgid-start", "300000", "--subid-count", "65536",
		"--cgroup-root", os.Getenv("SANDBOX_CGROUP_ROOT"))
	t.Cleanup(stop)
	assertSandboxCreated(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath,
		createSandboxWireRequest(t, "create", "run-replaced", identity)), "run-replaced")
	budget := filepath.Dir(observedResourceCgroup(t, sandboxProcessForRoot(t, fixture, identity)))
	blocker := filepath.Join(budget, "operator-held")
	if err := os.Mkdir(blocker, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Remove(blocker); err != nil && !os.IsNotExist(err) {
			t.Error(err)
		}
	})
	client := sandboxsupervisor.NewClient(fixture.socketPath)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	response, err := client.DestroySandbox(ctx, sandboxsupervisor.DestroySandboxRequest{RequestID: "destroy", SandboxID: "run-replaced"})
	if err != nil || response.Error == nil || response.Error.Code != "cleanup_failed" {
		t.Fatalf("obstructed cgroup cleanup must retain ownership: %+v, %v", response, err)
	}
	if _, err := os.Stat(blocker); err != nil {
		t.Fatalf("cleanup removed a foreign cgroup: %v", err)
	}
	assertSandboxSupervisorErrorCode(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath,
		createSandboxWireRequest(t, "occupied", "run-other", identity)), "identity_range_exhausted")
	if err := os.Remove(blocker); err != nil {
		t.Fatal(err)
	}
	response, err = client.DestroySandbox(ctx, sandboxsupervisor.DestroySandboxRequest{RequestID: "retry", SandboxID: "run-replaced"})
	if err != nil || response.Error != nil || response.Result == nil {
		t.Fatalf("repaired cgroup cleanup did not complete: %+v, %v", response, err)
	}
	assertNoResourceCgroups(t)
}
