//go:build linux && sandbox_root && profilebundle_root

package workspace_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSandboxExecutionSecurityDropsPrivilegesAcrossExec(t *testing.T) {
	fixture, identity := installedSandboxExecutionFixture(t)
	t.Cleanup(startSandboxSupervisor(t, fixture,
		"--subuid-start", "200000", "--subgid-start", "300000", "--subid-count", "65536",
		"--cgroup-root", os.Getenv("SANDBOX_CGROUP_ROOT")))
	assertSandboxCreated(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath,
		createSandboxWireRequest(t, "create", "run-privileges", identity)), "run-privileges")
	result := executeSandboxPython(t, fixture, "run-privileges", "probe", `import os, subprocess, sys
probe = '''import os
status = dict(line.split(':', 1) for line in open('/proc/self/status'))
assert status['NoNewPrivs'].strip() == '1', status['NoNewPrivs']
for field in ['CapInh', 'CapPrm', 'CapEff', 'CapBnd', 'CapAmb']:
    assert int(status[field], 16) == 0, (field, status[field])
assert os.getresuid() == (1000, 1000, 1000)
assert os.getresgid() == (1000, 1000, 1000)
assert os.getgroups() == []
try: os.setuid(0)
except PermissionError: pass
else: raise AssertionError('regained internal root')
'''
exec(probe)
subprocess.run([sys.executable, '-I', '-B', '-c', probe], check=True)
print('privileges-stay-dropped')`)
	if result.ExitCode != 0 || result.Stdout != "privileges-stay-dropped\n" {
		t.Fatalf("Workload or exec descendant retained privileges: %+v", result)
	}
}

func TestSandboxExecutionSecurityAllowsPythonDataWorkAndThreads(t *testing.T) {
	fixture, identity := installedSandboxExecutionFixture(t)
	t.Cleanup(startSandboxSupervisor(t, fixture,
		"--subuid-start", "200000", "--subgid-start", "300000", "--subid-count", "65536",
		"--cgroup-root", os.Getenv("SANDBOX_CGROUP_ROOT")))
	assertSandboxCreated(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath,
		createSandboxWireRequest(t, "create", "run-data", identity)), "run-data")
	result := executeSandboxPython(t, fixture, "run-data", "data", `import bz2, concurrent.futures, csv, ctypes, decimal, errno, gzip, hashlib, io, json, lzma, os, sqlite3, subprocess, sys, tempfile
rows = list(csv.DictReader(io.StringIO('name,amount\na,1.25\nb,3.75\n')))
assert sum(decimal.Decimal(row['amount']) for row in rows) == decimal.Decimal('5.00')
database = sqlite3.connect('data.sqlite')
database.execute('create table amounts (name text, amount text)')
database.executemany('insert into amounts values (?, ?)', [(r['name'], r['amount']) for r in rows])
database.commit()
assert database.execute('select count(*) from amounts').fetchone() == (2,)
database.close()
payload = json.dumps(rows).encode()
with tempfile.TemporaryDirectory() as directory:
    for codec in [gzip, bz2, lzma]:
        assert codec.decompress(codec.compress(payload)) == payload
    with open(os.path.join(directory, 'summary'), 'wb') as output: output.write(payload)
assert len(hashlib.sha256(payload).hexdigest()) == 64
def check_thread():
    status = dict(line.split(':', 1) for line in open('/proc/thread-self/status'))
    assert status['NoNewPrivs'].strip() == '1'
    assert status['Seccomp'].strip() == '2'
    for field in ['CapInh', 'CapPrm', 'CapEff', 'CapBnd', 'CapAmb']:
        assert int(status[field], 16) == 0, (field, status[field])
    libc = ctypes.CDLL(None, use_errno=True)
    assert libc.syscall(140, 0, 0) == -1 and ctypes.get_errno() == errno.EPERM
    return 42
with concurrent.futures.ThreadPoolExecutor(max_workers=2) as pool:
    assert [future.result() for future in [pool.submit(check_thread), pool.submit(check_thread)]] == [42, 42]
child = subprocess.run([sys.executable, '-I', '-B', '-c', "import ctypes, errno; libc=ctypes.CDLL(None, use_errno=True); assert libc.syscall(140, 0, 0) == -1 and ctypes.get_errno() == errno.EPERM; print('child-filtered')"], capture_output=True, text=True, check=True)
assert child.stdout == 'child-filtered\n'
print('data-and-descendants-ok')`)
	if result.ExitCode != 0 || result.Stdout != "data-and-descendants-ok\n" {
		t.Fatalf("reviewed Python data Workload or descendant restrictions failed: %+v", result)
	}
}

func TestSandboxExecutionSecurityDeniesNamespaceFlagsAndAlternateABIs(t *testing.T) {
	fixture, identity := installedSandboxExecutionFixture(t)
	t.Cleanup(startSandboxSupervisor(t, fixture,
		"--subuid-start", "200000", "--subgid-start", "300000", "--subid-count", "65536",
		"--cgroup-root", os.Getenv("SANDBOX_CGROUP_ROOT")))
	assertSandboxCreated(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath,
		createSandboxWireRequest(t, "create", "run-denials", identity)), "run-denials")
	result := executeSandboxPython(t, fixture, "run-denials", "probe", `import ctypes, errno, mmap, os
libc = ctypes.CDLL(None, use_errno=True)
libc.syscall.restype = ctypes.c_long
def denied(number, *arguments):
    ctypes.set_errno(0)
    value = libc.syscall(number, *arguments)
    assert value == -1 and ctypes.get_errno() == errno.EPERM, (number, value, ctypes.get_errno())
# unshare, setns, mount, ptrace, bpf, clone3, and attempts to relax NNP/seccomp.
for number, arguments in [(272, (0x10000000,)), (308, (-1, 0)), (165, (0, 0, 0, 0, 0)), (101, (0, 0, 0, 0)), (321, (0, 0, 0)), (435, (0, 0)), (157, (38, 0, 0, 0, 0)), (157, (22, 0, 0, 0, 0)), (317, (1, 0, 0))]:
    denied(number, *arguments)
# NEWNS/CGROUP/UTS/IPC/USER/PID/NET and unknown high bits must not
# become a successful ordinary clone through argument truncation.
for flag in [0x00020000, 0x02000000, 0x04000000, 0x08000000, 0x10000000, 0x20000000, 0x40000000, 1 << 32, 1 << 63]:
    ctypes.set_errno(0)
    value = libc.syscall(56, ctypes.c_uint64(flag | 17), 0, 0, 0, 0)
    if value == 0: os._exit(93)
    if value > 0: os.waitpid(value, 0)
    assert value == -1 and ctypes.get_errno() == errno.EPERM, (hex(flag), value, ctypes.get_errno())
# Same low bits as ARCH_SET_FS, different high bits. A low-word-only
# comparator would reach the kernel and change TLS instead of denying it.
denied(158, ctypes.c_uint64((1 << 32) | 4098), 0)
# x32 shares the amd64 audit architecture but has its own syscall-number bit.
denied(0x40000000 | 39)
# int 0x80 with i386 getpid (20) must not match amd64 writev (also 20).
code = mmap.mmap(-1, mmap.PAGESIZE, prot=mmap.PROT_READ | mmap.PROT_WRITE | mmap.PROT_EXEC)
code.write(bytes.fromhex('b814000000cd804898c3'))
address = ctypes.addressof(ctypes.c_char.from_buffer(code))
assert ctypes.CFUNCTYPE(ctypes.c_long)(address)() == -errno.EPERM
code.close()
for action in [lambda: open('/system-call-policy.json', 'wb'), lambda: os.unlink('/system-call-policy.json')]:
    try: action()
    except OSError as error: assert error.errno in [errno.EROFS, errno.EACCES, errno.EPERM]
    else: raise AssertionError('Workload changed its System Call Policy')
print('forbidden-operations-denied')`)
	if result.ExitCode != 0 || result.Stdout != "forbidden-operations-denied\n" {
		t.Fatalf("Workload bypassed a security restriction: %+v", result)
	}
	clean := executeSandboxPython(t, fixture, "run-denials", "clean", "print('clean')")
	if clean.ExitCode != 0 || clean.Stdout != "clean\n" {
		t.Fatalf("denials prevented clean sequential reuse: %+v", clean)
	}
}

func TestSandboxExecutionSecurityRejectsChangedPolicyBeforeCreation(t *testing.T) {
	for _, change := range []string{"contents", "missing", "symlink", "writable", "owner"} {
		t.Run(change, func(t *testing.T) {
			fixture, identity := installedSandboxExecutionFixture(t)
			policy := filepath.Join(fixture.profileStore, "sha256", strings.TrimPrefix(identity, "sha256:"), "rootfs", "system-call-policy.json")
			switch change {
			case "contents":
				if err := os.WriteFile(policy, []byte("{}\n"), 0o444); err != nil {
					t.Fatal(err)
				}
			case "missing", "symlink":
				if err := os.Remove(policy); err != nil {
					t.Fatal(err)
				}
				if change == "symlink" {
					if err := os.Symlink("../system-call-policy.json", policy); err != nil {
						t.Fatal(err)
					}
				}
			case "writable":
				if err := os.Chmod(policy, 0o644); err != nil {
					t.Fatal(err)
				}
			case "owner":
				if err := os.Chown(policy, 1000, 1000); err != nil {
					t.Fatal(err)
				}
			}
			t.Cleanup(startSandboxSupervisor(t, fixture,
				"--subuid-start", "200000", "--subgid-start", "300000", "--subid-count", "65536",
				"--cgroup-root", os.Getenv("SANDBOX_CGROUP_ROOT")))
			assertSandboxSupervisorErrorCode(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath,
				createSandboxWireRequest(t, "create", "run-policy", identity)), "invalid_reference")
			assertNoResourceCgroups(t)
		})
	}
}

func TestSandboxExecutionSecurityEnforcesSystemCallPolicy(t *testing.T) {
	fixture, identity := installedSandboxExecutionFixture(t)
	t.Cleanup(startSandboxSupervisor(t, fixture,
		"--subuid-start", "200000", "--subgid-start", "300000", "--subid-count", "65536",
		"--cgroup-root", os.Getenv("SANDBOX_CGROUP_ROOT")))
	assertSandboxCreated(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath,
		createSandboxWireRequest(t, "create", "run-syscalls", identity)), "run-syscalls")
	result := executeSandboxPython(t, fixture, "run-syscalls", "probe", `import ctypes, errno
status = dict(line.split(':', 1) for line in open('/proc/self/status'))
assert status['Seccomp'].strip() == '2', status['Seccomp']
libc = ctypes.CDLL(None, use_errno=True)
libc.syscall.restype = ctypes.c_long
# getpriority is harmless without filtering, but unnecessary for this Profile.
# Its EPERM demonstrates policy enforcement independently of capability/DAC denial.
assert libc.syscall(140, 0, 0) == -1
assert ctypes.get_errno() == errno.EPERM, ctypes.get_errno()
print('policy-enforced')`)
	if result.ExitCode != 0 || result.Stdout != "policy-enforced\n" {
		t.Fatalf("Workload did not enforce the System Call Policy: %+v", result)
	}
	clean := executeSandboxPython(t, fixture, "run-syscalls", "after-denial", "print('still-usable')")
	if clean.ExitCode != 0 || clean.Stdout != "still-usable\n" {
		t.Fatalf("System Call Policy denial poisoned the Sandbox: %+v", clean)
	}
}
