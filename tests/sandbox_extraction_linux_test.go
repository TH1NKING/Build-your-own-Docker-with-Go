//go:build linux && sandbox_root && profilebundle_root

package workspace_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"testing"
	"time"

	"github.com/TH1NKING/Build-your-own-Docker-with-Go/internal/sandboxsupervisor"
)

func TestSandboxExecutionExtractionReturnsOnlyDeclaredFiles(t *testing.T) {
	fixture, identity := installedSandboxExecutionFixture(t)
	t.Cleanup(startSandboxSupervisor(t, fixture,
		"--subuid-start", "200000", "--subgid-start", "300000", "--subid-count", "65536",
		"--cgroup-root", os.Getenv("SANDBOX_CGROUP_ROOT")))
	assertSandboxCreated(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath,
		createSandboxWireRequest(t, "create", "run-extract", identity)), "run-extract")
	result := executeDeclaredSandboxPython(t, fixture, "run-extract", "first", `import os
os.mkdir('reports')
open('reports/answer.txt', 'wb').write(b'abc')
open('private.txt', 'w').write('keep me')
print('generated')`, []string{"reports/answer.txt"})
	if result.ExitCode != 0 || result.Stdout != "generated\n" || result.OutputError != "" || len(result.Outputs) != 1 {
		t.Fatalf("declared output missing from successful Execution: %+v", result)
	}
	file := result.Outputs[0]
	if file.Path != "reports/answer.txt" || file.Size != 3 || string(file.Content) != "abc" ||
		file.SHA256 != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Fatalf("declared bytes and metadata changed: %+v", file)
	}
	next := executeSandboxPython(t, fixture, "run-extract", "reuse", "assert open('private.txt').read() == 'keep me'; assert open('reports/answer.txt').read() == 'abc'; print('retained')")
	if next.ExitCode != 0 || next.Stdout != "retained\n" {
		t.Fatalf("extraction removed or exposed undeclared Workspace state: %+v", next)
	}
}

func TestSandboxExecutionExtractionRejectsLinksAndSpecialFiles(t *testing.T) {
	fixture, identity := installedSandboxExecutionFixture(t)
	t.Cleanup(startSandboxSupervisor(t, fixture, "--subuid-start", "200000", "--subgid-start", "300000", "--subid-count", "65536",
		"--cgroup-root", os.Getenv("SANDBOX_CGROUP_ROOT")))
	const sandboxID = "run-unsafe-outputs"
	assertSandboxCreated(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath, createSandboxWireRequest(t, "create", sandboxID, identity)), sandboxID)
	setup := executeSandboxPython(t, fixture, sandboxID, "setup", `import os, ctypes
open('safe', 'wb').write(b'abc')
os.mkdir('directory')
open('directory/value', 'w').write('nested')
os.symlink('safe', 'inside-link')
os.symlink('/proc/1/fd/4', 'control-link')
os.symlink('../input', 'parent-link')
os.symlink('directory', 'directory-link')
assert ctypes.CDLL(None).syscall(86, b'safe', b'hard-link') == 0
print('prepared')`)
	if setup.ExitCode != 0 || setup.Stdout != "prepared\n" {
		t.Fatalf("prepare adversarial files: %+v", setup)
	}
	// The trusted harness injects objects denied by the Workload's syscall
	// policy, proving extraction's own type check instead of relying on seccomp.
	// Create with IDs mapped in this tmpfs's owning User Namespace. Host UID 0
	// is intentionally unmapped and cannot create inodes there (EOVERFLOW).
	probe := exec.Command("nsenter", "--target", sandboxProcessForRoot(t, fixture, identity), "--user", "--mount", "--pid", "--net", "--root", "--wd=/",
		"--", "/opt/python/bin/python3", "-I", "-B", "-c", "import os, socket; os.chdir('/workspace/output'); os.mkfifo('fifo'); s = socket.socket(socket.AF_UNIX); s.bind('socket'); s.close()")
	if output, err := probe.CombinedOutput(); err != nil {
		t.Fatalf("inject special-file fixtures: %v\n%s", err, output)
	}
	for index, name := range []string{"inside-link", "control-link", "parent-link", "directory-link/value", "hard-link", "safe", "directory", "missing", "fifo", "socket"} {
		t.Run(name, func(t *testing.T) {
			result := executeDeclaredSandboxPython(t, fixture, sandboxID, fmt.Sprintf("bad-%d", index), "print('completed')", []string{name})
			if result.ExitCode != 0 || result.Stdout != "completed\n" || result.TerminalReason != "exited" || result.OutputError != "unsafe_output" || len(result.Outputs) != 0 {
				t.Fatalf("unsafe output must be rejected independently of Execution status: %+v", result)
			}
		})
	}
	clean := executeDeclaredSandboxPython(t, fixture, sandboxID, "clean", "open('new', 'wb').write(b'abc')", []string{"new"})
	if clean.OutputError != "" || len(clean.Outputs) != 1 {
		t.Fatalf("unsafe outputs prevented reuse: %+v", clean)
	}
}

func TestSandboxExecutionExtractionBudgetsAreAtomicAndCumulative(t *testing.T) {
	fixture, identity := installedSandboxExecutionFixture(t)
	t.Cleanup(startSandboxSupervisor(t, fixture, "--subuid-start", "200000", "--subgid-start", "300000", "--subid-count", "65536",
		"--cgroup-root", os.Getenv("SANDBOX_CGROUP_ROOT"), "--extract-file-bytes", "5", "--extract-execution-bytes", "6", "--extract-sandbox-bytes", "9", "--extract-files", "2"))
	const sandboxID = "run-file-budgets"
	assertSandboxCreated(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath, createSandboxWireRequest(t, "create", sandboxID, identity)), sandboxID)
	setup := executeSandboxPython(t, fixture, sandboxID, "setup", `import os
for name, data in [('a', b'abc'), ('b', b'abcd'), ('c', b'xyz'), ('large', b'123456'), ('empty', b'')]:
    open(name, 'wb').write(data)
with open('sparse', 'wb') as file: file.truncate(1 << 30)
print('prepared')`)
	if setup.ExitCode != 0 {
		t.Fatalf("prepare budget files: %+v", setup)
	}
	for _, test := range []struct {
		name  string
		paths []string
		code  string
		count int
	}{
		{"per-file", []string{"large"}, "output_limit", 0},
		{"sparse", []string{"sparse"}, "output_limit", 0},
		{"per-execution", []string{"a", "b"}, "output_limit", 0},
		{"partial-unsafe", []string{"a", "missing"}, "unsafe_output", 0},
		{"exact-batch", []string{"a", "c"}, "", 2},
		{"repeat-charged", []string{"a"}, "", 1},
		{"cumulative", []string{"a"}, "output_limit", 0},
		{"empty-at-limit", []string{"empty"}, "", 1},
		{"no-declaration", nil, "", 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := executeDeclaredSandboxPython(t, fixture, sandboxID, test.name, "print('completed')", test.paths)
			if result.ExitCode != 0 || result.Stdout != "completed\n" || result.OutputError != test.code || len(result.Outputs) != test.count {
				t.Fatalf("budget outcome changed Execution or published a partial batch: %+v", result)
			}
		})
	}
}

func TestSandboxExecutionExtractionSnapshotsSurviveWorkspaceChanges(t *testing.T) {
	fixture, identity := installedSandboxExecutionFixture(t)
	t.Cleanup(startSandboxSupervisor(t, fixture, "--subuid-start", "200000", "--subgid-start", "300000", "--subid-count", "65536",
		"--cgroup-root", os.Getenv("SANDBOX_CGROUP_ROOT"), "--extract-sandbox-bytes", "3"))
	const sandboxID = "run-output-snapshot"
	assertSandboxCreated(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath, createSandboxWireRequest(t, "create", sandboxID, identity)), sandboxID)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	client := sandboxsupervisor.NewClient(fixture.socketPath)
	first, err := client.ExecutePython(ctx, sandboxsupervisor.ExecutePythonRequest{RequestID: "first", SandboxID: sandboxID, ExecutionID: "first",
		Source: "open('data', 'wb').write(b'abc')", OutputPaths: []string{"data"}})
	if err != nil || first.Error != nil || first.Result == nil || len(first.Result.Outputs) != 1 {
		t.Fatalf("public client extraction: %+v, %v", first, err)
	}
	first.Result.Outputs[0].Content[0] = 'x' // Caller mutation cannot alter the retained snapshot.
	change := executeSandboxPython(t, fixture, sandboxID, "change", "open('data', 'wb').write(b'changed')")
	if change.ExitCode != 0 {
		t.Fatalf("change Workspace: %+v", change)
	}
	for range 2 {
		read, err := client.GetExecutionResult(ctx, sandboxsupervisor.GetExecutionResultRequest{RequestID: "read", SandboxID: sandboxID, ExecutionID: "first"})
		if err != nil || read.Error != nil || read.Result == nil || len(read.Result.Outputs) != 1 || string(read.Result.Outputs[0].Content) != "abc" || read.Result.Outputs[0].Size != 3 {
			t.Fatalf("snapshot reread touched mutable Workspace or was charged again: %+v, %v", read, err)
		}
	}
	assertSandboxDestroyed(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath, destroySandboxWireRequest(t, sandboxID)), sandboxID)
	assertPublicExecutionResultError(t, client, sandboxID, "first", "execution_result_not_found")
}

func TestSandboxExecutionExtractionBinaryResponseAndDescendantCleanup(t *testing.T) {
	fixture, identity := installedSandboxExecutionFixture(t)
	t.Cleanup(startSandboxSupervisor(t, fixture, "--subuid-start", "200000", "--subgid-start", "300000", "--subid-count", "65536",
		"--cgroup-root", os.Getenv("SANDBOX_CGROUP_ROOT")))
	const sandboxID = "run-binary-output"
	assertSandboxCreated(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath, createSandboxWireRequest(t, "create", sandboxID, identity)), sandboxID)
	result := executeDeclaredSandboxPython(t, fixture, sandboxID, "binary", `import os, time
with open('binary', 'wb') as file: file.write(bytes(range(256)) * 8192)
read, write = os.pipe()
if os.fork() == 0:
    os.close(read)
    handle = open('binary', 'r+b')
    os.write(write, b'R')
    while True: time.sleep(60)
os.close(write)
assert os.read(read, 1) == b'R'
print('done')`, []string{"binary"})
	pattern := make([]byte, 256)
	for i := range pattern {
		pattern[i] = byte(i)
	}
	if result.ExitCode != 0 || result.OutputError != "" || len(result.Outputs) != 1 || !bytes.Equal(result.Outputs[0].Content, bytes.Repeat(pattern, 8192)) {
		t.Fatalf("binary extraction or descendant cleanup failed: exit=%d error=%s outputs=%d", result.ExitCode, result.OutputError, len(result.Outputs))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	response, err := sandboxsupervisor.NewClient(fixture.socketPath).GetExecutionResult(ctx, sandboxsupervisor.GetExecutionResultRequest{RequestID: "read", SandboxID: sandboxID, ExecutionID: "binary"})
	if err != nil || response.Error != nil || response.Result == nil || len(response.Result.Outputs) != 1 || !reflect.DeepEqual(response.Result.Outputs[0].Content, result.Outputs[0].Content) {
		t.Fatalf("binary snapshot could not be read through public client: %+v, %v", response.Error, err)
	}
}

func TestSandboxExecutionExtractionSharesResultCapacityBeforeWorkStarts(t *testing.T) {
	fixture, identity := installedSandboxExecutionFixture(t)
	t.Cleanup(startSandboxSupervisor(t, fixture, "--subuid-start", "200000", "--subgid-start", "300000", "--subid-count", "131072",
		"--cgroup-root", os.Getenv("SANDBOX_CGROUP_ROOT"), "--stdout-bytes", "8388608", "--stderr-bytes", "8388608", "--extract-file-bytes", "1048576"))
	for _, id := range []string{"run-reservations", "run-capacity"} {
		assertSandboxCreated(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath, createSandboxWireRequest(t, "create-"+id, id, identity)), id)
	}
	for index := range 7 { // Seven retained stdio reservations consume 112 MiB.
		result := executeSandboxPython(t, fixture, "run-reservations", fmt.Sprintf("saved-%d", index), "pass")
		if result.ExitCode != 0 {
			t.Fatalf("prepare reservations: %+v", result)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	client := sandboxsupervisor.NewClient(fixture.socketPath)
	denied, err := client.ExecutePython(ctx, sandboxsupervisor.ExecutePythonRequest{RequestID: "denied", SandboxID: "run-capacity", ExecutionID: "denied",
		Source: "open('must-not-exist', 'w').write('side effect')", OutputPaths: []string{"must-not-exist"}})
	if err != nil || denied.Error == nil || denied.Error.Code != "execution_result_capacity" || denied.Result != nil {
		t.Fatalf("unreserved extracted bytes started work: %+v, %v", denied, err)
	}
	check := executeSandboxPython(t, fixture, "run-capacity", "check", "import os; assert not os.path.exists('must-not-exist'); print('not-started')")
	if check.ExitCode != 0 || check.Stdout != "not-started\n" {
		t.Fatalf("capacity denial had side effects: %+v", check)
	}
	assertSandboxDestroyed(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath, destroySandboxWireRequest(t, "run-reservations")), "run-reservations")
	result := executeDeclaredSandboxPython(t, fixture, "run-capacity", "denied", "open('data', 'wb').write(b'abc')", []string{"data"})
	if result.OutputError != "" || len(result.Outputs) != 1 {
		t.Fatalf("destroy did not release reservations or denied ID was consumed: %+v", result)
	}
}

func TestSandboxExecutionExtractionMaximumResponseFitsTransport(t *testing.T) {
	fixture, identity := installedSandboxExecutionFixture(t)
	t.Cleanup(startSandboxSupervisor(t, fixture, "--subuid-start", "200000", "--subgid-start", "300000", "--subid-count", "65536",
		"--cgroup-root", os.Getenv("SANDBOX_CGROUP_ROOT"), "--stdout-bytes", "8388608", "--stderr-bytes", "8388608", "--extract-file-bytes", "33554432"))
	const sandboxID = "run-maximum-files"
	assertSandboxCreated(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath, createSandboxWireRequest(t, "create", sandboxID, identity)), sandboxID)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	response, err := sandboxsupervisor.NewClient(fixture.socketPath).ExecutePython(ctx, sandboxsupervisor.ExecutePythonRequest{
		RequestID: "maximum", SandboxID: sandboxID, ExecutionID: "maximum", OutputPaths: []string{"data"},
		Source: `import os
os.write(1, b'\x00' * (8 << 20))
os.write(2, b'<' * (8 << 20))
with open('data', 'wb') as file: file.truncate(32 << 20)`,
	})
	if err != nil || response.Error != nil || response.Result == nil {
		t.Fatalf("maximum bounded response lost: error=%v transport=%v", response.Error, err)
	}
	result := response.Result
	if result.ExitCode != 0 || result.Truncated || result.OutputError != "" || len(result.Stdout) != 8<<20 || len(result.Stderr) != 8<<20 || len(result.Outputs) != 1 {
		t.Fatalf("maximum response changed: exit=%d reason=%s truncated=%t output-error=%s stdout=%d stderr=%d files=%d", result.ExitCode, result.TerminalReason, result.Truncated, result.OutputError, len(result.Stdout), len(result.Stderr), len(result.Outputs))
	}
	if result.Outputs[0].Size != 32<<20 || len(result.Outputs[0].Content) != 32<<20 || bytes.Count(result.Outputs[0].Content, []byte{0}) != 32<<20 {
		t.Fatal("maximum binary output changed during transport")
	}
}

type declaredExecutionResult struct {
	sandboxExecutionResult
	Outputs []struct {
		Path    string `json:"path"`
		Size    int64  `json:"size"`
		SHA256  string `json:"sha256"`
		Content []byte `json:"content"`
	} `json:"outputs"`
	OutputError string `json:"output_error"`
}

func executeDeclaredSandboxPython(t *testing.T, fixture sandboxSupervisorFixture, sandboxID, executionID, source string, paths []string) declaredExecutionResult {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"schema": "sandbox-supervisor-request/v1", "request_id": executionID, "operation": "execute_python",
		"parameters": map[string]any{"sandbox_id": sandboxID, "execution_id": executionID, "source": source, "output_paths": paths},
	})
	if err != nil {
		t.Fatal(err)
	}
	response := exchangeSandboxSupervisorMessage(t, fixture.socketPath, payload)
	var envelope struct {
		Result *declaredExecutionResult `json:"result"`
		Error  any                      `json:"error"`
	}
	if err := json.Unmarshal(response, &envelope); err != nil || envelope.Error != nil || envelope.Result == nil {
		t.Fatalf("declared Execution failed: %s, %v", response, err)
	}
	return *envelope.Result
}
