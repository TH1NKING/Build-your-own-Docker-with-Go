//go:build linux && sandbox_root && profilebundle_root

package workspace_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// This suite drives the published Supervisor protocol. /proc observations are
// host security invariants, not a dependency on Supervisor implementation APIs.
func TestSandboxCreationMapsOnlyConfiguredSubordinateIDs(t *testing.T) {
	fixture, identity := installedSandboxCreationFixture(t)
	t.Cleanup(startSandboxSupervisor(t, fixture,
		"--subuid-start", "200000", "--subgid-start", "300000", "--subid-count", "131072"))
	response := exchangeSandboxSupervisorMessage(t, fixture.socketPath,
		createSandboxWireRequest(t, "req-create", "run-first", identity))
	assertSandboxCreated(t, response, "run-first")
	pid := sandboxProcessForRoot(t, fixture, identity)
	for name, want := range map[string]string{
		"uid_map": "0 200000 65536",
		"gid_map": "0 300000 65536",
	} {
		contents, err := os.ReadFile(filepath.Join("/proc", pid, name))
		if err != nil || strings.Join(strings.Fields(string(contents)), " ") != want {
			t.Fatalf("%s = %q, err=%v; want %s", name, contents, err, want)
		}
	}
	status, err := os.ReadFile(filepath.Join("/proc", pid, "status"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(status), "\n") {
		if strings.HasPrefix(line, "Groups:") && strings.TrimSpace(strings.TrimPrefix(line, "Groups:")) != "" {
			t.Fatalf("Sandbox retained supplementary host groups: %s", line)
		}
	}
}

func TestSandboxCreationDetachesHostRootAndKeepsProfileReadOnly(t *testing.T) {
	fixture, identity := installedSandboxCreationFixture(t)
	marker := filepath.Join(filepath.Dir(fixture.sandboxRoot), "host-only-marker")
	if err := os.WriteFile(marker, []byte("host secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A world-traversable ancestor makes denial independent of host DAC.
	if err := os.Chmod(filepath.Dir(fixture.sandboxRoot), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(startSandboxSupervisor(t, fixture,
		"--subuid-start", "200000", "--subgid-start", "300000", "--subid-count", "65536"))
	response := exchangeSandboxSupervisorMessage(t, fixture.socketPath,
		createSandboxWireRequest(t, "req-root", "run-root", identity))
	assertSandboxCreated(t, response, "run-root")
	pid := sandboxProcessForRoot(t, fixture, identity)

	for _, name := range []string{"user", "mnt", "pid", "net"} {
		sandbox, err := os.Stat(filepath.Join("/proc", pid, "ns", name))
		if err != nil {
			t.Fatal(err)
		}
		host, err := os.Stat(filepath.Join("/proc/self/ns", name))
		if err != nil || os.SameFile(sandbox, host) {
			t.Fatalf("Sandbox shares host %s namespace: %v", name, err)
		}
	}
	mountinfo, err := os.ReadFile(filepath.Join("/proc", pid, "mountinfo"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(mountinfo)), "\n")
	allowedMounts := map[string]bool{"/": false, "/proc": false, "/workspace": false, "/tmp": false}
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) < 10 {
			t.Fatalf("invalid mount information: %s", line)
		}
		if _, ok := allowedMounts[fields[4]]; !ok || allowedMounts[fields[4]] || strings.Contains(line, "shared:") || strings.Contains(line, "master:") {
			t.Fatalf("unexpected or non-private Sandbox mount: %s", line)
		}
		allowedMounts[fields[4]] = true
		if fields[4] == "/" && !strings.Contains(","+fields[5]+",", ",ro,") {
			t.Fatalf("root mount is writable: %s", line)
		}
	}
	for mountpoint, seen := range allowedMounts {
		if !seen {
			t.Fatalf("missing Sandbox mount %s", mountpoint)
		}
	}
	fdDirectory := filepath.Join("/proc", pid, "fd")
	fds, err := os.ReadDir(fdDirectory)
	if err != nil {
		t.Fatal(err)
	}
	for _, fd := range fds {
		target, err := os.Readlink(filepath.Join(fdDirectory, fd.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(target, "anon_inode:") {
			continue
		}
		info, err := os.Stat(filepath.Join(fdDirectory, fd.Name()))
		if err == nil && (info.IsDir() || info.Mode().IsRegular()) {
			t.Fatalf("Sandbox retains host filesystem descriptor %s (%s): %v", fd.Name(), target, info.Mode())
		}
	}
	command := exec.Command("nsenter", "--target", pid, "--user", "--mount", "--pid", "--net", "--root", "--wd=/",
		"--", "/opt/python/bin/python3", marker)
	if output, err := command.CombinedOutput(); err != nil || string(output) != "host-root-unreachable; profile-read-only\n" {
		t.Fatalf("kernel Workload probe failed: %v\n%s", err, output)
	}
	// Parent mount namespace and selected Profile contents remain intact.
	if content, err := os.ReadFile(marker); err != nil || string(content) != "host secret" {
		t.Fatalf("host marker changed: %q, %v", content, err)
	}
	var hostFilesystem syscall.Statfs_t
	if err := syscall.Statfs(filepath.Dir(marker), &hostFilesystem); err != nil || hostFilesystem.Flags&syscall.MS_RDONLY != 0 {
		t.Fatalf("Sandbox read-only remount propagated to the host: %v", err)
	}
}

func assertSandboxCreated(t *testing.T, response []byte, id string) {
	t.Helper()
	var envelope struct {
		Result *struct {
			SandboxID string `json:"sandbox_id"`
		} `json:"result"`
		Error any `json:"error"`
	}
	if err := json.Unmarshal(response, &envelope); err != nil || envelope.Result == nil || envelope.Result.SandboxID != id || envelope.Error != nil {
		t.Fatalf("create returned %s, want confirmed Sandbox %s (decode: %v)", response, id, err)
	}
}

func TestSandboxCreationRejectsProfileOutsideInstallerOwnership(t *testing.T) {
	fixture, identity := installedSandboxCreationFixture(t)
	profileDirectory := filepath.Join(fixture.profileStore, "sha256", strings.TrimPrefix(identity, "sha256:"))
	// An unprivileged owner could replace rootfs after selection. Valid rootfs
	// permissions alone cannot make the containing Profile trustworthy.
	if err := os.Chown(profileDirectory, 12345, 12345); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(startSandboxSupervisor(t, fixture,
		"--subuid-start", "200000", "--subgid-start", "300000", "--subid-count", "65536"))
	response := exchangeSandboxSupervisorMessage(t, fixture.socketPath,
		createSandboxWireRequest(t, "req-untrusted-profile", "run-untrusted", identity))
	assertSandboxSupervisorErrorCode(t, response, "invalid_reference")
}

func TestSandboxCreationAllocatesDistinctIdentitiesConcurrently(t *testing.T) {
	fixture, identity := installedSandboxCreationFixture(t)
	t.Cleanup(startSandboxSupervisor(t, fixture,
		"--subuid-start", "200000", "--subgid-start", "300000", "--subid-count", "131072"))
	var clients sync.WaitGroup
	for _, id := range []string{"run-one", "run-two"} {
		clients.Go(func() {
			response := exchangeSandboxSupervisorMessage(t, fixture.socketPath, createSandboxWireRequest(t, "req-"+id, id, identity))
			assertSandboxCreated(t, response, id)
		})
	}
	clients.Wait()
	pids := sandboxProcessesForRoot(t, fixture, identity)
	if len(pids) != 2 {
		t.Fatalf("active Sandbox count = %d, want 2", len(pids))
	}
	var mappings []string
	for _, pid := range pids {
		contents, err := os.ReadFile(filepath.Join("/proc", pid, "uid_map"))
		if err != nil {
			t.Fatal(err)
		}
		mappings = append(mappings, strings.Join(strings.Fields(string(contents)), " "))
	}
	sort.Strings(mappings)
	if strings.Join(mappings, ";") != "0 200000 65536;0 265536 65536" {
		t.Fatalf("concurrent Sandbox UID maps overlap or leave configured range: %v", mappings)
	}
	response := exchangeSandboxSupervisorMessage(t, fixture.socketPath, createSandboxWireRequest(t, "req-full", "run-three", identity))
	assertSandboxSupervisorErrorCode(t, response, "identity_range_exhausted")
	response = exchangeSandboxSupervisorMessage(t, fixture.socketPath, createSandboxWireRequest(t, "req-duplicate", "run-one", identity))
	assertSandboxSupervisorErrorCode(t, response, "sandbox_exists")
	if pids := sandboxProcessesForRoot(t, fixture, identity); len(pids) != 2 {
		t.Fatalf("rejected creation disturbed existing Sandboxes: %v", pids)
	}
}

func TestSandboxCreationRejectsUnsafeSubordinateConfiguration(t *testing.T) {
	fixture := newSafeSandboxSupervisorFixture(t)
	for _, arguments := range [][]string{
		{"--subuid-start", "0", "--subgid-start", "300000", "--subid-count", "65536"},
		{"--subuid-start", "200000", "--subgid-start", "0", "--subid-count", "65536"},
		{"--subuid-start", "200000", "--subgid-start", "300000"},
		{"--subuid-start", "200000", "--subgid-start", "300000", "--subid-count", "1000"},
		{"--subuid-start", "4294960000", "--subgid-start", "300000", "--subid-count", "65536"},
		{"--subuid-start", "200000", "--subgid-start", "4294960000", "--subid-count", "65536"},
	} {
		t.Run(strings.Join(arguments, "_"), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, sandboxdCommandPath(t), "--socket", fixture.socketPath, "--profile-store", fixture.profileStore, "--sandbox-root", fixture.sandboxRoot)
			command.Args = append(command.Args, arguments...)
			if output, err := command.CombinedOutput(); err == nil || ctx.Err() != nil {
				t.Fatalf("unsafe subordinate mapping accepted: %v\n%s", arguments, output)
			}
			if _, err := os.Lstat(fixture.socketPath); !os.IsNotExist(err) {
				t.Fatalf("invalid mapping published socket: %v", err)
			}
		})
	}
}

func TestSandboxCreationPreservesPreexistingSandboxDirectory(t *testing.T) {
	fixture, identity := installedSandboxCreationFixture(t)
	existing := filepath.Join(fixture.sandboxRoot, "run-existing")
	if err := os.Mkdir(existing, 0o755); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(existing, "sentinel")
	if err := os.WriteFile(sentinel, []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(startSandboxSupervisor(t, fixture,
		"--subuid-start", "200000", "--subgid-start", "300000", "--subid-count", "65536"))
	response := exchangeSandboxSupervisorMessage(t, fixture.socketPath, createSandboxWireRequest(t, "req-existing", "run-existing", identity))
	assertSandboxSupervisorErrorCode(t, response, "sandbox_exists")
	if contents, err := os.ReadFile(sentinel); err != nil || string(contents) != "preserve" {
		t.Fatalf("pre-existing state modified: %q, %v", contents, err)
	}
	response = exchangeSandboxSupervisorMessage(t, fixture.socketPath, createSandboxWireRequest(t, "req-after-conflict", "run-new", identity))
	assertSandboxCreated(t, response, "run-new")
}

func TestSandboxCreationRejectsRootFilesystemSymlink(t *testing.T) {
	fixture, identity := installedSandboxCreationFixture(t)
	profile := filepath.Join(fixture.profileStore, "sha256", strings.TrimPrefix(identity, "sha256:"))
	rootfs := filepath.Join(profile, "rootfs")
	if err := os.Rename(rootfs, rootfs+"-original"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("rootfs-original", rootfs); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(startSandboxSupervisor(t, fixture,
		"--subuid-start", "200000", "--subgid-start", "300000", "--subid-count", "65536"))
	response := exchangeSandboxSupervisorMessage(t, fixture.socketPath, createSandboxWireRequest(t, "req-link", "run-link", identity))
	assertSandboxSupervisorErrorCode(t, response, "invalid_reference")
}

func TestSandboxCreationRejectsChangedOrLinkedInit(t *testing.T) {
	for _, mutation := range []string{"changed-bytes", "symlink"} {
		t.Run(mutation, func(t *testing.T) {
			fixture, identity := installedSandboxCreationFixture(t)
			rootfs := filepath.Join(fixture.profileStore, "sha256", strings.TrimPrefix(identity, "sha256:"), "rootfs")
			initPath := filepath.Join(rootfs, "sandbox-init")
			if mutation == "symlink" {
				if err := os.Remove(initPath); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("../sandbox-init", initPath); err != nil {
					t.Fatal(err)
				}
			} else {
				file, err := os.OpenFile(initPath, os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				_, err = file.WriteAt([]byte("BAD!"), 0)
				file.Close()
				if err != nil {
					t.Fatal(err)
				}
			}
			t.Cleanup(startSandboxSupervisor(t, fixture, "--subuid-start", "200000", "--subgid-start", "300000", "--subid-count", "65536"))
			response := exchangeSandboxSupervisorMessage(t, fixture.socketPath, createSandboxWireRequest(t, "req-tampered", "run-tampered", identity))
			assertSandboxSupervisorErrorCode(t, response, "invalid_reference")
		})
	}
}

func TestSandboxCreationFailsClosedWhenKernelRejectsIdentityMapping(t *testing.T) {
	fixture, identity := installedSandboxCreationFixture(t)
	// An outer User Namespace containing only ID zero cannot grant the child
	// the requested subordinate range. The real kernel rejects map creation;
	// no fault-injection hook is added to the Supervisor.
	fixture.launchPrefix = []string{"unshare", "--user", "--map-root-user"}
	t.Cleanup(startSandboxSupervisor(t, fixture,
		"--subuid-start", "200000", "--subgid-start", "300000", "--subid-count", "65536"))
	for _, requestID := range []string{"req-denied", "req-retry-denied"} {
		response := exchangeSandboxSupervisorMessage(t, fixture.socketPath, createSandboxWireRequest(t, requestID, "run-denied", identity))
		assertSandboxSupervisorErrorCode(t, response, "creation_failed")
		if entries, err := os.ReadDir(fixture.sandboxRoot); err != nil || len(entries) != 0 {
			t.Fatalf("failed creation retained runtime state: %v, %v", entries, err)
		}
		if strings.Contains(string(response), fixture.profileStore) {
			t.Fatalf("failure disclosed host path: %s", response)
		}
	}
}

func TestSandboxCreationDoesNotInheritLauncherHostDirectory(t *testing.T) {
	fixture, identity := installedSandboxCreationFixture(t)
	fixture.launchPrefix = []string{"bash", "-c", `exec 9<"$1"; exec "${@:2}"`, "sandboxd-inherited-fd", filepath.Dir(fixture.sandboxRoot)}
	t.Cleanup(startSandboxSupervisor(t, fixture,
		"--subuid-start", "200000", "--subgid-start", "300000", "--subid-count", "65536"))
	response := exchangeSandboxSupervisorMessage(t, fixture.socketPath, createSandboxWireRequest(t, "req-inherited-fd", "run-inherited-fd", identity))
	assertSandboxCreated(t, response, "run-inherited-fd")
	pid := sandboxProcessForRoot(t, fixture, identity)
	entries, err := os.ReadDir(filepath.Join("/proc", pid, "fd"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		info, err := os.Stat(filepath.Join("/proc", pid, "fd", entry.Name()))
		if err == nil && info.IsDir() {
			t.Fatalf("Sandbox retained launcher host directory at FD %s", entry.Name())
		}
	}
}

func installedSandboxCreationFixture(t *testing.T) (sandboxSupervisorFixture, string) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Fatal("sandbox_root tests require root inside a disposable Linux mount/PID/network namespace")
	}
	cli := os.Getenv("PROFILE_BUNDLE_CLI")
	if cli == "" {
		t.Fatal("PROFILE_BUNDLE_CLI must name the prebuilt installer")
	}
	fixture := newSafeSandboxSupervisorFixture(t)
	if err := os.Chmod(filepath.Join(fixture.profileStore, "sha256"), 0o755); err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(filepath.Dir(fixture.sandboxRoot), "fixture.bundle")
	probe, err := os.ReadFile(os.Getenv("SANDBOX_ROOT_PROBE"))
	if err != nil {
		t.Fatalf("SANDBOX_ROOT_PROBE must name the prebuilt conformance fixture: %v", err)
	}
	rootfs := writeIndependentRootFSFixture(t, "opt/python/bin/python3", probe)
	init, err := os.ReadFile(os.Getenv("SANDBOX_INIT_CLI"))
	if err != nil {
		t.Fatalf("SANDBOX_INIT_CLI must name the real Init: %v", err)
	}
	digest := writeIndependentBundleFixtureWithRootFSAndInit(t, bundle, validFixtureLock(), validFixturePolicy(), rootfs, init)
	command := exec.Command(cli, "install", "--bundle", bundle,
		"--expected-sha256", digest, "--store", fixture.profileStore)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("install real Profile Bundle: %v\n%s", err, output)
	}
	return fixture, "sha256:" + digest
}

func sandboxProcessForRoot(t *testing.T, fixture sandboxSupervisorFixture, identity string) string {
	t.Helper()
	pids := sandboxProcessesForRoot(t, fixture, identity)
	if len(pids) != 1 {
		t.Fatalf("want one live process rooted in Profile, got %v", pids)
	}
	return pids[0]
}

func sandboxProcessesForRoot(t *testing.T, fixture sandboxSupervisorFixture, identity string) []string {
	t.Helper()
	root, err := os.Stat(filepath.Join(fixture.profileStore, "sha256", strings.TrimPrefix(identity, "sha256:"), "rootfs"))
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatal(err)
	}
	var pids []string
	for _, entry := range entries {
		info, err := os.Stat(filepath.Join("/proc", entry.Name(), "root"))
		if err == nil && os.SameFile(info, root) {
			pids = append(pids, entry.Name())
		}
	}
	return pids
}
