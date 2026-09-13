//go:build linux && sandbox_root && profilebundle_root

package workspace_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TH1NKING/Build-your-own-Docker-with-Go/internal/sandboxsupervisor"
)

func TestSandboxExecutionAttachmentsClientAcceptsSixteenWithoutLeakingHandlesOrSpendingOutputSlots(t *testing.T) {
	fixture, identity := installedSandboxExecutionFixture(t)
	staging := sandboxAttachmentStaging(t, fixture)
	var inputs []sandboxsupervisor.AttachmentInput
	var raw []map[string]string
	var sources []os.FileInfo
	for index := 0; index < 17; index++ {
		id := fmt.Sprintf("source-%02d", index)
		name := fmt.Sprintf("input-%02d.txt", index)
		writeSandboxAttachment(t, staging, id, "trusted")
		inputs = append(inputs, sandboxsupervisor.AttachmentInput{StagingID: id, Name: name})
		raw = append(raw, map[string]string{"staging_id": id, "name": name})
		info, err := os.Stat(filepath.Join(staging, id))
		if err != nil {
			t.Fatal(err)
		}
		sources = append(sources, info)
	}
	arguments := append(attachmentSupervisorArguments(staging), "--workspace-files", "3")
	t.Cleanup(startSandboxSupervisor(t, fixture, arguments...))
	assertSandboxSupervisorErrorCode(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath,
		attachmentCreateRequest(t, "run-slots", identity, raw)), "invalid_reference")
	client := sandboxsupervisor.NewClient(fixture.socketPath)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	created, err := client.CreateSandbox(ctx, sandboxsupervisor.CreateSandboxRequest{
		RequestID: "client-inputs", SandboxID: "run-slots", ProfileIdentity: identity, Attachments: inputs[:16],
	})
	if err != nil || created.Error != nil || created.Result == nil || created.Result.SandboxID != "run-slots" {
		t.Fatalf("public client failed to create with sixteen Attachments: %+v, %v", created, err)
	}
	// Readiness has already exec'd trusted Init. Compare kernel file identity,
	// not pathname strings, so either an original or reopened O_PATH leak fails.
	pid := sandboxProcessForRoot(t, fixture, identity)
	fdRoot := filepath.Join("/proc", pid, "fd")
	descriptors, err := os.ReadDir(fdRoot)
	if err != nil {
		t.Fatal(err)
	}
	for _, descriptor := range descriptors {
		info, err := os.Stat(filepath.Join(fdRoot, descriptor.Name()))
		if os.IsNotExist(err) {
			continue // A Go runtime descriptor can close between listing and stat.
		}
		if err != nil {
			t.Fatal(err)
		}
		for _, source := range sources {
			if os.SameFile(info, source) {
				t.Fatalf("Sandbox Init retained an Attachment descriptor: fd=%s", descriptor.Name())
			}
		}
	}
	result := executeSandboxPython(t, fixture, "run-slots", "slot-budget", `import errno, os
names = os.listdir('/workspace/input')
assert len(names) == 16, names
inputs = [os.stat('/workspace/input/' + name) for name in names]
for fd in os.listdir('/proc/self/fd'):
    try: held = os.fstat(int(fd))
    except OSError: continue
    assert all((held.st_dev, held.st_ino) != (source.st_dev, source.st_ino) for source in inputs), fd
for name in names: assert open('/workspace/input/' + name).read() == 'trusted'
for number in range(3): open(str(number), 'w').close()
try: open('fourth', 'w').close()
except OSError as error: assert error.errno == errno.ENOSPC, error
else: raise AssertionError('input mounts changed the three-file output budget')
print('16 inputs; 3 output slots; no inherited input handles')`)
	if result.ExitCode != 0 || result.Stdout != "16 inputs; 3 output slots; no inherited input handles\n" {
		t.Fatalf("Attachment count, FD isolation, or writable inode budget changed: %+v", result)
	}
	t.Log(strings.TrimSpace(result.Stdout))
	assertSandboxDestroyed(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath, destroySandboxWireRequest(t, "run-slots")), "run-slots")
}

func TestSandboxExecutionAttachmentsDisabledRejectsInputsAndKeepsEmptyCreateAvailable(t *testing.T) {
	fixture, identity := installedSandboxExecutionFixture(t)
	t.Cleanup(startSandboxSupervisor(t, fixture,
		"--subuid-start", "200000", "--subgid-start", "300000", "--subid-count", "65536"))
	assertSandboxSupervisorErrorCode(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath,
		attachmentCreateRequest(t, "run-disabled", identity, []map[string]string{{"staging_id": "known", "name": "input.txt"}})), "operation_unavailable")
	assertSandboxCreated(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath,
		createSandboxWireRequest(t, "empty-create", "run-disabled", identity)), "run-disabled")
	assertSandboxDestroyed(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath, destroySandboxWireRequest(t, "run-disabled")), "run-disabled")
}

func TestSandboxExecutionAttachmentsEnforceExactAggregateByteBudget(t *testing.T) {
	fixture, identity := installedSandboxExecutionFixture(t)
	staging := sandboxAttachmentStaging(t, fixture)
	var inputs []map[string]string
	for index := 0; index < 5; index++ {
		id := fmt.Sprintf("large-%d", index)
		file, err := os.OpenFile(filepath.Join(staging, id), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o444)
		if err != nil {
			t.Fatal(err)
		}
		// Sparse files exercise declared-byte admission without allocating or
		// reading 100 MiB in the VM. Runtime reads sample their final byte only.
		if err := file.Truncate(20 << 20); err != nil {
			file.Close()
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		inputs = append(inputs, map[string]string{"staging_id": id, "name": id})
	}
	writeSandboxAttachment(t, staging, "extra-byte", "x")
	arguments := append(attachmentSupervisorArguments(staging), "--workspace-bytes", "4096", "--workspace-files", "1")
	t.Cleanup(startSandboxSupervisor(t, fixture, arguments...))
	overflow := append(append([]map[string]string{}, inputs...), map[string]string{"staging_id": "extra-byte", "name": "extra"})
	assertSandboxSupervisorErrorCode(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath,
		attachmentCreateRequest(t, "run-total", identity, overflow)), "invalid_reference")
	assertSandboxCreated(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath,
		attachmentCreateRequest(t, "run-total", identity, inputs)), "run-total")
	result := executeSandboxPython(t, fixture, "run-total", "exact-total", `import errno, os
names = os.listdir('/workspace/input')
assert len(names) == 5
assert sum(os.stat('/workspace/input/' + name).st_size for name in names) == 100 << 20
for name in names:
    with open('/workspace/input/' + name, 'rb') as source:
        source.seek(-1, os.SEEK_END)
        assert source.read() == b'\0'
with open('one-output', 'wb', buffering=0) as output: assert output.write(b'x' * 4096) == 4096
try: open('second-output', 'wb').close()
except OSError as error: assert error.errno == errno.ENOSPC, error
else: raise AssertionError('Attachment mounts changed the output inode budget')
print('100 MiB accepted; 100 MiB plus one byte rejected; output budget independent')`)
	if result.ExitCode != 0 || result.Stdout != "100 MiB accepted; 100 MiB plus one byte rejected; output budget independent\n" {
		t.Fatalf("aggregate Attachment boundary failed: %+v", result)
	}
	t.Log(strings.TrimSpace(result.Stdout))
	assertSandboxDestroyed(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath, destroySandboxWireRequest(t, "run-total")), "run-total")
}

func TestSandboxExecutionAttachmentsRejectUnsafeStartupRoot(t *testing.T) {
	for _, scenario := range []string{"symlink", "group-writable"} {
		t.Run(scenario, func(t *testing.T) {
			fixture := newSafeSandboxSupervisorFixture(t)
			staging := sandboxAttachmentStaging(t, fixture)
			if scenario == "symlink" {
				alias := staging + "-alias"
				if err := os.Symlink(staging, alias); err != nil {
					t.Fatal(err)
				}
				staging = alias
			} else if err := os.Chmod(staging, 0o775); err != nil {
				t.Fatal(err)
			}
			commandPath := sandboxdCommandPath(t)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, commandPath, "--socket", fixture.socketPath,
				"--profile-store", fixture.profileStore, "--sandbox-root", fixture.sandboxRoot, "--attachment-root", staging)
			output, err := command.CombinedOutput()
			if err == nil || ctx.Err() != nil || !strings.Contains(string(output), "Attachment root") {
				t.Fatalf("unsafe Attachment root %s was not rejected at startup: %v\n%s", scenario, err, output)
			}
			if _, err := os.Lstat(fixture.socketPath); !os.IsNotExist(err) {
				t.Fatalf("unsafe Attachment root published a control socket: %v", err)
			}
		})
	}
}

func TestSandboxExecutionAttachmentsInaccessibleAncestorRollsBackAndAllowsCorrectedRetry(t *testing.T) {
	fixture, identity := installedSandboxExecutionFixture(t)
	staging := sandboxAttachmentStaging(t, fixture)
	writeSandboxAttachment(t, staging, "valid", "trusted")
	ancestor := filepath.Dir(staging)
	if err := os.Chmod(ancestor, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(startSandboxSupervisor(t, fixture, attachmentSupervisorArguments(staging)...))
	request := attachmentCreateRequest(t, "run-retry", identity, []map[string]string{{"staging_id": "valid", "name": "input.txt"}})
	assertSandboxSupervisorErrorCode(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath, request), "creation_failed")
	if info, err := os.Stat(ancestor); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("failed bootstrap altered trusted host permissions: %v, %v", info, err)
	}
	if _, err := os.Lstat(filepath.Join(fixture.sandboxRoot, "run-retry")); !os.IsNotExist(err) {
		t.Fatalf("failed bootstrap retained its runtime directory: %v", err)
	}
	if processes := sandboxProcessesForRoot(t, fixture, identity); len(processes) != 0 {
		t.Fatalf("failed bootstrap retained Profile-rooted processes: %v", processes)
	}
	assertNoResourceCgroups(t)
	// Only this test's trusted host-side action repairs the directory. The
	// Supervisor must neither relax host DAC nor consume a failed Sandbox ID.
	if err := os.Chmod(ancestor, 0o755); err != nil {
		t.Fatal(err)
	}
	assertSandboxCreated(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath, request), "run-retry")
	result := executeSandboxPython(t, fixture, "run-retry", "corrected", "assert open('/workspace/input/input.txt').read() == 'trusted'; print('corrected retry')")
	if result.ExitCode != 0 || result.Stdout != "corrected retry\n" {
		t.Fatalf("corrected Attachment creation did not recover: %+v", result)
	}
	assertSandboxDestroyed(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath, destroySandboxWireRequest(t, "run-retry")), "run-retry")
}
