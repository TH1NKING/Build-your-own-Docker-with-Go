//go:build linux && profilebundle_root

package workspace_test

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"syscall"
	"testing"
	"time"
)

func TestProfileBundleInstallPublishesVerifiedRootOwnedProfile(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Fatal("profilebundle_root tests must run as root on a disposable Linux environment")
	}
	cliPath := os.Getenv("PROFILE_BUNDLE_CLI")
	if cliPath == "" {
		t.Fatal("PROFILE_BUNDLE_CLI must name the prebuilt profile-bundle command")
	}

	temporaryDirectory := t.TempDir()
	if err := os.Chmod(temporaryDirectory, 0o755); err != nil {
		t.Fatal("make test parent traversable by the non-privileged probe:", err)
	}
	bundlePath := filepath.Join(temporaryDirectory, "fixture.bundle")
	bundleDigest := writeIndependentBundleFixture(t, bundlePath, validFixtureLock(), validFixturePolicy())
	storePath := filepath.Join(temporaryDirectory, "profile-store")
	if err := os.Mkdir(storePath, 0o755); err != nil {
		t.Fatal("create Profile store:", err)
	}

	command := exec.Command(
		cliPath, "install",
		"--bundle", bundlePath,
		"--expected-sha256", bundleDigest,
		"--store", storePath,
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("install valid Profile Bundle: %v\n%s", err, output)
	}

	installedPath := filepath.Join(storePath, "sha256", bundleDigest)
	manifestPath := filepath.Join(installedPath, "manifest.json")
	info, err := os.Stat(manifestPath)
	if err != nil {
		t.Fatal("stat installed manifest:", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatal("installed manifest has no Linux ownership metadata")
	}
	if stat.Uid != 0 || stat.Gid != 0 {
		t.Fatalf("installed manifest ownership = %d:%d, want 0:0", stat.Uid, stat.Gid)
	}
	initPath := filepath.Join(installedPath, "rootfs", "sandbox-init")
	initBytes, err := os.ReadFile(initPath)
	if err != nil || string(initBytes) != "abc" {
		t.Fatalf("installed root Sandbox Init = %q, err=%v, want verified component abc", initBytes, err)
	}
	for _, name := range []string{"sandbox-init", "proc", "workspace", "tmp"} {
		info, err := os.Lstat(filepath.Join(installedPath, "rootfs", name))
		if err != nil {
			t.Fatalf("inspect reserved root path %s: %v", name, err)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != 0 || stat.Gid != 0 || info.Mode().Perm() != 0o555 {
			t.Fatalf("reserved root path %s must be root-owned mode 0555: %#v", name, info)
		}
		if name == "sandbox-init" {
			if !info.Mode().IsRegular() {
				t.Fatal("installed Sandbox Init must be a regular file")
			}
		} else if entries, err := os.ReadDir(filepath.Join(installedPath, "rootfs", name)); err != nil || len(entries) != 0 {
			t.Fatalf("reserved mountpoint %s must be an empty directory: entries=%#v err=%v", name, entries, err)
		}
	}
	policyPath := filepath.Join(installedPath, "rootfs", "system-call-policy.json")
	policyBytes, err := os.ReadFile(policyPath)
	if err != nil || !bytes.Equal(policyBytes, validFixturePolicy()) {
		t.Fatalf("installed root Policy differs from its verified component: %q, %v", policyBytes, err)
	}
	policyInfo, err := os.Lstat(policyPath)
	if err != nil {
		t.Fatal(err)
	}
	policyStat, ok := policyInfo.Sys().(*syscall.Stat_t)
	if !ok || policyStat.Uid != 0 || policyStat.Gid != 0 || !policyInfo.Mode().IsRegular() || policyInfo.Mode().Perm() != 0o444 {
		t.Fatalf("installed Policy must be root-owned mode 0444: %#v", policyInfo)
	}

	writeAttempt := exec.Command(
		"setpriv",
		"--reuid=65534",
		"--regid=65534",
		"--clear-groups",
		"sh", "-c", `printf tampered >> "$1"`, "sh", manifestPath,
	)
	if output, err := writeAttempt.CombinedOutput(); err == nil {
		t.Fatalf("non-privileged process modified installed manifest:\n%s", output)
	}
}

func TestProfileBundleInstallIsIdempotentForTheSameDigest(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Fatal("profilebundle_root tests must run as root on a disposable Linux environment")
	}
	cliPath := os.Getenv("PROFILE_BUNDLE_CLI")
	if cliPath == "" {
		t.Fatal("PROFILE_BUNDLE_CLI must name the prebuilt profile-bundle command")
	}

	temporaryDirectory := t.TempDir()
	bundlePath := filepath.Join(temporaryDirectory, "fixture.bundle")
	bundleDigest := writeIndependentBundleFixture(t, bundlePath, validFixtureLock(), validFixturePolicy())
	storePath := filepath.Join(temporaryDirectory, "profile-store")
	if err := os.Mkdir(storePath, 0o755); err != nil {
		t.Fatal("create Profile store:", err)
	}

	for attempt := 1; attempt <= 2; attempt++ {
		command := exec.Command(
			cliPath, "install",
			"--bundle", bundlePath,
			"--expected-sha256", bundleDigest,
			"--store", storePath,
		)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("install identical Profile Bundle attempt %d: %v\n%s", attempt, err, output)
		}
	}

	installed, err := os.ReadDir(filepath.Join(storePath, "sha256"))
	if err != nil {
		t.Fatal("list content-addressed Profile store:", err)
	}
	if len(installed) != 1 || installed[0].Name() != bundleDigest {
		t.Fatalf("content-addressed Profile store entries = %#v, want only %q", installed, bundleDigest)
	}
}

func TestProfileBundleInstallRejectsReservedSandboxRootPaths(t *testing.T) {
	for _, reserved := range []string{"sandbox-init", "proc", "workspace", "tmp"} {
		for _, kind := range []string{"file", "descendant", "symlink"} {
			t.Run(reserved+"/"+kind, func(t *testing.T) {
				name := reserved
				if kind == "descendant" {
					name += "/payload"
				}
				names := []string{"opt/python/bin/python3", name}
				sort.Strings(names)
				var archive bytes.Buffer
				writer := tar.NewWriter(&archive)
				for _, entryName := range names {
					contents := []byte("fixture")
					header := &tar.Header{
						Name: entryName, Mode: 0o555, Size: int64(len(contents)),
						Uid: 0, Gid: 0, ModTime: time.Unix(0, 0).UTC(),
						Typeflag: tar.TypeReg, Format: tar.FormatPAX,
					}
					if entryName == name && kind == "symlink" {
						header.Typeflag = tar.TypeSymlink
						header.Linkname = "opt"
						header.Size = 0
						contents = nil
					}
					if err := writer.WriteHeader(header); err != nil {
						t.Fatal(err)
					}
					if _, err := writer.Write(contents); err != nil {
						t.Fatal(err)
					}
				}
				if err := writer.Close(); err != nil {
					t.Fatal(err)
				}
				assertRootFSRejectedWithoutPublish(t, archive.Bytes())
			})
		}
	}
}

func TestProfileBundleInstallConcurrentlyPublishesOneIdenticalIdentity(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Fatal("profilebundle_root tests must run as root on a disposable Linux environment")
	}
	cliPath := os.Getenv("PROFILE_BUNDLE_CLI")
	if cliPath == "" {
		t.Fatal("PROFILE_BUNDLE_CLI must name the prebuilt profile-bundle command")
	}
	temporaryDirectory := t.TempDir()
	bundlePath := filepath.Join(temporaryDirectory, "fixture.bundle")
	bundleDigest := writeIndependentBundleFixture(t, bundlePath, validFixtureLock(), validFixturePolicy())
	storePath := filepath.Join(temporaryDirectory, "profile-store")
	if err := os.Mkdir(storePath, 0o755); err != nil {
		t.Fatal("create Profile store:", err)
	}

	commands := make([]*exec.Cmd, 2)
	outputs := make([]bytes.Buffer, len(commands))
	for index := range commands {
		commands[index] = exec.Command(
			cliPath, "install",
			"--bundle", bundlePath,
			"--expected-sha256", bundleDigest,
			"--store", storePath,
		)
		commands[index].Stdout = &outputs[index]
		commands[index].Stderr = &outputs[index]
		if err := commands[index].Start(); err != nil {
			t.Fatalf("start concurrent install %d: %v", index+1, err)
		}
	}
	for index, command := range commands {
		if err := command.Wait(); err != nil {
			t.Fatalf("concurrent install %d: %v\n%s", index+1, err, &outputs[index])
		}
	}
	installed, err := os.ReadDir(filepath.Join(storePath, "sha256"))
	if err != nil {
		t.Fatal("list content-addressed Profile store:", err)
	}
	if len(installed) != 1 || installed[0].Name() != bundleDigest {
		t.Fatalf("concurrent installs published %#v, want only %q", installed, bundleDigest)
	}
	staging, err := os.ReadDir(filepath.Join(storePath, ".staging"))
	if err != nil || len(staging) != 0 {
		t.Fatalf("concurrent installs left staging entries=%#v err=%v", staging, err)
	}
}

func TestProfileBundleInstallRejectsMismatchedSystemCallPolicy(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Fatal("profilebundle_root tests must run as root on a disposable Linux environment")
	}
	cliPath := os.Getenv("PROFILE_BUNDLE_CLI")
	if cliPath == "" {
		t.Fatal("PROFILE_BUNDLE_CLI must name the prebuilt profile-bundle command")
	}

	temporaryDirectory := t.TempDir()
	bundlePath := filepath.Join(temporaryDirectory, "mismatched-policy.bundle")
	mismatchedPolicy := []byte("{\"schema\":\"system-call-policy/v1\",\"profile\":\"another-profile\",\"target\":{\"os\":\"linux\",\"arch\":\"amd64\"},\"default_action\":{\"action\":\"errno\",\"errno\":1},\"rules\":[]}\n")
	bundleDigest := writeIndependentBundleFixture(t, bundlePath, validFixtureLock(), mismatchedPolicy)
	storePath := filepath.Join(temporaryDirectory, "profile-store")
	if err := os.Mkdir(storePath, 0o755); err != nil {
		t.Fatal("create Profile store:", err)
	}

	command := exec.Command(
		cliPath, "install",
		"--bundle", bundlePath,
		"--expected-sha256", bundleDigest,
		"--store", storePath,
	)
	if output, err := command.CombinedOutput(); err == nil {
		t.Fatalf("install accepted a System Call Policy for another profile:\n%s", output)
	}
	if entries, err := os.ReadDir(filepath.Join(storePath, "sha256")); err != nil || len(entries) != 0 {
		t.Fatalf("rejected Policy published a Profile: entries=%#v err=%v", entries, err)
	}
}

func TestProfileBundleInstallRejectsMismatchedBuildLock(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Fatal("profilebundle_root tests must run as root on a disposable Linux environment")
	}
	cliPath := os.Getenv("PROFILE_BUNDLE_CLI")
	if cliPath == "" {
		t.Fatal("PROFILE_BUNDLE_CLI must name the prebuilt profile-bundle command")
	}

	temporaryDirectory := t.TempDir()
	bundlePath := filepath.Join(temporaryDirectory, "mismatched-lock.bundle")
	mismatchedLock := bytes.Replace(validFixtureLock(), []byte(`"profile":"python-data-v1"`), []byte(`"profile":"another-profile"`), 1)
	bundleDigest := writeIndependentBundleFixture(t, bundlePath, mismatchedLock, validFixturePolicy())
	storePath := filepath.Join(temporaryDirectory, "profile-store")
	if err := os.Mkdir(storePath, 0o755); err != nil {
		t.Fatal("create Profile store:", err)
	}

	command := exec.Command(
		cliPath, "install",
		"--bundle", bundlePath,
		"--expected-sha256", bundleDigest,
		"--store", storePath,
	)
	if output, err := command.CombinedOutput(); err == nil {
		t.Fatalf("install accepted a build lock for another profile:\n%s", output)
	}
	if entries, err := os.ReadDir(filepath.Join(storePath, "sha256")); err != nil || len(entries) != 0 {
		t.Fatalf("rejected build lock published a Profile: entries=%#v err=%v", entries, err)
	}
}

func TestProfileBundleInstallRejectsAlteredDeclaredComponent(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Fatal("profilebundle_root tests must run as root on a disposable Linux environment")
	}
	cliPath := os.Getenv("PROFILE_BUNDLE_CLI")
	if cliPath == "" {
		t.Fatal("PROFILE_BUNDLE_CLI must name the prebuilt profile-bundle command")
	}

	temporaryDirectory := t.TempDir()
	bundlePath := filepath.Join(temporaryDirectory, "altered-component.bundle")
	writeIndependentBundleFixture(t, bundlePath, validFixtureLock(), validFixturePolicy())
	bundleDigest := rewriteIndependentBundleFixture(t, bundlePath, func(entries []independentOuterEntry) []independentOuterEntry {
		for index := range entries {
			if entries[index].name == "sandbox-init" {
				entries[index].contents = []byte("abd")
				return entries
			}
		}
		t.Fatal("independent fixture has no Sandbox Init to alter")
		return nil
	})
	storePath := filepath.Join(temporaryDirectory, "profile-store")
	if err := os.Mkdir(storePath, 0o755); err != nil {
		t.Fatal("create Profile store:", err)
	}

	command := exec.Command(
		cliPath, "install",
		"--bundle", bundlePath,
		"--expected-sha256", bundleDigest,
		"--store", storePath,
	)
	if output, err := command.CombinedOutput(); err == nil {
		t.Fatalf("install accepted an altered declared component:\n%s", output)
	}
	if entries, err := os.ReadDir(filepath.Join(storePath, "sha256")); err != nil || len(entries) != 0 {
		t.Fatalf("altered component published a Profile: entries=%#v err=%v", entries, err)
	}
}

func TestProfileBundleInstallRejectsMissingDeclaredComponent(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Fatal("profilebundle_root tests must run as root on a disposable Linux environment")
	}
	cliPath := os.Getenv("PROFILE_BUNDLE_CLI")
	if cliPath == "" {
		t.Fatal("PROFILE_BUNDLE_CLI must name the prebuilt profile-bundle command")
	}

	temporaryDirectory := t.TempDir()
	bundlePath := filepath.Join(temporaryDirectory, "missing-component.bundle")
	writeIndependentBundleFixture(t, bundlePath, validFixtureLock(), validFixturePolicy())
	bundleDigest := rewriteIndependentBundleFixture(t, bundlePath, func(entries []independentOuterEntry) []independentOuterEntry {
		for index := range entries {
			if entries[index].name == "sandbox-init" {
				return append(entries[:index], entries[index+1:]...)
			}
		}
		t.Fatal("independent fixture has no Sandbox Init to remove")
		return nil
	})
	storePath := filepath.Join(temporaryDirectory, "profile-store")
	if err := os.Mkdir(storePath, 0o755); err != nil {
		t.Fatal("create Profile store:", err)
	}

	command := exec.Command(
		cliPath, "install",
		"--bundle", bundlePath,
		"--expected-sha256", bundleDigest,
		"--store", storePath,
	)
	if output, err := command.CombinedOutput(); err == nil {
		t.Fatalf("install accepted a Bundle missing a declared component:\n%s", output)
	}
	if entries, err := os.ReadDir(filepath.Join(storePath, "sha256")); err != nil || len(entries) != 0 {
		t.Fatalf("missing component published a Profile: entries=%#v err=%v", entries, err)
	}
}

func TestProfileBundleInstallRejectsUnlistedComponent(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Fatal("profilebundle_root tests must run as root on a disposable Linux environment")
	}
	cliPath := os.Getenv("PROFILE_BUNDLE_CLI")
	if cliPath == "" {
		t.Fatal("PROFILE_BUNDLE_CLI must name the prebuilt profile-bundle command")
	}

	temporaryDirectory := t.TempDir()
	bundlePath := filepath.Join(temporaryDirectory, "unlisted-component.bundle")
	writeIndependentBundleFixture(t, bundlePath, validFixtureLock(), validFixturePolicy())
	bundleDigest := rewriteIndependentBundleFixture(t, bundlePath, func(entries []independentOuterEntry) []independentOuterEntry {
		return append(entries, independentOuterEntry{name: "unexpected-component", mode: 0o444, contents: []byte("unexpected")})
	})
	storePath := filepath.Join(temporaryDirectory, "profile-store")
	if err := os.Mkdir(storePath, 0o755); err != nil {
		t.Fatal("create Profile store:", err)
	}

	command := exec.Command(
		cliPath, "install",
		"--bundle", bundlePath,
		"--expected-sha256", bundleDigest,
		"--store", storePath,
	)
	if output, err := command.CombinedOutput(); err == nil {
		t.Fatalf("install accepted an unlisted component:\n%s", output)
	}
	if entries, err := os.ReadDir(filepath.Join(storePath, "sha256")); err != nil || len(entries) != 0 {
		t.Fatalf("unlisted component published a Profile: entries=%#v err=%v", entries, err)
	}
}

func TestProfileBundleInstallRejectsUnexpectedBundleDigest(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Fatal("profilebundle_root tests must run as root on a disposable Linux environment")
	}
	cliPath := os.Getenv("PROFILE_BUNDLE_CLI")
	if cliPath == "" {
		t.Fatal("PROFILE_BUNDLE_CLI must name the prebuilt profile-bundle command")
	}

	temporaryDirectory := t.TempDir()
	bundlePath := filepath.Join(temporaryDirectory, "unexpected-digest.bundle")
	writeIndependentBundleFixture(t, bundlePath, validFixtureLock(), validFixturePolicy())
	storePath := filepath.Join(temporaryDirectory, "profile-store")
	if err := os.Mkdir(storePath, 0o755); err != nil {
		t.Fatal("create Profile store:", err)
	}

	command := exec.Command(
		cliPath, "install",
		"--bundle", bundlePath,
		"--expected-sha256", "0000000000000000000000000000000000000000000000000000000000000000",
		"--store", storePath,
	)
	if output, err := command.CombinedOutput(); err == nil {
		t.Fatalf("install accepted an unexpected external Bundle digest:\n%s", output)
	}
	for _, directory := range []string{"sha256", ".staging"} {
		entries, err := os.ReadDir(filepath.Join(storePath, directory))
		if err != nil || len(entries) != 0 {
			t.Fatalf("unexpected digest left entries in %s: entries=%#v err=%v", directory, entries, err)
		}
	}
}

func TestProfileBundleInstallDoesNotEscapeProfileStore(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Fatal("profilebundle_root tests must run as root on a disposable Linux environment")
	}
	cliPath := os.Getenv("PROFILE_BUNDLE_CLI")
	if cliPath == "" {
		t.Fatal("PROFILE_BUNDLE_CLI must name the prebuilt profile-bundle command")
	}

	temporaryDirectory := t.TempDir()
	sentinelPath := filepath.Join(temporaryDirectory, "sentinel")
	if err := os.WriteFile(sentinelPath, []byte("safe"), 0o644); err != nil {
		t.Fatal("write host sentinel:", err)
	}
	maliciousRootFS := writeIndependentRootFSFixture(t, "../../../../sentinel", []byte("tampered"))
	bundlePath := filepath.Join(temporaryDirectory, "path-escape.bundle")
	bundleDigest := writeIndependentBundleFixtureWithRootFS(t, bundlePath, validFixtureLock(), validFixturePolicy(), maliciousRootFS)
	storePath := filepath.Join(temporaryDirectory, "profile-store")
	if err := os.Mkdir(storePath, 0o755); err != nil {
		t.Fatal("create Profile store:", err)
	}

	command := exec.Command(
		cliPath, "install",
		"--bundle", bundlePath,
		"--expected-sha256", bundleDigest,
		"--store", storePath,
	)
	if output, err := command.CombinedOutput(); err == nil {
		t.Fatalf("install accepted a root filesystem path escape:\n%s", output)
	}
	sentinel, err := os.ReadFile(sentinelPath)
	if err != nil {
		t.Fatal("read host sentinel after rejected install:", err)
	}
	if string(sentinel) != "safe" {
		t.Fatalf("root filesystem path escape changed host sentinel to %q", sentinel)
	}
	if entries, err := os.ReadDir(filepath.Join(storePath, "sha256")); err != nil || len(entries) != 0 {
		t.Fatalf("path escape published a Profile: entries=%#v err=%v", entries, err)
	}
}

func TestProfileBundleInstallRejectsManifestThatContradictsBuildLock(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Fatal("profilebundle_root tests must run as root on a disposable Linux environment")
	}
	cliPath := os.Getenv("PROFILE_BUNDLE_CLI")
	if cliPath == "" {
		t.Fatal("PROFILE_BUNDLE_CLI must name the prebuilt profile-bundle command")
	}

	temporaryDirectory := t.TempDir()
	bundlePath := filepath.Join(temporaryDirectory, "contradictory-manifest.bundle")
	writeIndependentBundleFixture(t, bundlePath, validFixtureLock(), validFixturePolicy())
	bundleDigest := rewriteIndependentBundleFixture(t, bundlePath, func(entries []independentOuterEntry) []independentOuterEntry {
		for index := range entries {
			if entries[index].name == "manifest.json" {
				entries[index].contents = bytes.Replace(
					entries[index].contents,
					[]byte(`"entrypoint":"/opt/python/bin/python3"`),
					[]byte(`"entrypoint":"/opt/python/bin/python9"`),
					1,
				)
				return entries
			}
		}
		t.Fatal("independent fixture has no manifest to contradict")
		return nil
	})
	storePath := filepath.Join(temporaryDirectory, "profile-store")
	if err := os.Mkdir(storePath, 0o755); err != nil {
		t.Fatal("create Profile store:", err)
	}

	command := exec.Command(
		cliPath, "install",
		"--bundle", bundlePath,
		"--expected-sha256", bundleDigest,
		"--store", storePath,
	)
	if output, err := command.CombinedOutput(); err == nil {
		t.Fatalf("install accepted a manifest that contradicts its build lock:\n%s", output)
	}
	if entries, err := os.ReadDir(filepath.Join(storePath, "sha256")); err != nil || len(entries) != 0 {
		t.Fatalf("contradictory manifest published a Profile: entries=%#v err=%v", entries, err)
	}
}

func TestProfileBundleInstallRejectsEscapingRootFilesystemSymlink(t *testing.T) {
	rootfs := writeIndependentRootFSEntry(t, &tar.Header{
		Name:     "opt/python/bin/python3",
		Linkname: "../../../../sentinel",
		Mode:     0o555,
		Uid:      0,
		Gid:      0,
		ModTime:  time.Unix(0, 0).UTC(),
		Typeflag: tar.TypeSymlink,
		Format:   tar.FormatPAX,
	}, nil)
	assertRootFSRejectedWithoutPublish(t, rootfs)
}

func TestProfileBundleInstallRejectsRootFilesystemHardLink(t *testing.T) {
	rootfs := writeIndependentRootFSEntry(t, &tar.Header{
		Name:     "opt/python/bin/python3",
		Linkname: "opt/python/bin/other",
		Mode:     0o555,
		Uid:      0,
		Gid:      0,
		ModTime:  time.Unix(0, 0).UTC(),
		Typeflag: tar.TypeLink,
		Format:   tar.FormatPAX,
	}, nil)
	assertRootFSRejectedWithoutPublish(t, rootfs)
}

func TestProfileBundleInstallRejectsRootFilesystemDevice(t *testing.T) {
	rootfs := writeIndependentRootFSEntry(t, &tar.Header{
		Name:     "dev/host-device",
		Mode:     0o444,
		Uid:      0,
		Gid:      0,
		Devmajor: 1,
		Devminor: 3,
		ModTime:  time.Unix(0, 0).UTC(),
		Typeflag: tar.TypeChar,
		Format:   tar.FormatPAX,
	}, nil)
	assertRootFSRejectedWithoutPublish(t, rootfs)
}

func TestProfileBundleInstallRejectsMissingPythonEntrypoint(t *testing.T) {
	rootfs := writeIndependentRootFSFixture(t, "opt/python/bin/not-python", []byte("fixture"))
	assertRootFSRejectedWithoutPublish(t, rootfs)
}

func assertRootFSRejectedWithoutPublish(t *testing.T, rootfs []byte) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Fatal("profilebundle_root tests must run as root on a disposable Linux environment")
	}
	cliPath := os.Getenv("PROFILE_BUNDLE_CLI")
	if cliPath == "" {
		t.Fatal("PROFILE_BUNDLE_CLI must name the prebuilt profile-bundle command")
	}
	temporaryDirectory := t.TempDir()
	bundlePath := filepath.Join(temporaryDirectory, "unsafe-rootfs.bundle")
	bundleDigest := writeIndependentBundleFixtureWithRootFS(t, bundlePath, validFixtureLock(), validFixturePolicy(), rootfs)
	storePath := filepath.Join(temporaryDirectory, "profile-store")
	if err := os.Mkdir(storePath, 0o755); err != nil {
		t.Fatal("create Profile store:", err)
	}
	command := exec.Command(
		cliPath, "install",
		"--bundle", bundlePath,
		"--expected-sha256", bundleDigest,
		"--store", storePath,
	)
	if output, err := command.CombinedOutput(); err == nil {
		t.Fatalf("install accepted an unsafe root filesystem entry:\n%s", output)
	}
	if entries, err := os.ReadDir(filepath.Join(storePath, "sha256")); err != nil || len(entries) != 0 {
		t.Fatalf("unsafe root filesystem published a Profile: entries=%#v err=%v", entries, err)
	}
}

func validFixtureLock() []byte {
	return []byte("{\"schema\":\"profile-build-lock/v1\",\"profile\":\"python-data-v1\",\"target\":{\"os\":\"linux\",\"arch\":\"amd64\"},\"python\":{\"version\":\"3.14.7\",\"entrypoint\":\"/opt/python/bin/python3\",\"packages\":[]},\"inputs\":[{\"id\":\"fixture\",\"kind\":\"python-install-only\",\"version\":\"3.14.7+fixture\",\"source\":\"https://example.invalid/python.tar.gz\",\"filename\":\"python.tar.gz\",\"size\":1,\"digest\":\"sha256:0000000000000000000000000000000000000000000000000000000000000000\",\"archive_prefix\":\"python/\",\"destination\":\"opt/python/\",\"include\":[],\"exclude\":[]}]}\n")
}

func validFixturePolicy() []byte {
	return []byte("{\"schema\":\"system-call-policy/v1\",\"profile\":\"python-data-v1\",\"target\":{\"os\":\"linux\",\"arch\":\"amd64\"},\"default_action\":{\"action\":\"errno\",\"errno\":1},\"rules\":[]}\n")
}

func writeIndependentBundleFixture(t *testing.T, bundlePath string, lock, policy []byte) string {
	t.Helper()
	rootfs := writeIndependentRootFSFixture(t, "opt/python/bin/python3", []byte("fixture-python"))
	return writeIndependentBundleFixtureWithRootFS(t, bundlePath, lock, policy, rootfs)
}

func writeIndependentRootFSFixture(t *testing.T, name string, contents []byte) []byte {
	t.Helper()
	return writeIndependentRootFSEntry(t, &tar.Header{
		Name:     name,
		Mode:     0o555,
		Size:     int64(len(contents)),
		Uid:      0,
		Gid:      0,
		ModTime:  time.Unix(0, 0).UTC(),
		Typeflag: tar.TypeReg,
		Format:   tar.FormatPAX,
	}, contents)
}

func writeIndependentRootFSEntry(t *testing.T, header *tar.Header, contents []byte) []byte {
	t.Helper()
	var rootfs bytes.Buffer
	rootfsWriter := tar.NewWriter(&rootfs)
	if err := rootfsWriter.WriteHeader(header); err != nil {
		t.Fatal("write fixture root filesystem header:", err)
	}
	if _, err := rootfsWriter.Write(contents); err != nil {
		t.Fatal("write fixture root filesystem contents:", err)
	}
	if err := rootfsWriter.Close(); err != nil {
		t.Fatal("close fixture root filesystem:", err)
	}
	return rootfs.Bytes()
}

func writeIndependentBundleFixtureWithRootFS(t *testing.T, bundlePath string, lock, policy, rootfs []byte) string {
	t.Helper()
	return writeIndependentBundleFixtureWithRootFSAndInit(t, bundlePath, lock, policy, rootfs, []byte("abc"))
}

func writeIndependentBundleFixtureWithRootFSAndInit(t *testing.T, bundlePath string, lock, policy, rootfs, init []byte) string {
	t.Helper()

	type fixtureComponent struct {
		role       string
		path       string
		format     string
		mode       string
		contents   []byte
		sha256Text string
	}
	components := []fixtureComponent{
		{role: "build-lock", path: "profile.lock.json", format: "profile-build-lock/v1", mode: "0444", contents: lock},
		{role: "system-call-policy", path: "system-call-policy.json", format: "system-call-policy/v1", mode: "0444", contents: policy},
		{role: "sandbox-init", path: "sandbox-init", format: "opaque/v1", mode: "0555", contents: init},
		{role: "rootfs", path: "rootfs.tar", format: "rootfs-pax-tar/v1", mode: "0444", contents: rootfs},
	}
	for index := range components {
		components[index].sha256Text = independentSHA256(components[index].contents)
	}

	manifest := fmt.Sprintf(
		"{\"schema\":\"profile-bundle-manifest/v1\",\"profile\":\"python-data-v1\",\"target\":{\"os\":\"linux\",\"arch\":\"amd64\"},\"python\":{\"version\":\"3.14.7\",\"entrypoint\":\"/opt/python/bin/python3\",\"packages\":[]},\"components\":["+
			"{\"role\":\"build-lock\",\"path\":\"profile.lock.json\",\"format\":\"profile-build-lock/v1\",\"mode\":\"0444\",\"size\":%d,\"digest\":\"sha256:%s\"},"+
			"{\"role\":\"system-call-policy\",\"path\":\"system-call-policy.json\",\"format\":\"system-call-policy/v1\",\"mode\":\"0444\",\"size\":%d,\"digest\":\"sha256:%s\"},"+
			"{\"role\":\"sandbox-init\",\"path\":\"sandbox-init\",\"format\":\"opaque/v1\",\"mode\":\"0555\",\"size\":%d,\"digest\":\"sha256:%s\"},"+
			"{\"role\":\"rootfs\",\"path\":\"rootfs.tar\",\"format\":\"rootfs-pax-tar/v1\",\"mode\":\"0444\",\"size\":%d,\"digest\":\"sha256:%s\"}]}\n",
		len(lock), components[0].sha256Text,
		len(policy), components[1].sha256Text,
		len(init), components[2].sha256Text,
		len(rootfs), components[3].sha256Text,
	)

	file, err := os.Create(bundlePath)
	if err != nil {
		t.Fatal("create independent Profile Bundle fixture:", err)
	}
	bundleWriter := tar.NewWriter(file)
	writeIndependentOuterEntry(t, bundleWriter, "manifest.json", 0o444, []byte(manifest))
	for _, component := range components {
		mode := int64(0o444)
		if component.mode == "0555" {
			mode = 0o555
		}
		writeIndependentOuterEntry(t, bundleWriter, component.path, mode, component.contents)
	}
	if err := bundleWriter.Close(); err != nil {
		t.Fatal("close independent Profile Bundle fixture tar:", err)
	}
	if err := file.Close(); err != nil {
		t.Fatal("close independent Profile Bundle fixture:", err)
	}

	bundleBytes, err := os.ReadFile(bundlePath)
	if err != nil {
		t.Fatal("read independent Profile Bundle fixture:", err)
	}
	return independentSHA256(bundleBytes)
}

func writeIndependentOuterEntry(t *testing.T, writer *tar.Writer, name string, mode int64, contents []byte) {
	t.Helper()
	header := &tar.Header{
		Name:     name,
		Mode:     mode,
		Size:     int64(len(contents)),
		Uid:      0,
		Gid:      0,
		ModTime:  time.Unix(0, 0).UTC(),
		Typeflag: tar.TypeReg,
		Format:   tar.FormatUSTAR,
	}
	if err := writer.WriteHeader(header); err != nil {
		t.Fatalf("write independent fixture entry %q header: %v", name, err)
	}
	if _, err := writer.Write(contents); err != nil {
		t.Fatalf("write independent fixture entry %q: %v", name, err)
	}
}

func independentSHA256(contents []byte) string {
	digest := sha256.Sum256(contents)
	return hex.EncodeToString(digest[:])
}

type independentOuterEntry struct {
	name     string
	mode     int64
	contents []byte
}

func rewriteIndependentBundleFixture(t *testing.T, bundlePath string, mutate func([]independentOuterEntry) []independentOuterEntry) string {
	t.Helper()

	file, err := os.Open(bundlePath)
	if err != nil {
		t.Fatal("open independent Profile Bundle fixture for rewrite:", err)
	}
	reader := tar.NewReader(file)
	var entries []independentOuterEntry
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			file.Close()
			t.Fatal("read independent Profile Bundle fixture for rewrite:", err)
		}
		contents, err := io.ReadAll(reader)
		if err != nil {
			file.Close()
			t.Fatalf("read independent entry %q for rewrite: %v", header.Name, err)
		}
		entries = append(entries, independentOuterEntry{name: header.Name, mode: header.Mode, contents: contents})
	}
	if err := file.Close(); err != nil {
		t.Fatal("close independent Profile Bundle fixture before rewrite:", err)
	}
	entries = mutate(entries)

	file, err = os.Create(bundlePath)
	if err != nil {
		t.Fatal("recreate independent Profile Bundle fixture:", err)
	}
	writer := tar.NewWriter(file)
	for _, entry := range entries {
		writeIndependentOuterEntry(t, writer, entry.name, entry.mode, entry.contents)
	}
	if err := writer.Close(); err != nil {
		t.Fatal("close rewritten independent Profile Bundle fixture tar:", err)
	}
	if err := file.Close(); err != nil {
		t.Fatal("close rewritten independent Profile Bundle fixture:", err)
	}
	bundleBytes, err := os.ReadFile(bundlePath)
	if err != nil {
		t.Fatal("read rewritten independent Profile Bundle fixture:", err)
	}
	return independentSHA256(bundleBytes)
}
