//go:build linux && sandbox_root && profilebundle_root

package workspace_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/TH1NKING/Build-your-own-Docker-with-Go/internal/sandboxsupervisor"
)

func TestSandboxExecutionStorageDefaults(t *testing.T) {
	fixture, identity := installedSandboxExecutionFixture(t)
	t.Cleanup(startSandboxSupervisor(t, fixture,
		"--subuid-start", "200000", "--subgid-start", "300000", "--subid-count", "65536"))
	assertSandboxCreated(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath,
		createSandboxWireRequest(t, "create", "run-default-storage", identity)), "run-default-storage")
	client := sandboxsupervisor.NewClient(fixture.socketPath)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	response, err := client.ExecutePython(ctx, sandboxsupervisor.ExecutePythonRequest{
		RequestID: "defaults", SandboxID: "run-default-storage", ExecutionID: "defaults",
		Source: `import errno, os
for root, megabytes, files in [('/workspace/output', 512, 5000), ('/tmp', 16, 1023)]:
    with open(root + '/full', 'wb', buffering=0) as file:
        block = b'x' * (1024 * 1024)
        for _ in range(megabytes): assert file.write(block) == len(block)
        try: file.write(b'x')
        except OSError as error: assert error.errno == errno.ENOSPC, error
        else: raise AssertionError('default byte budget was exceeded')
    for n in range(files - 1): open(root + '/' + str(n), 'wb').close()
    try: open(root + '/extra', 'wb').close()
    except OSError as error: assert error.errno == errno.ENOSPC, error
    else: raise AssertionError('default file budget was exceeded')
print('defaults')`,
	})
	if err != nil || response.Error != nil || response.Result == nil {
		t.Fatalf("default storage acceptance failed: %+v, %v", response, err)
	}
	if response.Result.ExitCode != 0 || response.Result.Stdout != "defaults\n" || response.Result.ResourceUsage.OOMEvents != 0 {
		t.Fatalf("default Workspace and temporary budgets did not reach and enforce their boundaries: %+v", response.Result)
	}
}

func TestSandboxExecutionStorageSparseAndMappedWritesStayBounded(t *testing.T) {
	fixture, identity := installedSandboxExecutionFixture(t)
	t.Cleanup(startSandboxSupervisor(t, fixture,
		"--subuid-start", "200000", "--subgid-start", "300000", "--subid-count", "65536",
		"--workspace-bytes", "4096"))
	assertSandboxCreated(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath,
		createSandboxWireRequest(t, "create", "run-sparse", identity)), "run-sparse")
	result := executeSandboxPython(t, fixture, "run-sparse", "sparse", `import errno, mmap, os, resource, signal
resource.setrlimit(resource.RLIMIT_CORE, (0, 0))
with open('sparse', 'w+b', buffering=0) as file:
    os.ftruncate(file.fileno(), 1 << 30)
    assert os.fstat(file.fileno()).st_size == 1 << 30
    assert os.pwrite(file.fileno(), b'x' * 4096, (1 << 30) - 4096) == 4096
    try: os.pwrite(file.fileno(), b'y', 0)
    except OSError as error: assert error.errno == errno.ENOSPC, error
    else: raise AssertionError('sparse allocation escaped the budget')
os.unlink('sparse')
with open('mapped', 'w+b', buffering=0) as file:
    os.ftruncate(file.fileno(), 8192)
    assert file.write(b'x' * 4096) == 4096
    pid = os.fork()
    if pid == 0:
        mapping = mmap.mmap(file.fileno(), 8192, flags=mmap.MAP_SHARED, prot=mmap.PROT_READ | mmap.PROT_WRITE)
        mapping[4096] = 1
        os._exit(42)
    status = os.waitpid(pid, 0)[1]
    assert os.WIFSIGNALED(status) and os.WTERMSIG(status) == signal.SIGBUS, status
os.unlink('mapped')
print('allocation-bounded')`)
	if result.ExitCode != 0 || result.Stdout != "allocation-bounded\n" || result.ResourceUsage.OOMEvents != 0 {
		t.Fatalf("sparse and shared mappings must remain bounded by allocated storage: %+v", result)
	}
	clean := executeSandboxPython(t, fixture, "run-sparse", "after-sigbus", `with open('healthy', 'wb', buffering=0) as file:
    assert file.write(b'x' * 4096) == 4096
print('healthy')`)
	if clean.ExitCode != 0 || clean.Stdout != "healthy\n" {
		t.Fatalf("SIGBUS in a descendant prevented Sandbox reuse: %+v", clean)
	}
}

func TestSandboxExecutionStorageIsIndependentAcrossSandboxesAndDestroyed(t *testing.T) {
	fixture, identity := installedSandboxExecutionFixture(t)
	t.Cleanup(startSandboxSupervisor(t, fixture,
		"--subuid-start", "200000", "--subgid-start", "300000", "--subid-count", "131072",
		"--workspace-bytes", "4096", "--workspace-files", "1",
		"--temporary-bytes", "4096", "--temporary-files", "1"))
	for _, id := range []string{"run-first", "run-second"} {
		assertSandboxCreated(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath,
			createSandboxWireRequest(t, "create-"+id, id, identity)), id)
		result := executeSandboxPython(t, fixture, id, "fill", `import os
for root in ['/workspace/output', '/tmp']:
    assert os.listdir(root) == []
    with open(root + '/full', 'wb', buffering=0) as file:
        assert file.write(b'x' * 4096) == 4096
print('independent')`)
		if result.ExitCode != 0 || result.Stdout != "independent\n" {
			t.Fatalf("one full Sandbox consumed another Sandbox's budget or exposed its files: %+v", result)
		}
	}
	client := sandboxsupervisor.NewClient(fixture.socketPath)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, id := range []string{"run-first", "run-second"} {
		response, err := client.DestroySandbox(ctx, sandboxsupervisor.DestroySandboxRequest{RequestID: "destroy-" + id, SandboxID: id})
		if err != nil || response.Error != nil || response.Result == nil {
			t.Fatalf("destroy full Sandbox %s: %+v, %v", id, response, err)
		}
	}
	assertNoResourceCgroups(t)
	assertSandboxCreated(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath,
		createSandboxWireRequest(t, "create-fresh", "run-fresh", identity)), "run-fresh")
	clean := executeSandboxPython(t, fixture, "run-fresh", "fresh", `import os
for root in ['/workspace/output', '/tmp']:
    assert os.listdir(root) == []
    with open(root + '/new', 'wb', buffering=0) as file:
        assert file.write(b'y' * 4096) == 4096
print('fresh')`)
	if clean.ExitCode != 0 || clean.Stdout != "fresh\n" {
		t.Fatalf("destroyed storage survived or prevented a fresh Sandbox: %+v", clean)
	}
}

func TestSandboxExecutionStorageMetadataAndOpenUnlinkedFilesStayCharged(t *testing.T) {
	fixture, identity := installedSandboxExecutionFixture(t)
	t.Cleanup(startSandboxSupervisor(t, fixture,
		"--subuid-start", "200000", "--subgid-start", "300000", "--subid-count", "65536",
		"--workspace-bytes", "8192", "--workspace-files", "4",
		"--temporary-bytes", "8192", "--temporary-files", "4"))
	assertSandboxCreated(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath,
		createSandboxWireRequest(t, "create", "run-metadata", identity)), "run-metadata")
	result := executeSandboxPython(t, fixture, "run-metadata", "metadata", `import ctypes, errno, os
# python-data-v1 permits amd64 link (86); CPython os.link uses denied linkat.
libc = ctypes.CDLL(None, use_errno=True)
libc.syscall.restype = ctypes.c_long
def link(source, target):
    if libc.syscall(ctypes.c_long(86), ctypes.c_char_p(source.encode()), ctypes.c_char_p(target.encode())) != 0:
        error = ctypes.get_errno()
        raise OSError(error, os.strerror(error))
def refused(action):
    try: action()
    except OSError as error: assert error.errno == errno.ENOSPC, error
    else: raise AssertionError('metadata or data escaped its budget')
for root in ['/workspace/output', '/tmp']:
    os.chdir(root)
    os.mkdir('directory')
    held = open('file', 'w+b', buffering=0)
    assert held.write(b'x' * 8192) == 8192
    os.symlink('file', 'symlink')
    link('file', 'hardlink')
    refused(lambda: open('extra', 'wb'))
    refused(lambda: os.mkdir('extra'))
    refused(lambda: os.symlink('file', 'extra'))
    refused(lambda: link('file', 'extra'))
    os.unlink('hardlink')
    os.unlink('file')
    # The unlinked, open inode still uses one slot and both allocated pages.
    with open('replacement', 'wb', buffering=0) as replacement:
        refused(lambda: open('extra', 'wb'))
        refused(lambda: replacement.write(b'y'))
        assert held.seek(0) == 0
        assert held.read() == b'x' * 8192
        held.close()
        assert replacement.write(b'y' * 8192) == 8192
    open('last-slot', 'wb').close()
    refused(lambda: open('extra', 'wb'))
print('metadata-bounded')`)
	if result.ExitCode != 0 || result.Stdout != "metadata-bounded\n" || result.ResourceUsage.OOMEvents != 0 {
		t.Fatalf("links and unlinked open files must remain charged until released: %+v", result)
	}
}

func TestSandboxExecutionStorageConcurrentWritersShareOneBudget(t *testing.T) {
	fixture, identity := installedSandboxExecutionFixture(t)
	t.Cleanup(startSandboxSupervisor(t, fixture,
		"--subuid-start", "200000", "--subgid-start", "300000", "--subid-count", "65536",
		"--workspace-bytes", "32768", "--workspace-files", "4"))
	assertSandboxCreated(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath,
		createSandboxWireRequest(t, "create", "run-writers", identity)), "run-writers")
	result := executeSandboxPython(t, fixture, "run-writers", "writers", `import errno, os
reader, writer = os.pipe()
children = []
for n in range(4):
    pid = os.fork()
    if pid == 0:
        os.close(writer)
        assert os.read(reader, 1) == b'x'
        os.close(reader)
        with open(str(n), 'wb', buffering=0) as file:
            try:
                for _ in range(9): assert file.write(b'x' * 4096) == 4096
            except OSError as error:
                assert error.errno == errno.ENOSPC, error
            else:
                raise AssertionError('writer escaped aggregate storage budget')
        os._exit(0)
    children.append(pid)
os.close(reader)
assert os.write(writer, b'xxxx') == 4
os.close(writer)
for pid in children: assert os.waitpid(pid, 0)[1] == 0
assert sum(os.stat(str(n)).st_size for n in range(4)) == 32768
try: open('extra', 'wb').close()
except OSError as error: assert error.errno == errno.ENOSPC, error
else: raise AssertionError('file budget was not shared by descendants')
print('shared-budget')`)
	if result.ExitCode != 0 || result.Stdout != "shared-budget\n" || result.ResourceUsage.OOMEvents != 0 {
		t.Fatalf("concurrent Workload descendants must share one storage budget: %+v", result)
	}
}

func TestSandboxExecutionStorageExhaustionPreservesReadOnlyAreas(t *testing.T) {
	fixture, identity := installedSandboxExecutionFixture(t)
	sentinel := filepath.Join(t.TempDir(), "host-sentinel")
	if err := os.WriteFile(sentinel, []byte("host-unchanged"), 0o444); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(startSandboxSupervisor(t, fixture,
		"--subuid-start", "200000", "--subgid-start", "300000", "--subid-count", "65536",
		"--workspace-bytes", "4096", "--workspace-files", "1",
		"--temporary-bytes", "4096", "--temporary-files", "1"))
	assertSandboxCreated(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath,
		createSandboxWireRequest(t, "create", "run-readonly", identity)), "run-readonly")
	result := executeSandboxPython(t, fixture, "run-readonly", "protect", fmt.Sprintf(`import errno, hashlib, os
assert os.path.isdir('/workspace/input')
policy_before = hashlib.sha256(open('/system-call-policy.json', 'rb').read()).digest()
for root in ['/workspace/output', '/tmp']:
    with open(root + '/full', 'wb', buffering=0) as file:
        assert file.write(b'x' * 4096) == 4096
        try: file.write(b'x')
        except OSError as error: assert error.errno == errno.ENOSPC, error
        else: raise AssertionError('write escaped the byte budget')
    try: open(root + '/extra', 'wb').close()
    except OSError as error: assert error.errno == errno.ENOSPC, error
    else: raise AssertionError('create escaped the file budget')
for path in ['/system-call-policy.json', '/workspace/input/forbidden', '/workspace/forbidden', %q]:
    try: open(path, 'wb').close()
    except OSError as error: assert error.errno in (errno.EROFS, errno.EACCES, errno.ENOENT), error
    else: raise AssertionError('writable protected path: ' + path)
for action in [lambda: os.chmod('/workspace/input', 0o777), lambda: os.rmdir('/workspace/input'), lambda: os.rename('/workspace/output', '/workspace/input')]:
    try: action()
    except OSError as error: assert error.errno in (errno.EPERM, errno.EACCES, errno.EROFS), error
    else: raise AssertionError('Workload changed the protected layout')
assert hashlib.sha256(open('/system-call-policy.json', 'rb').read()).digest() == policy_before
print('protected')`, sentinel))
	if result.ExitCode != 0 || result.Stdout != "protected\n" {
		t.Fatalf("storage exhaustion must preserve Profile, input slot and host boundaries: %+v", result)
	}
	contents, err := os.ReadFile(sentinel)
	if err != nil || string(contents) != "host-unchanged" {
		t.Fatalf("host file changed after Sandbox storage exhaustion: %q, %v", contents, err)
	}
}

func TestSandboxExecutionStorageTemporaryBytesAreIndependent(t *testing.T) {
	fixture, identity := installedSandboxExecutionFixture(t)
	t.Cleanup(startSandboxSupervisor(t, fixture,
		"--subuid-start", "200000", "--subgid-start", "300000", "--subid-count", "65536",
		"--workspace-bytes", "8192", "--temporary-bytes", "4096"))
	assertSandboxCreated(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath,
		createSandboxWireRequest(t, "create", "run-temporary", identity)), "run-temporary")
	result := executeSandboxPython(t, fixture, "run-temporary", "fill", `import errno, os
with open('/tmp/full', 'wb', buffering=0) as file:
    assert file.write(b't' * 4096) == 4096
    try:
        file.write(b't')
    except OSError as error:
        assert error.errno == errno.ENOSPC, error
    else:
        raise AssertionError('temporary write escaped its budget')
with open('output', 'wb', buffering=0) as file:
    assert file.write(b'w' * 8192) == 8192
print('independent')`)
	if result.ExitCode != 0 || result.Stdout != "independent\n" || result.ResourceUsage.OOMEvents != 0 {
		t.Fatalf("temporary exhaustion must leave the Workspace allowance available: %+v", result)
	}
	reused := executeSandboxPython(t, fixture, "run-temporary", "reuse", `import errno, os
assert open('/tmp/full', 'rb').read() == b't' * 4096
os.unlink('/tmp/full')
with open('/tmp/replacement', 'wb', buffering=0) as file:
    assert file.write(b'n' * 4096) == 4096
assert open('output', 'rb').read() == b'w' * 8192
print('reused')`)
	if reused.ExitCode != 0 || reused.Stdout != "reused\n" {
		t.Fatalf("temporary files must retain their budget and be releasable across Executions: %+v", reused)
	}
}

func TestSandboxExecutionStorageConfiguredFileBudgets(t *testing.T) {
	fixture, identity := installedSandboxExecutionFixture(t)
	t.Cleanup(startSandboxSupervisor(t, fixture,
		"--subuid-start", "200000", "--subgid-start", "300000", "--subid-count", "65536",
		"--workspace-files", "3", "--temporary-files", "2"))
	assertSandboxCreated(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath,
		createSandboxWireRequest(t, "create", "run-files", identity)), "run-files")
	for _, test := range []struct {
		name, root string
		files      int
	}{{"workspace", "/workspace/output", 3}, {"temporary", "/tmp", 2}} {
		t.Run(test.name, func(t *testing.T) {
			result := executeSandboxPython(t, fixture, "run-files", test.name, fmt.Sprintf(`import errno, os
os.chdir(%q)
for n in range(%d):
    with open(str(n), 'wb') as file: file.write(b'x')
try:
    open('overflow', 'wb').close()
except OSError as error:
    assert error.errno == errno.ENOSPC, error
else:
    raise AssertionError('file creation escaped the budget')
os.unlink('0')
with open('replacement', 'wb') as file: file.write(b'y')
print('file-budget')`, test.root, test.files))
			if result.ExitCode != 0 || result.Stdout != "file-budget\n" || result.ResourceUsage.OOMEvents != 0 {
				t.Fatalf("configured file budget must reject creation and release deleted files: %+v", result)
			}
		})
	}
}

func TestSandboxExecutionStorageConfiguredBytesRefuseWritesAndPermitReuse(t *testing.T) {
	fixture, identity := installedSandboxExecutionFixture(t)
	t.Cleanup(startSandboxSupervisor(t, fixture,
		"--subuid-start", "200000", "--subgid-start", "300000", "--subid-count", "65536",
		"--workspace-bytes", "8192"))
	assertSandboxCreated(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath,
		createSandboxWireRequest(t, "create", "run-storage", identity)), "run-storage")
	full := executeSandboxPython(t, fixture, "run-storage", "fill", `import errno, os
with open('retained', 'wb', buffering=0) as file:
    assert file.write(b'x' * 8192) == 8192
    try:
        file.write(b'x')
    except OSError as error:
        assert error.errno == errno.ENOSPC, error
    else:
        raise AssertionError('write escaped the storage budget')
print('full')`)
	if full.ExitCode != 0 || full.TerminalReason != "exited" || full.Stdout != "full\n" || full.ResourceUsage.OOMEvents != 0 {
		t.Fatalf("configured storage exhaustion must refuse writes without OOM: %+v", full)
	}
	reused := executeSandboxPython(t, fixture, "run-storage", "reuse", `import errno, os
assert open('retained', 'rb').read() == b'x' * 8192
with open('next', 'wb', buffering=0) as file:
    try:
        file.write(b'y')
    except OSError as error:
        assert error.errno == errno.ENOSPC, error
    else:
        raise AssertionError('a later Execution reset the storage budget')
os.unlink('retained')
with open('next', 'wb', buffering=0) as file:
    assert file.write(b'y' * 8192) == 8192
print('reused')`)
	if reused.ExitCode != 0 || reused.Stdout != "reused\n" || reused.ResourceUsage.OOMEvents != 0 {
		t.Fatalf("retained storage must stay charged until released: %+v", reused)
	}
}
