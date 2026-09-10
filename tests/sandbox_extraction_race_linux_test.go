//go:build linux && sandbox_root && profilebundle_root

package workspace_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/TH1NKING/Build-your-own-Docker-with-Go/internal/sandboxsupervisor"
)

func TestSandboxExecutionExtractionRejectsChangesBeforePublishing(t *testing.T) {
	fixture, identity := installedSandboxExecutionFixture(t)
	t.Cleanup(startSandboxSupervisor(t, fixture,
		"--subuid-start", "200000", "--subgid-start", "300000", "--subid-count", "65536",
		"--cgroup-root", os.Getenv("SANDBOX_CGROUP_ROOT")))
	const sandboxID = "run-extraction-races"
	assertSandboxCreated(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath,
		createSandboxWireRequest(t, "create", sandboxID, identity)), sandboxID)
	initPID := sandboxProcessForRoot(t, fixture, identity)
	workspace := filepath.Join("/proc", initPID, "root", "workspace", "output")
	client := sandboxsupervisor.NewClient(fixture.socketPath)

	for _, test := range []struct {
		name   string
		mutate func(string, *os.File) error
	}{
		{"unchanged", nil},
		{"replaced-file", func(directory string, _ *os.File) error {
			return os.Rename(filepath.Join(directory, "replacement"), filepath.Join(directory, "parent", "data"))
		}},
		{"changed-content", func(_ string, file *os.File) error {
			// Preserve the length, so checking size alone cannot pass this test.
			if _, err := file.WriteAt([]byte("changed"), 0); err != nil {
				return err
			}
			// Set a distinct timestamp without depending on filesystem clock
			// granularity or a delay between the initial stat and this write.
			return syscall.Futimes(int(file.Fd()), []syscall.Timeval{{Sec: 1}, {Sec: 1}})
		}},
		{"relocated-parent", func(directory string, _ *os.File) error {
			// Moving this directory needs IDs mapped in the tmpfs's owning
			// User Namespace; host UID 0 gets EOVERFLOW. The trusted harness
			// joins that namespace only to inject the filesystem fault.
			inside := filepath.Join("/workspace/output", filepath.Base(directory))
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, "nsenter", "--target", initPID,
				"--user", "--mount", "--pid", "--net", "--root", "--wd=/", "--",
				"/opt/python/bin/python3", "-I", "-B", "-c",
				fmt.Sprintf("import os; os.rename(%q, %q)", filepath.Join(inside, "parent"), filepath.Join(inside, "relocated")))
			if output, err := command.CombinedOutput(); err != nil {
				return fmt.Errorf("relocate output parent with mapped IDs: %w\n%s", err, output)
			}
			return nil
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			// Workloads are already dead before extraction. The trusted host
			// injects the stronger concurrent-filesystem fault without adding a
			// production hook or giving a Workload access to Init's descriptors.
			// Create inodes through Python: host UID 0 is deliberately unmapped
			// in the Workspace tmpfs's owning User Namespace.
			directory := "race-" + test.name
			prepared := executeSandboxPython(t, fixture, sandboxID, "prepare-"+test.name, fmt.Sprintf(`import os
base = %q
os.mkdir(base)
os.mkdir(base + '/parent')
open(base + '/first', 'wb').write(b'abc')
open(base + '/parent/data', 'wb').write(b'initial')
open(base + '/replacement', 'wb').write(b'replace')
print('prepared')`, directory))
			if prepared.ExitCode != 0 || prepared.Stdout != "prepared\n" {
				t.Fatalf("prepare leased output fixtures: %+v", prepared)
			}
			lease, breaking, release := holdSandboxExtractionLease(t, filepath.Join(workspace, directory, "parent", "data"))
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			type outcome struct {
				response sandboxsupervisor.ExecutePythonResponse
				err      error
			}
			finished := make(chan outcome, 1)
			joined := make(chan struct{})
			go func() {
				defer close(joined)
				response, err := client.ExecutePython(ctx, sandboxsupervisor.ExecutePythonRequest{
					RequestID: "extract-" + test.name, SandboxID: sandboxID, ExecutionID: "extract-" + test.name,
					Source: "print('execution-finished')", OutputPaths: []string{directory + "/first", directory + "/parent/data"},
				})
				finished <- outcome{response, err}
			}()
			defer func() {
				// Release the kernel waiter on every failure path before cancelling
				// the operation; never leave Init blocked until lease-break-time.
				if err := release(); err != nil {
					t.Errorf("release output lease during cleanup: %v", err)
				}
				cancel()
				select {
				case <-joined:
				case <-time.After(3 * time.Second):
					t.Error("leased Execution client did not stop after cancellation")
				}
			}()

			select {
			case <-breaking:
				// SIGIO is the kernel's acknowledgement that a conflicting open
				// reached the lease. No fixed sleep chooses the mutation window.
			case result := <-finished:
				t.Fatalf("extraction completed without reaching the lease: %+v, %v", result.response, result.err)
			case <-time.After(3 * time.Second):
				t.Fatal("extraction did not reach the output write lease")
			}
			busy, err := client.ExecutePython(ctx, sandboxsupervisor.ExecutePythonRequest{
				RequestID: "overlap-" + test.name, SandboxID: sandboxID, ExecutionID: "overlap-" + test.name,
				Source: "raise AssertionError('extraction must retain exclusive execution ownership')",
			})
			if err != nil || busy.Result != nil || busy.Error == nil || busy.Error.Code != sandboxsupervisor.ErrorCodeSandboxBusy {
				t.Fatalf("next Execution entered while extraction was blocked: %+v, %v", busy, err)
			}
			pending, err := client.GetExecutionResult(ctx, sandboxsupervisor.GetExecutionResultRequest{
				RequestID: "pending-" + test.name, SandboxID: sandboxID, ExecutionID: "extract-" + test.name,
			})
			if err != nil || pending.Result != nil || pending.Error == nil || pending.Error.Code != sandboxsupervisor.ErrorCodeResultNotReady {
				t.Fatalf("partially extracted batch became readable: %+v, %v", pending, err)
			}
			if test.mutate != nil {
				if err := test.mutate(filepath.Join(workspace, directory), lease); err != nil {
					t.Fatalf("inject filesystem change while extraction is blocked: %v", err)
				}
			}
			if err := release(); err != nil {
				t.Fatalf("release output extraction: %v", err)
			}
			var completed outcome
			select {
			case completed = <-finished:
			case <-ctx.Done():
				t.Fatal("extraction did not complete after releasing the lease")
			}
			response := completed.response
			if completed.err != nil || response.Error != nil || response.Result == nil {
				t.Fatalf("filesystem change lost the Execution Result: %+v, %v", response, completed.err)
			}
			result := response.Result
			if result.ExitCode != 0 || result.Stdout != "execution-finished\n" || result.TerminalReason != sandboxsupervisor.ExecutionExited {
				t.Fatalf("extraction changed the completed Execution's status: %+v", result)
			}
			if test.mutate == nil {
				if result.OutputError != "" || len(result.Outputs) != 2 || string(result.Outputs[0].Content) != "abc" || string(result.Outputs[1].Content) != "initial" {
					t.Fatalf("an unchanged file failed the lease-controlled extraction: %+v", result)
				}
			} else if result.OutputError != sandboxsupervisor.OutputUnsafe || len(result.Outputs) != 0 {
				t.Fatalf("filesystem change published changed bytes or the earlier partial file: %+v", result)
			}
			reused := executeSandboxPython(t, fixture, sandboxID, "reuse-"+test.name, "print('reused')")
			if reused.ExitCode != 0 || reused.Stdout != "reused\n" {
				t.Fatalf("extraction failure prevented the next sequential Execution: %+v", reused)
			}
		})
	}
}

// Linux F_SETLEASE documents that a conflicting open blocks and notifies the
// lease holder with SIGIO, while the final close always releases the lease:
// https://man7.org/linux/man-pages/man2/F_SETLEASE.2const.html
// The root-only harness needs CAP_LEASE for the Workload-owned tmpfs inode.
func holdSandboxExtractionLease(t *testing.T, name string) (*os.File, <-chan os.Signal, func() error) {
	t.Helper()
	file, err := os.OpenFile(name, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open existing output for a kernel lease: %v", err)
	}
	breaking := make(chan os.Signal, 1)
	signal.Notify(breaking, syscall.SIGIO)
	active := false
	release := func() error {
		if !active {
			return nil
		}
		_, _, errno := syscall.Syscall(syscall.SYS_FCNTL, file.Fd(), syscall.F_SETLEASE, syscall.F_UNLCK)
		if errno != 0 {
			return errno
		}
		active = false
		return nil
	}
	t.Cleanup(func() {
		if err := release(); err != nil {
			t.Errorf("remove output lease: %v", err)
		}
		_ = file.Close() // Closing also releases a lease if explicit unlock failed.
		signal.Stop(breaking)
	})
	if _, _, errno := syscall.Syscall(syscall.SYS_FCNTL, file.Fd(), syscall.F_SETOWN, uintptr(os.Getpid())); errno != 0 {
		t.Fatalf("direct output lease signals to test process: %v", errno)
	}
	if _, _, errno := syscall.Syscall(syscall.SYS_FCNTL, file.Fd(), syscall.F_SETLEASE, syscall.F_WRLCK); errno != 0 {
		t.Fatalf("root Linux fixture must support write leases on Workspace tmpfs: %v", errno)
	}
	active = true
	return file, breaking, release
}
