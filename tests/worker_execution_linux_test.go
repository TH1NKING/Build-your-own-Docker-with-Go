//go:build linux && sandbox_root && profilebundle_root

package workspace_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/TH1NKING/Build-your-own-Docker-with-Go/internal/executionqueue"
	"github.com/TH1NKING/Build-your-own-Docker-with-Go/internal/sandboxsupervisor"
	"github.com/TH1NKING/Build-your-own-Docker-with-Go/internal/worker"
	"github.com/TH1NKING/Build-your-own-Docker-with-Go/internal/workerapi"
)

func TestWorkerExecutionRunsPythonAndReportsItsBoundedResult(t *testing.T) {
	fixture, identity := installedSandboxExecutionFixture(t)
	t.Cleanup(startSandboxSupervisor(t, fixture,
		"--subuid-start", "200000", "--subgid-start", "300000", "--subid-count", "65536",
		"--cgroup-root", os.Getenv("SANDBOX_CGROUP_ROOT"), "--stdout-bytes", "32"))
	api, readResult := workerExecutionControlPlane(t, `import os
assert os.getuid() == 1000
open('counter', 'x').write('executed once')
os.write(1, b'x' * 100)
print('detail', file=__import__('sys').stderr)`, nil)
	node, err := worker.New(worker.Config{API: api, SupervisorSocket: fixture.socketPath, ProfileIdentity: identity})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	claimed, err := node.RunOnce(ctx)
	if err != nil || !claimed {
		t.Fatalf("Worker execution: claimed=%v, error=%v", claimed, err)
	}
	result := readResult()
	if result.ExitCode != 0 || result.Stdout != "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" || !result.StdoutTruncated || result.Stderr != "detail\n" || result.TerminalReason != "exited" {
		t.Fatalf("Control Plane did not retain the bounded Execution Result: %+v", result)
	}
	assertNoResourceCgroups(t)
}

func TestWorkerExecutionRetriesLostAcknowledgementWithoutRunningPythonAgain(t *testing.T) {
	fixture, identity := installedSandboxExecutionFixture(t)
	t.Cleanup(startSandboxSupervisor(t, fixture,
		"--subuid-start", "200000", "--subgid-start", "300000", "--subid-count", "65536",
		"--cgroup-root", os.Getenv("SANDBOX_CGROUP_ROOT")))
	var mu sync.Mutex
	var reports [][]byte
	api, readResult := workerExecutionControlPlane(t, `import os
open('counter', 'x').write('executed once')
print(os.urandom(32).hex())`, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/worker/v1/complete" {
				next.ServeHTTP(w, r)
				return
			}
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			mu.Lock()
			reports = append(reports, body)
			first := len(reports) == 1
			mu.Unlock()
			if first {
				// The real Control Plane commits the result, then the HTTPS
				// connection disappears before its acknowledgement reaches Worker.
				recorder := httptest.NewRecorder()
				next.ServeHTTP(recorder, r)
				if recorder.Code != http.StatusNoContent {
					t.Errorf("first result was not accepted: HTTP %d", recorder.Code)
				}
				connection, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				_ = connection.Close()
				return
			}
			next.ServeHTTP(w, r)
		})
	})
	node, err := worker.New(worker.Config{API: api, SupervisorSocket: fixture.socketPath, ProfileIdentity: identity, RetryInterval: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	claimed, err := node.RunOnce(ctx)
	if err != nil || !claimed {
		t.Fatalf("Worker must retry the lost acknowledgement: claimed=%v, error=%v", claimed, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(reports) < 2 {
		t.Fatal("lost acknowledgement was not retried")
	}
	for _, report := range reports[1:] {
		if !bytes.Equal(reports[0], report) {
			t.Fatal("retry changed the lease or immutable Execution Result")
		}
	}
	result := readResult()
	nonce, err := hex.DecodeString(strings.TrimSuffix(result.Stdout, "\n"))
	if err != nil || len(nonce) != 32 || result.ExitCode != 0 {
		t.Fatalf("unexpected Workload result: %+v", result)
	}
	// Re-executing in the same Sandbox fails exclusive creation; executing in
	// another Sandbox generates a different nonce and changes the report bytes.
	assertNoResourceCgroups(t)
}

func TestWorkerExecutionCommandUsesAnUnprivilegedAccountAndPrivateCA(t *testing.T) {
	binary := os.Getenv("WORKER_CLI")
	if binary == "" {
		t.Fatal("WORKER_CLI must name the prebuilt Worker command")
	}
	setpriv, err := exec.LookPath("setpriv")
	if err != nil {
		t.Fatal("real Worker acceptance requires setpriv:", err)
	}
	fixture, identity := installedSandboxExecutionFixture(t)
	// The Supervisor remains root, but publishes its restricted socket to the
	// dedicated Worker group. The Worker cannot traverse either runtime root.
	if err := os.Chmod(filepath.Dir(filepath.Dir(fixture.socketPath)), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(filepath.Dir(fixture.socketPath), 0, 65534); err != nil {
		t.Fatal(err)
	}
	fixture.launchPrefix = []string{setpriv, "--regid=65534", "--clear-groups"}
	t.Cleanup(startSandboxSupervisor(t, fixture,
		"--subuid-start", "200000", "--subgid-start", "300000", "--subid-count", "65536",
		"--cgroup-root", os.Getenv("SANDBOX_CGROUP_ROOT")))
	queue, credentials := workerQueueFixture(t, time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := queue.Enqueue(ctx, executionqueue.Execution{ExecutionID: "execution-cli", AgentRunID: "run-cli", Source: "print('unprivileged-worker')"}); err != nil {
		t.Fatal(err)
	}
	issued, err := credentials.Provision(ctx, "worker-cli", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(workerapi.NewHandler(queue))
	t.Cleanup(server.Close)
	directory := shortSandboxSupervisorTemporaryDirectory(t)
	if err := os.Chmod(directory, 0755); err != nil {
		t.Fatal(err)
	}
	credentialPath := filepath.Join(directory, "credential")
	if err := os.WriteFile(credentialPath, []byte(issued.Token+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(credentialPath, 65534, 65534); err != nil {
		t.Fatal(err)
	}
	caPath := filepath.Join(directory, "control-plane-ca.pem")
	if err := os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0644); err != nil {
		t.Fatal(err)
	}
	arguments := []string{"--once", "--control-plane", server.URL, "--credential-file", credentialPath,
		"--ca-file", caPath, "--supervisor-socket", fixture.socketPath, "--profile-identity", identity, "--poll-wait=1s"}
	rootCommand := exec.CommandContext(ctx, binary, arguments...)
	rootCommand.Env = []string{"PATH=/usr/bin:/bin"}
	if output, err := rootCommand.CombinedOutput(); err == nil || !strings.Contains(string(output), "dedicated unprivileged account") {
		t.Fatalf("root Worker must be refused before claiming work: %v\n%s", err, output)
	}
	command := exec.CommandContext(ctx, binary, arguments...)
	command.Env = []string{"PATH=/usr/bin:/bin"}
	command.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 65534, Gid: 65534, Groups: []uint32{}}}
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("unprivileged Worker command failed: %v\n%s", err, output)
	}
	record, err := queue.Get(ctx, "execution-cli")
	if err != nil || record.State != "completed" || record.Result == nil || record.Result.Stdout != "unprivileged-worker\n" {
		t.Fatalf("unprivileged Worker did not persist its Execution Result: %+v, %v", record, err)
	}
	assertNoResourceCgroups(t)
}

func TestWorkerExecutionPreservesDeclaredBinaryOutputs(t *testing.T) {
	fixture, identity := installedSandboxExecutionFixture(t)
	t.Cleanup(startSandboxSupervisor(t, fixture,
		"--subuid-start", "200000", "--subgid-start", "300000", "--subid-count", "65536",
		"--cgroup-root", os.Getenv("SANDBOX_CGROUP_ROOT")))
	api, readResult := workerExecutionControlPlane(t, `import os
os.write(1, b'before\x00after')
os.write(2, b'error\x00detail')
open('blob.bin', 'wb').write(bytes([0, 1, 2, 255, 0]))`, nil, "blob.bin")
	node, err := worker.New(worker.Config{API: api, SupervisorSocket: fixture.socketPath, ProfileIdentity: identity})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if claimed, err := node.RunOnce(ctx); err != nil || !claimed {
		t.Fatalf("binary Execution: claimed=%v, error=%v", claimed, err)
	}
	result := readResult()
	if result.ExitCode != 0 || result.Stdout != "before\x00after" || result.Stderr != "error\x00detail" || result.OutputError != "" || len(result.Outputs) != 1 {
		t.Fatalf("NUL bytes or declared output were lost: %+v", result)
	}
	output := result.Outputs[0]
	if output.Path != "blob.bin" || output.Size != 5 || !bytes.Equal(output.Content, []byte{0, 1, 2, 255, 0}) || output.SHA256 != "79eebf0b2aae05b2490234d0b884581935b2e28127f06a74a5b48e758f86bd25" {
		t.Fatalf("declared binary snapshot changed before reaching the Control Plane: %+v", output)
	}
	assertNoResourceCgroups(t)
}

func workerExecutionControlPlane(t *testing.T, source string, intercept func(http.Handler) http.Handler, outputPaths ...string) (*workerapi.Client, func() sandboxsupervisor.ExecutePythonResult) {
	t.Helper()
	queue, credentials := workerQueueFixture(t, time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := queue.Enqueue(ctx, executionqueue.Execution{ExecutionID: "execution-one", AgentRunID: "run-one", Source: source, OutputPaths: outputPaths}); err != nil {
		t.Fatal(err)
	}
	issued, err := credentials.Provision(ctx, "worker-one", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	handler := workerapi.NewHandler(queue)
	if intercept != nil {
		handler = intercept(handler)
	}
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	client, err := workerapi.NewClient(server.URL, issued.Token, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	return client, func() sandboxsupervisor.ExecutePythonResult {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		record, err := queue.Get(ctx, "execution-one")
		if err != nil || record.Result == nil {
			t.Fatalf("read accepted Execution Result: %+v, %v", record, err)
		}
		return *record.Result
	}
}
