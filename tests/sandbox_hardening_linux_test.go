//go:build linux && sandbox_root && profilebundle_root

package workspace_test

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// This is a production ExecutePython request. It must fail against the old
// layout, which has no /dev and exposes writable, unmasked proc metadata.
func TestSandboxExecutionHardeningExposesOnlyRequiredKernelViews(t *testing.T) {
	fixture, identity := installedSandboxExecutionFixture(t)
	t.Cleanup(startSandboxSupervisor(t, fixture,
		"--subuid-start", "200000", "--subgid-start", "300000", "--subid-count", "65536",
		"--cgroup-root", os.Getenv("SANDBOX_CGROUP_ROOT")))
	assertSandboxCreated(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath,
		createSandboxWireRequest(t, "create", "run-kernel-views", identity)), "run-kernel-views")
	result := executeSandboxPython(t, fixture, "run-kernel-views", "probe", `import errno, os, stat, subprocess, sys
assert sorted(os.listdir('/dev')) == ['null', 'zero']
for name, minor in [('null', 3), ('zero', 5)]:
    device = os.stat('/dev/' + name)
    assert stat.S_ISCHR(device.st_mode)
    assert (os.major(device.st_rdev), os.minor(device.st_rdev)) == (1, minor)
with open('/dev/null', 'wb') as null: assert null.write(b'discarded') == 9
with open('/dev/null', 'rb') as null: assert null.read(1) == b''
with open('/dev/zero', 'rb') as zero: assert zero.read(32) == bytes(32)
subprocess.run([sys.executable, '-I', '-B', '-c', 'print(42)'], stdout=subprocess.DEVNULL, check=True)
for directory in ['sys', 'irq', 'bus', 'fs', 'acpi', 'scsi']:
    path = '/proc/' + directory
    if os.path.exists(path): assert os.listdir(path) == [], path
for name in ['kcore', 'keys', 'timer_list', 'sysrq-trigger', 'interrupts']:
    path = '/proc/' + name
    if os.path.exists(path):
        with open(path, 'rb') as masked: assert masked.read(1) == b'', path
assert not os.path.exists('/sys')
assert not os.path.exists('/dev/pts')
assert not os.path.exists('/.oldroot')
assert not os.path.exists('/proc/1/root/sys')
assert set(name for name in os.listdir('/proc') if name.isdigit()) == {'1', str(os.getpid())}
mounts = [line.split() for line in open('/proc/self/mountinfo')]
proc = [entry for entry in mounts if entry[4] == '/proc']
assert len(proc) == 1 and 'ro' in proc[0][5].split(',')
for path in ['/dev/new-device', '/proc/new-file']:
    try: open(path, 'wb')
    except OSError as error: assert error.errno in [errno.EROFS, errno.EACCES, errno.ENOENT]
    else: raise AssertionError('writable kernel view ' + path)
print('minimal-devices; namespace-processes; read-only-masked-proc')`)
	if result.ExitCode != 0 || result.Stdout != "minimal-devices; namespace-processes; read-only-masked-proc\n" {
		t.Fatalf("Sandbox kernel views do not satisfy the production boundary: %+v", result)
	}
}

func TestSandboxExecutionHardeningDoesNotInheritHostFilesOrSockets(t *testing.T) {
	fixture, identity := installedSandboxExecutionFixture(t)
	fixture.launchPrefix = []string{os.Args[0], "-test.run=^TestSandboxHardeningHostDescriptorLauncher$", "--", "--sandbox-host-descriptor-fixture", filepath.Dir(fixture.sandboxRoot)}
	t.Cleanup(startSandboxSupervisor(t, fixture,
		"--subuid-start", "200000", "--subgid-start", "300000", "--subid-count", "65536",
		"--cgroup-root", os.Getenv("SANDBOX_CGROUP_ROOT")))
	assertSandboxCreated(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath,
		createSandboxWireRequest(t, "create", "run-host-fds", identity)), "run-host-fds")
	pid := sandboxProcessForRoot(t, fixture, identity)
	entries, err := os.ReadDir(filepath.Join("/proc", pid, "fd"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		name := filepath.Join("/proc", pid, "fd", entry.Name())
		target, err := os.Readlink(name)
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(target, "anon_inode:") {
			continue // Go's own eventpoll/eventfd are not host filesystem access.
		}
		info, err := os.Stat(name)
		if err != nil {
			t.Fatal(err)
		}
		if info.IsDir() || info.Mode().IsRegular() || info.Mode()&os.ModeSocket != 0 {
			t.Fatalf("trusted Init retained a host file/directory/socket at FD %s: %s", entry.Name(), target)
		}
	}
	result := executeSandboxPython(t, fixture, "run-host-fds", "probe", `import errno, os
# listdir's own directory FD has closed before this loop. Check all observed
# descriptor numbers, including high descriptors, rather than a fixed range.
for name in os.listdir('/proc/self/fd'):
    fd = int(name)
    if fd < 3: continue
    try: os.fstat(fd)
    except OSError as error: assert error.errno == errno.EBADF
    else: raise AssertionError('inherited host or control descriptor %d' % fd)
print('only-workload-stdio')`)
	if result.ExitCode != 0 || result.Stdout != "only-workload-stdio\n" {
		t.Fatalf("Workload inherited Supervisor or Init authority: %+v", result)
	}
}

// This host-only launcher deliberately execs sandboxd with non-CLOEXEC host
// descriptors. It is never installed in a Profile Bundle or callable through
// the production protocol.
func TestSandboxHardeningHostDescriptorLauncher(t *testing.T) {
	if len(os.Args) < 6 || os.Args[3] != "--sandbox-host-descriptor-fixture" {
		return
	}
	directory := os.Args[4]
	dirFD, err := syscall.Open(directory, syscall.O_RDONLY|syscall.O_DIRECTORY, 0)
	if err != nil {
		t.Fatal(err)
	}
	fileFD, err := syscall.Open(filepath.Join(directory, "host-descriptor-sentinel"), syscall.O_CREAT|syscall.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Bind(listener, &syscall.SockaddrUnix{Name: filepath.Join(directory, "host-listener.sock")}); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Listen(listener, 1); err != nil {
		t.Fatal(err)
	}
	connected, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Include a sparse high descriptor to expose implementations that only
	// close the first few conventional descriptor numbers.
	if err := syscall.Dup2(fileFD, 513); err != nil {
		t.Fatal(err)
	}
	for _, fd := range []int{dirFD, fileFD, listener, connected[0], connected[1], 513} {
		flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), syscall.F_GETFD, 0)
		if errno != 0 || flags&syscall.FD_CLOEXEC != 0 {
			t.Fatalf("host descriptor fixture %d is not inheritable: flags=%d errno=%v", fd, flags, errno)
		}
	}
	if err := syscall.Exec(os.Args[5], os.Args[5:], os.Environ()); err != nil {
		t.Fatal(err)
	}
}

func TestSandboxExecutionHardeningNetworkNoneHasNoIngressOrEgress(t *testing.T) {
	fixture, identity := installedSandboxExecutionFixture(t)
	t.Cleanup(startSandboxSupervisor(t, fixture,
		"--subuid-start", "200000", "--subgid-start", "300000", "--subid-count", "65536",
		"--cgroup-root", os.Getenv("SANDBOX_CGROUP_ROOT")))
	assertSandboxCreated(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath,
		createSandboxWireRequest(t, "create", "run-network-none", identity)), "run-network-none")
	pid := sandboxProcessForRoot(t, fixture, identity)
	result := executeSandboxPython(t, fixture, "run-network-none", "probe", `import errno, socket
for family in [socket.AF_INET, socket.AF_INET6, socket.AF_UNIX]:
    for kind in [socket.SOCK_STREAM, socket.SOCK_DGRAM]:
        try: socket.socket(family, kind)
        except OSError as error: assert error.errno == errno.EPERM
        else: raise AssertionError('Workload created network socket')
interfaces = open('/proc/net/dev').readlines()
routes = open('/proc/net/route').readlines()
diagnostic = {'interfaces': interfaces, 'routes': routes}
assert [line.split(':')[0].strip() for line in interfaces[2:]] == ['lo'], diagnostic
# Linux fib_route_seq_start returns no rows, including no header, until the
# main routing table exists. Once it exists, accept its exact header only;
# any route row (even a local or reject route) violates this empty-route test.
header = ['Iface', 'Destination', 'Gateway', 'Flags', 'RefCnt', 'Use', 'Metric', 'Mask', 'MTU', 'Window', 'IRTT']
assert routes == [] or (len(routes) == 1 and routes[0].split() == header), diagnostic
print('network-syscalls-denied; no-routes')`)
	if result.ExitCode != 0 || result.Stdout != "network-syscalls-denied; no-routes\n" {
		t.Fatalf("Workload bypassed the none Network Policy: %+v", result)
	}
	// Prove the network namespace independently of the seccomp filter with
	// a trusted host helper that only enters the Sandbox's net namespace.
	// The outer test harness is already an isolated disposable net namespace.
	loopback, err := net.InterfaceByName("lo")
	if err != nil {
		t.Fatal(err)
	}
	if loopback.Flags&net.FlagUp == 0 {
		if output, err := exec.Command("ip", "link", "set", "lo", "up").CombinedOutput(); err != nil {
			t.Fatalf("bring up disposable test loopback: %v\n%s", err, output)
		}
		t.Cleanup(func() {
			if output, err := exec.Command("ip", "link", "set", "lo", "down").CombinedOutput(); err != nil {
				t.Errorf("restore disposable test loopback: %v\n%s", err, output)
			}
		})
	}
	hostListener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer hostListener.Close()
	hostAddress := hostListener.Addr().String()
	control, err := net.DialTimeout("tcp4", hostAddress, time.Second)
	if err != nil {
		t.Fatal("host positive network control failed:", err)
	}
	control.Close()
	probe := exec.Command("nsenter", "--target", pid, "--net", "--", os.Args[0], "-test.run=^TestSandboxHardeningNetworkProbe$", "--", "--sandbox-network-fixture", "egress", hostAddress)
	if output, err := probe.CombinedOutput(); err != nil || string(output) != "sandbox-egress-unreachable\n" {
		t.Fatalf("trusted netns probe could reach a live host listener: %v\n%s", err, output)
	}
	// Leave no host TCP listener that could be mistaken for the namespace's
	// independently allocated port during the reverse-direction probe.
	if err := hostListener.Close(); err != nil {
		t.Fatal(err)
	}
	ingress := exec.Command("nsenter", "--target", pid, "--net", "--", os.Args[0], "-test.run=^TestSandboxHardeningNetworkProbe$", "--", "--sandbox-network-fixture", "ingress")
	stdout, err := ingress.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	ingress.Stderr = os.Stderr
	if err := ingress.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ingress.Process.Kill(); _ = ingress.Wait() }()
	scanner := bufio.NewScanner(stdout)
	if !scanner.Scan() {
		t.Fatal("trusted netns listener did not publish its listening port:", scanner.Err())
	}
	port, err := strconv.Atoi(scanner.Text())
	if err != nil || port < 1 || port > 65535 {
		t.Fatalf("invalid netns listener port: %q", scanner.Text())
	}
	inbound, err := net.DialTimeout("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 250*time.Millisecond)
	if err == nil {
		inbound.Close()
		t.Fatal("host reached a listener inside the none Network Policy namespace")
	}
	if !scanner.Scan() || scanner.Text() != "sandbox-ingress-unreachable" {
		t.Fatalf("netns listener observed unexpected inbound traffic: %q, %v", scanner.Text(), scanner.Err())
	}
	if err := ingress.Wait(); err != nil {
		t.Fatal("trusted netns listener failed:", err)
	}
	t.Log("positive host TCP control passed; independent netns ingress and egress probes failed as required")
}

func TestSandboxHardeningNetworkProbe(t *testing.T) {
	if len(os.Args) < 5 || os.Args[3] != "--sandbox-network-fixture" {
		return
	}
	interfaces, err := net.Interfaces()
	if err != nil || len(interfaces) != 1 || interfaces[0].Name != "lo" || interfaces[0].Flags&net.FlagUp != 0 {
		t.Fatalf("none Network Policy must have only a down loopback: %v, %v", interfaces, err)
	}
	if os.Args[4] == "egress" && len(os.Args) == 6 {
		connection, err := net.DialTimeout("tcp4", os.Args[5], 250*time.Millisecond)
		if err == nil {
			connection.Close()
			t.Fatal("Sandbox network namespace reached the host")
		}
		fmt.Println("sandbox-egress-unreachable")
		os.Exit(0)
	}
	if os.Args[4] != "ingress" || len(os.Args) != 5 {
		t.Fatal("invalid host network fixture mode")
	}
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4zero})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := listener.SetDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	fmt.Println(listener.Addr().(*net.TCPAddr).Port)
	connection, err := listener.Accept()
	if err == nil {
		connection.Close()
		t.Fatal("Sandbox network namespace accepted an inbound connection")
	}
	if networkError, ok := err.(net.Error); !ok || !networkError.Timeout() {
		t.Fatal("netns listener failed for a reason other than blocked ingress:", err)
	}
	fmt.Println("sandbox-ingress-unreachable")
	os.Exit(0)
}
