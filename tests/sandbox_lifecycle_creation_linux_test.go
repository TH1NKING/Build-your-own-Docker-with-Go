//go:build linux && sandbox_root && profilebundle_root

package workspace_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/TH1NKING/Build-your-own-Docker-with-Go/internal/sandboxsupervisor"
)

func TestSandboxLifecycleUnreadyInitReleasesCreation(t *testing.T) {
	for _, scenario := range []string{"client-disconnect", "readiness-timeout"} {
		t.Run(scenario, func(t *testing.T) {
			// Install a separately digest-verified test Init that keeps its ready
			// pipe open but never announces readiness. Production entrypoints and
			// the real Init used by all other tests remain unchanged.
			waitingInit := os.Getenv("SANDBOX_WAITING_INIT_CLI")
			if waitingInit == "" {
				t.Fatal("SANDBOX_WAITING_INIT_CLI must name the prebuilt waiting Init fixture")
			}
			t.Setenv("SANDBOX_INIT_CLI", waitingInit)
			t.Setenv("SANDBOX_ROOT_PROBE", waitingInit)
			fixture, identity := installedSandboxCreationFixture(t)
			installedInit, err := os.Stat(filepath.Join(fixture.profileStore, "sha256", strings.TrimPrefix(identity, "sha256:"), "rootfs", "sandbox-init"))
			if err != nil {
				t.Fatal(err)
			}
			unrelated := filepath.Join(fixture.sandboxRoot, "run-unrelated")
			if err := os.Mkdir(unrelated, 0o755); err != nil {
				t.Fatal(err)
			}
			sentinel := filepath.Join(unrelated, "sentinel")
			if err := os.WriteFile(sentinel, []byte("belongs to another creation"), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(startSandboxSupervisor(t, fixture,
				"--subuid-start", "200000", "--subgid-start", "300000", "--subid-count", "65536"))
			mountsBefore, err := os.ReadFile("/proc/self/mountinfo")
			if err != nil {
				t.Fatal(err)
			}

			client := sandboxsupervisor.NewClient(fixture.socketPath)
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			type outcome struct {
				response sandboxsupervisor.CreateSandboxResponse
				err      error
			}
			finished := make(chan outcome, 1)
			id := "run-" + scenario
			started := time.Now()
			go func() {
				response, err := client.CreateSandbox(ctx, sandboxsupervisor.CreateSandboxRequest{
					RequestID: "create-" + id, SandboxID: id, ProfileIdentity: identity,
				})
				finished <- outcome{response, err}
			}()

			// Host kernel observations ensure cancellation occurs after bootstrap
			// has exec'd the installed Init and is waiting for its readiness byte.
			for {
				waiting := false
				for _, pid := range sandboxProcessesForSubordinateMapping(t, 200000, 65536) {
					image, err := os.Stat(filepath.Join("/proc", pid, "exe"))
					if err == nil && os.SameFile(image, installedInit) {
						waiting = true
						break
					}
				}
				if waiting {
					break
				}
				select {
				case result := <-finished:
					t.Fatalf("creation completed before the waiting Init started: %+v, %v", result.response, result.err)
				default:
				}
				if time.Since(started) > 2*time.Second {
					t.Fatal("waiting Init did not start within two seconds")
				}
				time.Sleep(10 * time.Millisecond)
			}

			if scenario == "client-disconnect" {
				cleanupDeadline := time.Now().Add(3 * time.Second)
				cancel()
				select {
				case result := <-finished:
					if result.err == nil || result.response.Result != nil {
						t.Fatalf("disconnected creation returned success: %+v, %v", result.response, result.err)
					}
				case <-time.After(time.Until(cleanupDeadline)):
					t.Fatal("cancelled client remained blocked on creation")
				}
				for {
					pids := sandboxProcessesForSubordinateMapping(t, 200000, 65536)
					_, err := os.Lstat(filepath.Join(fixture.sandboxRoot, id))
					if len(pids) == 0 && os.IsNotExist(err) {
						break
					}
					if time.Now().After(cleanupDeadline) {
						t.Fatalf("disconnected creation retained Init or its directory: pids=%v, directory=%v", pids, err)
					}
					time.Sleep(10 * time.Millisecond)
				}
			} else {
				select {
				case result := <-finished:
					if result.err != nil || result.response.Result != nil || result.response.Error == nil || result.response.Error.Code != sandboxsupervisor.ErrorCodeCreationFailed {
						t.Fatalf("unready Init returned false completion: %+v, %v", result.response, result.err)
					}
					if time.Since(started) < 3*time.Second {
						t.Fatal("creation failed before the three-second readiness guard elapsed")
					}
				case <-time.After(5 * time.Second):
					t.Fatal("creation did not enforce its readiness guard")
				}
			}
			assertSandboxLifecycleCreationResourcesReleased(t, fixture, id, mountsBefore, sentinel)
		})
	}
}

func TestSandboxLifecycleCreationFailureReleasesOnlyAcquiredResources(t *testing.T) {
	// Each missing installer-owned mountpoint makes the real kernel reject a
	// later acquisition: proc after the Profile bind, Workspace after pivot_root,
	// and temporary storage after the Workspace tmpfs. A noexec Profile rejects
	// the final Init exec after all mounts succeed. Init remains unmodified.
	for _, stage := range []string{"proc", "workspace", "tmp", "init-exec"} {
		t.Run(stage, func(t *testing.T) {
			fixture, identity := installedSandboxExecutionFixture(t)
			rootfs := filepath.Join(fixture.profileStore, "sha256", strings.TrimPrefix(identity, "sha256:"), "rootfs")
			repair := injectSandboxLifecycleCreationFailure(t, rootfs, stage)

			unrelated := filepath.Join(fixture.sandboxRoot, "run-unrelated")
			if err := os.Mkdir(unrelated, 0o755); err != nil {
				t.Fatal(err)
			}
			sentinel := filepath.Join(unrelated, "sentinel")
			if err := os.WriteFile(sentinel, []byte("belongs to another creation"), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(startSandboxSupervisor(t, fixture,
				"--subuid-start", "200000", "--subgid-start", "300000", "--subid-count", "65536",
				"--cgroup-root", os.Getenv("SANDBOX_CGROUP_ROOT")))
			mountsBefore, err := os.ReadFile("/proc/self/mountinfo")
			if err != nil {
				t.Fatal(err)
			}

			failedID := "run-failed-" + stage
			response := exchangeSandboxSupervisorMessage(t, fixture.socketPath,
				createSandboxWireRequest(t, "create-"+failedID, failedID, identity))
			assertSandboxSupervisorErrorCode(t, response, "creation_failed")
			assertSandboxLifecycleCreationResourcesReleased(t, fixture, failedID, mountsBefore, sentinel)

			if err := repair(); err != nil {
				t.Fatal("repair Profile fixture:", err)
			}
			// Removing the test's noexec bind intentionally changes its mount
			// table. Compare subsequent lifecycle behavior to the repaired host.
			mountsBefore, err = os.ReadFile("/proc/self/mountinfo")
			if err != nil {
				t.Fatal(err)
			}
			// The sole subordinate-ID block must be reusable after rollback.
			// A fresh identifier cannot accidentally attach to the failed attempt.
			replacementID := "run-after-" + stage
			assertSandboxCreated(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath,
				createSandboxWireRequest(t, "create-"+replacementID, replacementID, identity)), replacementID)
			assertSandboxDestroyed(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath,
				destroySandboxWireRequest(t, replacementID)), replacementID)
			assertSandboxLifecycleCreationResourcesReleased(t, fixture, replacementID, mountsBefore, sentinel)
		})
	}
}

func injectSandboxLifecycleCreationFailure(t *testing.T, rootfs, stage string) func() error {
	t.Helper()
	if stage == "init-exec" {
		// The outer acceptance harness already has private mount propagation.
		// Stack a bind only over this fixture; never remount its backing filesystem.
		mounted := false
		repair := func() error {
			if !mounted {
				return nil
			}
			if err := syscall.Unmount(rootfs, 0); err != nil {
				return err
			}
			mounted = false
			return nil
		}
		t.Cleanup(func() {
			if err := repair(); err != nil {
				t.Errorf("remove fixture noexec mount: %v", err)
			}
		})
		if err := syscall.Mount(rootfs, rootfs, "", syscall.MS_BIND, ""); err != nil {
			t.Fatal("bind fixture Profile:", err)
		}
		mounted = true
		if err := syscall.Mount("", rootfs, "", syscall.MS_BIND|syscall.MS_REMOUNT|syscall.MS_NOEXEC|syscall.MS_NOSUID|syscall.MS_NODEV, ""); err != nil {
			t.Fatal("make only the fixture Profile noexec:", err)
		}
		return repair
	}

	mountpoint := filepath.Join(rootfs, stage)
	held := filepath.Join(rootfs, stage+"-held-by-test")
	if err := os.Rename(mountpoint, held); err != nil {
		t.Fatal("remove fixture mountpoint:", err)
	}
	restored := false
	repair := func() error {
		if restored {
			return nil
		}
		if err := os.Rename(held, mountpoint); err != nil {
			return err
		}
		restored = true
		return nil
	}
	t.Cleanup(func() {
		if err := repair(); err != nil {
			t.Errorf("restore fixture mountpoint: %v", err)
		}
	})
	return repair
}

func assertSandboxLifecycleCreationResourcesReleased(t *testing.T, fixture sandboxSupervisorFixture, id string, mountsBefore []byte, sentinel string) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(fixture.sandboxRoot, id)); !os.IsNotExist(err) {
		t.Fatalf("terminal creation retained its runtime directory: %v", err)
	}
	// A failed bootstrap may not have reached pivot_root. Inspect its kernel
	// identity mapping rather than only processes already rooted in the Profile.
	if pids := sandboxProcessesForSubordinateMapping(t, 200000, 65536); len(pids) != 0 {
		t.Fatalf("terminal creation retained processes in its User Namespace: %v", pids)
	}
	mountsAfter, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(mountsBefore, mountsAfter) {
		t.Fatal("Sandbox creation or cleanup changed the host mount table")
	}
	if contents, err := os.ReadFile(sentinel); err != nil || string(contents) != "belongs to another creation" {
		t.Fatalf("rollback modified unrelated runtime state: %q, %v", contents, err)
	}
	if entries, err := os.ReadDir(fixture.sandboxRoot); err != nil || len(entries) != 1 || entries[0].Name() != "run-unrelated" {
		t.Fatalf("terminal creation left unexpected runtime entries: %v, %v", entries, err)
	}
}

func sandboxProcessesForSubordinateMapping(t *testing.T, firstUID, count uint) []string {
	t.Helper()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatal(err)
	}
	expected := fmt.Sprintf("0 %d %d", firstUID, count)
	var pids []string
	for _, entry := range entries {
		if _, err := strconv.Atoi(entry.Name()); err != nil {
			continue
		}
		mapping, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "uid_map"))
		if os.IsNotExist(err) || errors.Is(err, syscall.ESRCH) {
			continue // Processes outside the Sandbox may exit during observation.
		}
		if err != nil {
			t.Fatalf("observe process %s User Namespace mapping: %v", entry.Name(), err)
		}
		if strings.Join(strings.Fields(string(mapping)), " ") == expected {
			pids = append(pids, entry.Name())
		}
	}
	return pids
}
