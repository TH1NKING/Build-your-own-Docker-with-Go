package workspace_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestProfileBundleBuildRecordsSuppliedSandboxInitIdentity(t *testing.T) {
	repositoryRoot := profileBundleRepositoryRoot(t)
	temporaryDirectory := t.TempDir()
	sourceCache := filepath.Join(temporaryDirectory, "source-cache")
	if err := os.Mkdir(sourceCache, 0o755); err != nil {
		t.Fatal("create source cache:", err)
	}

	sourceArchive := filepath.Join(sourceCache, "python-runtime.tar.gz")
	writePythonSourceArchive(t, sourceArchive)
	sourceDigest := fileSHA256(t, sourceArchive)
	sourceInfo, err := os.Stat(sourceArchive)
	if err != nil {
		t.Fatal("stat source archive:", err)
	}

	lockPath := filepath.Join(temporaryDirectory, "profile.lock.json")
	lock := fmt.Sprintf(`{
  "schema": "profile-build-lock/v1",
  "profile": "python-data-v1",
  "target": {"os": "linux", "arch": "amd64"},
  "python": {"version": "3.14.7", "entrypoint": "/opt/python/bin/python3", "packages": []},
  "inputs": [{
    "id": "python-runtime",
    "kind": "python-install-only",
    "version": "3.14.7+test",
    "source": "https://example.invalid/python-runtime.tar.gz",
    "filename": "python-runtime.tar.gz",
    "size": %d,
    "digest": "sha256:%s",
    "archive_prefix": "python/",
    "destination": "opt/python/",
    "include": [],
    "exclude": []
  }]
}
`, sourceInfo.Size(), sourceDigest)
	if err := os.WriteFile(lockPath, []byte(lock), 0o644); err != nil {
		t.Fatal("write build lock:", err)
	}

	policyPath := filepath.Join(temporaryDirectory, "system-call-policy.json")
	policy := []byte("{\"schema\":\"system-call-policy/v1\",\"profile\":\"python-data-v1\",\"target\":{\"os\":\"linux\",\"arch\":\"amd64\"},\"default_action\":{\"action\":\"errno\",\"errno\":1},\"rules\":[]}\n")
	if err := os.WriteFile(policyPath, policy, 0o644); err != nil {
		t.Fatal("write System Call Policy:", err)
	}

	initPath := filepath.Join(temporaryDirectory, "sandbox-init")
	if err := os.WriteFile(initPath, []byte("abc"), 0o755); err != nil {
		t.Fatal("write Sandbox Init:", err)
	}

	bundlePath := filepath.Join(temporaryDirectory, "python-data-v1.bundle")
	command := exec.Command(
		"go", "run", "./cmd/profile-bundle", "build",
		"--lock", lockPath,
		"--source-cache", sourceCache,
		"--system-call-policy", policyPath,
		"--sandbox-init", initPath,
		"--output", bundlePath,
	)
	command.Dir = repositoryRoot
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build Profile Bundle: %v\n%s", err, output)
	}

	entries := readOuterBundle(t, bundlePath)
	manifestBytes, ok := entries["manifest.json"]
	if !ok {
		t.Fatal("built Profile Bundle has no manifest.json")
	}

	var manifest struct {
		Components []struct {
			Role   string `json:"role"`
			Digest string `json:"digest"`
		} `json:"components"`
	}
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatal("decode public Profile Bundle manifest:", err)
	}

	const expectedInitDigest = "sha256:ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	for _, component := range manifest.Components {
		if component.Role == "sandbox-init" {
			if component.Digest != expectedInitDigest {
				t.Fatalf("Sandbox Init digest = %q, want %q", component.Digest, expectedInitDigest)
			}
			return
		}
	}

	t.Fatal("public Profile Bundle manifest does not declare the Sandbox Init component")
}

func TestPythonDataV1RecipePinsEveryExternalInput(t *testing.T) {
	repositoryRoot := profileBundleRepositoryRoot(t)
	lockPath := filepath.Join(repositoryRoot, "profiles", "python-data-v1", "profile.lock.json")
	lockBytes, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal("read published python-data-v1 build lock:", err)
	}

	var lock struct {
		Schema  string `json:"schema"`
		Profile string `json:"profile"`
		Target  struct {
			OS   string `json:"os"`
			Arch string `json:"arch"`
		} `json:"target"`
		Python struct {
			Version  string `json:"version"`
			Packages []any  `json:"packages"`
		} `json:"python"`
		Inputs []struct {
			Version  string `json:"version"`
			Source   string `json:"source"`
			Filename string `json:"filename"`
			Size     int64  `json:"size"`
			Digest   string `json:"digest"`
		} `json:"inputs"`
	}
	if err := json.Unmarshal(lockBytes, &lock); err != nil {
		t.Fatal("decode published python-data-v1 build lock:", err)
	}

	wantHeader := struct {
		Schema        string
		Profile       string
		TargetOS      string
		TargetArch    string
		PythonVersion string
	}{
		Schema:        "profile-build-lock/v1",
		Profile:       "python-data-v1",
		TargetOS:      "linux",
		TargetArch:    "amd64",
		PythonVersion: "3.14.7",
	}
	gotHeader := struct {
		Schema        string
		Profile       string
		TargetOS      string
		TargetArch    string
		PythonVersion string
	}{
		Schema:        lock.Schema,
		Profile:       lock.Profile,
		TargetOS:      lock.Target.OS,
		TargetArch:    lock.Target.Arch,
		PythonVersion: lock.Python.Version,
	}
	if gotHeader != wantHeader {
		t.Fatalf("published python-data-v1 lock header = %#v, want %#v", gotHeader, wantHeader)
	}
	wantInputs := []struct {
		Version  string
		Source   string
		Filename string
		Size     int64
		Digest   string
	}{
		{
			Version:  "alpine-3.22.5-musl-1.2.5-r12",
			Source:   "https://dl-cdn.alpinelinux.org/alpine/v3.22/releases/x86_64/alpine-minirootfs-3.22.5-x86_64.tar.gz",
			Filename: "alpine-minirootfs-3.22.5-x86_64.tar.gz",
			Size:     3638276,
			Digest:   "sha256:4b4daa9fe2fc696c4919c4412a4c3d3e770d8fb70292a004a2c72f5096175282",
		},
		{
			Version:  "3.14.7+20260825",
			Source:   "https://github.com/astral-sh/python-build-standalone/releases/download/20260825/cpython-3.14.7%2B20260825-x86_64-unknown-linux-musl-install_only_stripped.tar.gz",
			Filename: "cpython-3.14.7+20260825-x86_64-unknown-linux-musl-install_only_stripped.tar.gz",
			Size:     29011055,
			Digest:   "sha256:22374158b61f9415135614fb7ebdba498a84849921bb5ad025809e925d8bb428",
		},
	}
	if len(lock.Inputs) != len(wantInputs) {
		t.Fatalf("published python-data-v1 inputs = %d, want %d", len(lock.Inputs), len(wantInputs))
	}
	for index, want := range wantInputs {
		got := lock.Inputs[index]
		if got.Version != want.Version || got.Source != want.Source || got.Filename != want.Filename || got.Size != want.Size || got.Digest != want.Digest {
			t.Fatalf("published python-data-v1 input %d = %#v, want %#v", index, got, want)
		}
	}
	if len(lock.Python.Packages) != 0 {
		t.Fatalf("python-data-v1 unexpectedly declares third-party packages: %#v", lock.Python.Packages)
	}
}

func TestPythonDataV1SystemCallPolicyDefaultsToDeny(t *testing.T) {
	repositoryRoot := profileBundleRepositoryRoot(t)
	policyPath := filepath.Join(repositoryRoot, "profiles", "python-data-v1", "system-call-policy.json")
	policyBytes, err := os.ReadFile(policyPath)
	if err != nil {
		t.Fatal("read published python-data-v1 System Call Policy:", err)
	}

	var policy struct {
		Schema  string `json:"schema"`
		Profile string `json:"profile"`
		Target  struct {
			OS   string `json:"os"`
			Arch string `json:"arch"`
		} `json:"target"`
		DefaultAction struct {
			Action string `json:"action"`
			Errno  int    `json:"errno"`
		} `json:"default_action"`
	}
	if err := json.Unmarshal(policyBytes, &policy); err != nil {
		t.Fatal("decode published python-data-v1 System Call Policy:", err)
	}

	if policy.Schema != "system-call-policy/v1" ||
		policy.Profile != "python-data-v1" ||
		policy.Target.OS != "linux" ||
		policy.Target.Arch != "amd64" ||
		policy.DefaultAction.Action != "errno" ||
		policy.DefaultAction.Errno != 1 {
		t.Fatalf("python-data-v1 policy does not provide the published default-deny contract: %#v", policy)
	}
}

func TestProfileBundleBuildCanonicalizesRootFilesystemOrder(t *testing.T) {
	repositoryRoot := profileBundleRepositoryRoot(t)
	temporaryDirectory := t.TempDir()
	sourceCache := filepath.Join(temporaryDirectory, "source-cache")
	if err := os.Mkdir(sourceCache, 0o755); err != nil {
		t.Fatal("create source cache:", err)
	}
	sourceArchive := filepath.Join(sourceCache, "python-runtime.tar.gz")
	writeReverseOrderedPythonSourceArchive(t, sourceArchive)
	sourceInfo, err := os.Stat(sourceArchive)
	if err != nil {
		t.Fatal("stat source archive:", err)
	}

	lockPath := filepath.Join(temporaryDirectory, "profile.lock.json")
	lock := fmt.Sprintf(`{"schema":"profile-build-lock/v1","profile":"python-data-v1","target":{"os":"linux","arch":"amd64"},"python":{"version":"3.14.7","entrypoint":"/opt/python/bin/python3","packages":[]},"inputs":[{"id":"python-runtime","kind":"python-install-only","version":"3.14.7+test","source":"https://example.invalid/python-runtime.tar.gz","filename":"python-runtime.tar.gz","size":%d,"digest":"sha256:%s","archive_prefix":"python/","destination":"opt/python/","include":[],"exclude":[]}]}
`, sourceInfo.Size(), fileSHA256(t, sourceArchive))
	if err := os.WriteFile(lockPath, []byte(lock), 0o644); err != nil {
		t.Fatal("write build lock:", err)
	}
	policyPath := filepath.Join(temporaryDirectory, "system-call-policy.json")
	policy := []byte("{\"schema\":\"system-call-policy/v1\",\"profile\":\"python-data-v1\",\"target\":{\"os\":\"linux\",\"arch\":\"amd64\"},\"default_action\":{\"action\":\"errno\",\"errno\":1},\"rules\":[]}\n")
	if err := os.WriteFile(policyPath, policy, 0o644); err != nil {
		t.Fatal("write System Call Policy:", err)
	}
	initPath := filepath.Join(temporaryDirectory, "sandbox-init")
	if err := os.WriteFile(initPath, []byte("abc"), 0o755); err != nil {
		t.Fatal("write Sandbox Init:", err)
	}
	bundlePath := filepath.Join(temporaryDirectory, "python-data-v1.bundle")
	command := exec.Command(
		"go", "run", "./cmd/profile-bundle", "build",
		"--lock", lockPath,
		"--source-cache", sourceCache,
		"--system-call-policy", policyPath,
		"--sandbox-init", initPath,
		"--output", bundlePath,
	)
	command.Dir = repositoryRoot
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build Profile Bundle from reverse-ordered source: %v\n%s", err, output)
	}

	rootfs := readOuterBundle(t, bundlePath)["rootfs.tar"]
	reader := tar.NewReader(bytes.NewReader(rootfs))
	var names []string
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal("read canonical root filesystem:", err)
		}
		names = append(names, header.Name)
	}
	want := []string{"opt/python/bin/a-helper", "opt/python/bin/python3"}
	if len(names) != len(want) || names[0] != want[0] || names[1] != want[1] {
		t.Fatalf("canonical root filesystem order = %#v, want %#v", names, want)
	}
}

func TestProfileBundleBuildRejectsMismatchedSystemCallPolicy(t *testing.T) {
	repositoryRoot := profileBundleRepositoryRoot(t)
	temporaryDirectory := t.TempDir()
	sourceCache := filepath.Join(temporaryDirectory, "source-cache")
	if err := os.Mkdir(sourceCache, 0o755); err != nil {
		t.Fatal("create source cache:", err)
	}
	sourceArchive := filepath.Join(sourceCache, "python-runtime.tar.gz")
	writePythonSourceArchive(t, sourceArchive)
	sourceInfo, err := os.Stat(sourceArchive)
	if err != nil {
		t.Fatal("stat source archive:", err)
	}
	lockPath := filepath.Join(temporaryDirectory, "profile.lock.json")
	lock := fmt.Sprintf(`{"schema":"profile-build-lock/v1","profile":"python-data-v1","target":{"os":"linux","arch":"amd64"},"python":{"version":"3.14.7","entrypoint":"/opt/python/bin/python3","packages":[]},"inputs":[{"id":"python-runtime","kind":"python-install-only","version":"3.14.7+test","source":"https://example.invalid/python-runtime.tar.gz","filename":"python-runtime.tar.gz","size":%d,"digest":"sha256:%s","archive_prefix":"python/","destination":"opt/python/","include":[],"exclude":[]}]}
`, sourceInfo.Size(), fileSHA256(t, sourceArchive))
	if err := os.WriteFile(lockPath, []byte(lock), 0o644); err != nil {
		t.Fatal("write build lock:", err)
	}
	policyPath := filepath.Join(temporaryDirectory, "system-call-policy.json")
	mismatchedPolicy := []byte("{\"schema\":\"system-call-policy/v1\",\"profile\":\"another-profile\",\"target\":{\"os\":\"linux\",\"arch\":\"amd64\"},\"default_action\":{\"action\":\"errno\",\"errno\":1},\"rules\":[]}\n")
	if err := os.WriteFile(policyPath, mismatchedPolicy, 0o644); err != nil {
		t.Fatal("write mismatched System Call Policy:", err)
	}
	initPath := filepath.Join(temporaryDirectory, "sandbox-init")
	if err := os.WriteFile(initPath, []byte("abc"), 0o755); err != nil {
		t.Fatal("write Sandbox Init:", err)
	}
	bundlePath := filepath.Join(temporaryDirectory, "python-data-v1.bundle")
	command := exec.Command(
		"go", "run", "./cmd/profile-bundle", "build",
		"--lock", lockPath,
		"--source-cache", sourceCache,
		"--system-call-policy", policyPath,
		"--sandbox-init", initPath,
		"--output", bundlePath,
	)
	command.Dir = repositoryRoot
	if output, err := command.CombinedOutput(); err == nil {
		t.Fatalf("build accepted a System Call Policy for another profile:\n%s", output)
	}
}

func TestProfileBundleBuildRejectsMalformedSystemCallPolicyRule(t *testing.T) {
	repositoryRoot := profileBundleRepositoryRoot(t)
	temporaryDirectory := t.TempDir()
	policyPath := filepath.Join(temporaryDirectory, "system-call-policy.json")
	malformedPolicy := []byte("{\"schema\":\"system-call-policy/v1\",\"profile\":\"python-data-v1\",\"target\":{\"os\":\"linux\",\"arch\":\"amd64\"},\"default_action\":{\"action\":\"errno\",\"errno\":1},\"rules\":[{\"names\":[\"../bad\"],\"action\":\"allow\",\"arguments\":null}]}\n")
	if err := os.WriteFile(policyPath, malformedPolicy, 0o644); err != nil {
		t.Fatal("write malformed System Call Policy:", err)
	}
	initPath := filepath.Join(temporaryDirectory, "sandbox-init")
	if err := os.WriteFile(initPath, []byte("abc"), 0o755); err != nil {
		t.Fatal("write Sandbox Init:", err)
	}
	sourceCache := filepath.Join(temporaryDirectory, "source-cache")
	if err := os.Mkdir(sourceCache, 0o755); err != nil {
		t.Fatal("create empty source cache:", err)
	}
	command := exec.Command(
		"go", "run", "./cmd/profile-bundle", "build",
		"--lock", filepath.Join(repositoryRoot, "profiles", "python-data-v1", "profile.lock.json"),
		"--source-cache", sourceCache,
		"--system-call-policy", policyPath,
		"--sandbox-init", initPath,
		"--output", filepath.Join(temporaryDirectory, "malformed-policy.bundle"),
	)
	command.Dir = repositoryRoot
	if output, err := command.CombinedOutput(); err == nil {
		t.Fatalf("build accepted a malformed System Call Policy rule:\n%s", output)
	}
}

func TestProfileBundleBuildDoesNotOverwriteConcurrentOutput(t *testing.T) {
	repositoryRoot := profileBundleRepositoryRoot(t)
	temporaryDirectory := t.TempDir()
	sourceCache := filepath.Join(temporaryDirectory, "source-cache")
	if err := os.Mkdir(sourceCache, 0o755); err != nil {
		t.Fatal("create source cache:", err)
	}
	sourceArchive := filepath.Join(sourceCache, "python-runtime.tar.gz")
	writePythonSourceArchive(t, sourceArchive)
	sourceInfo, err := os.Stat(sourceArchive)
	if err != nil {
		t.Fatal("stat source archive:", err)
	}
	lockPath := filepath.Join(temporaryDirectory, "profile.lock.json")
	lock := fmt.Sprintf(`{"schema":"profile-build-lock/v1","profile":"python-data-v1","target":{"os":"linux","arch":"amd64"},"python":{"version":"3.14.7","entrypoint":"/opt/python/bin/python3","packages":[]},"inputs":[{"id":"python-runtime","kind":"python-install-only","version":"3.14.7+test","source":"https://example.invalid/python-runtime.tar.gz","filename":"python-runtime.tar.gz","size":%d,"digest":"sha256:%s","archive_prefix":"python/","destination":"opt/python/","include":[],"exclude":[]}]}
`, sourceInfo.Size(), fileSHA256(t, sourceArchive))
	if err := os.WriteFile(lockPath, []byte(lock), 0o644); err != nil {
		t.Fatal("write build lock:", err)
	}
	policyPath := filepath.Join(temporaryDirectory, "system-call-policy.json")
	if err := os.WriteFile(policyPath, validCLIFixturePolicy(), 0o644); err != nil {
		t.Fatal("write System Call Policy:", err)
	}
	initPaths := []string{
		filepath.Join(temporaryDirectory, "sandbox-init-one"),
		filepath.Join(temporaryDirectory, "sandbox-init-two"),
	}
	if err := os.WriteFile(initPaths[0], []byte("abc"), 0o755); err != nil {
		t.Fatal("write first Sandbox Init:", err)
	}
	if err := os.WriteFile(initPaths[1], []byte("abcd"), 0o755); err != nil {
		t.Fatal("write second Sandbox Init:", err)
	}
	outputPath := filepath.Join(temporaryDirectory, "concurrent.bundle")

	commands := make([]*exec.Cmd, len(initPaths))
	outputs := make([]bytes.Buffer, len(initPaths))
	for index, initPath := range initPaths {
		commands[index] = exec.Command(
			"go", "run", "./cmd/profile-bundle", "build",
			"--lock", lockPath,
			"--source-cache", sourceCache,
			"--system-call-policy", policyPath,
			"--sandbox-init", initPath,
			"--output", outputPath,
		)
		commands[index].Dir = repositoryRoot
		commands[index].Stdout = &outputs[index]
		commands[index].Stderr = &outputs[index]
		if err := commands[index].Start(); err != nil {
			t.Fatalf("start concurrent build %d: %v", index+1, err)
		}
	}
	successes := 0
	winnerDigest := ""
	for index, command := range commands {
		if err := command.Wait(); err == nil {
			successes++
			winnerDigest = strings.TrimSpace(outputs[index].String())
		}
	}
	if successes != 1 {
		t.Fatalf("concurrent builds succeeded %d times, want exactly one; outputs=%v", successes, outputs)
	}
	if actualDigest := fileSHA256(t, outputPath); actualDigest != winnerDigest {
		t.Fatalf("published concurrent Bundle SHA-256 = %q, successful builder reported %q", actualDigest, winnerDigest)
	}
}

func TestProfileBundleBuildRejectsRootFilesystemWhoseImplicitDirectoriesExceedBudget(t *testing.T) {
	repositoryRoot := profileBundleRepositoryRoot(t)
	temporaryDirectory := t.TempDir()
	sourceCache := filepath.Join(temporaryDirectory, "source-cache")
	if err := os.Mkdir(sourceCache, 0o755); err != nil {
		t.Fatal("create source cache:", err)
	}
	sourceArchive := filepath.Join(sourceCache, "python-runtime.tar.gz")
	writeDeepPythonSourceArchive(t, sourceArchive)
	sourceInfo, err := os.Stat(sourceArchive)
	if err != nil {
		t.Fatal("stat deep source archive:", err)
	}
	lockPath := filepath.Join(temporaryDirectory, "profile.lock.json")
	lock := fmt.Sprintf(`{"schema":"profile-build-lock/v1","profile":"python-data-v1","target":{"os":"linux","arch":"amd64"},"python":{"version":"3.14.7","entrypoint":"/opt/python/bin/python3","packages":[]},"inputs":[{"id":"python-runtime","kind":"python-install-only","version":"3.14.7+test","source":"https://example.invalid/python-runtime.tar.gz","filename":"python-runtime.tar.gz","size":%d,"digest":"sha256:%s","archive_prefix":"python/","destination":"opt/python/","include":[],"exclude":[]}]}
`, sourceInfo.Size(), fileSHA256(t, sourceArchive))
	if err := os.WriteFile(lockPath, []byte(lock), 0o644); err != nil {
		t.Fatal("write build lock:", err)
	}
	policyPath := filepath.Join(temporaryDirectory, "system-call-policy.json")
	if err := os.WriteFile(policyPath, validCLIFixturePolicy(), 0o644); err != nil {
		t.Fatal("write System Call Policy:", err)
	}
	initPath := filepath.Join(temporaryDirectory, "sandbox-init")
	if err := os.WriteFile(initPath, []byte("abc"), 0o755); err != nil {
		t.Fatal("write Sandbox Init:", err)
	}
	command := exec.Command(
		"go", "run", "./cmd/profile-bundle", "build",
		"--lock", lockPath,
		"--source-cache", sourceCache,
		"--system-call-policy", policyPath,
		"--sandbox-init", initPath,
		"--output", filepath.Join(temporaryDirectory, "over-budget.bundle"),
	)
	command.Dir = repositoryRoot
	if output, err := command.CombinedOutput(); err == nil {
		t.Fatalf("build accepted a root filesystem whose implicit directories exceed the v1 budget:\n%s", output)
	}
}

func TestProfileBundleBuildRejectsNonDirectoryAncestor(t *testing.T) {
	repositoryRoot := profileBundleRepositoryRoot(t)
	temporaryDirectory := t.TempDir()
	sourceCache := filepath.Join(temporaryDirectory, "source-cache")
	if err := os.Mkdir(sourceCache, 0o755); err != nil {
		t.Fatal("create source cache:", err)
	}
	sourceArchive := filepath.Join(sourceCache, "python-runtime.tar.gz")
	writeConflictingPythonSourceArchive(t, sourceArchive)
	sourceInfo, err := os.Stat(sourceArchive)
	if err != nil {
		t.Fatal("stat conflicting source archive:", err)
	}
	lockPath := filepath.Join(temporaryDirectory, "profile.lock.json")
	lock := fmt.Sprintf(`{"schema":"profile-build-lock/v1","profile":"python-data-v1","target":{"os":"linux","arch":"amd64"},"python":{"version":"3.14.7","entrypoint":"/opt/python/bin/python3","packages":[]},"inputs":[{"id":"python-runtime","kind":"python-install-only","version":"3.14.7+test","source":"https://example.invalid/python-runtime.tar.gz","filename":"python-runtime.tar.gz","size":%d,"digest":"sha256:%s","archive_prefix":"python/","destination":"opt/python/","include":[],"exclude":[]}]}
`, sourceInfo.Size(), fileSHA256(t, sourceArchive))
	if err := os.WriteFile(lockPath, []byte(lock), 0o644); err != nil {
		t.Fatal("write build lock:", err)
	}
	policyPath := filepath.Join(temporaryDirectory, "system-call-policy.json")
	if err := os.WriteFile(policyPath, validCLIFixturePolicy(), 0o644); err != nil {
		t.Fatal("write System Call Policy:", err)
	}
	initPath := filepath.Join(temporaryDirectory, "sandbox-init")
	if err := os.WriteFile(initPath, []byte("abc"), 0o755); err != nil {
		t.Fatal("write Sandbox Init:", err)
	}
	command := exec.Command(
		"go", "run", "./cmd/profile-bundle", "build",
		"--lock", lockPath,
		"--source-cache", sourceCache,
		"--system-call-policy", policyPath,
		"--sandbox-init", initPath,
		"--output", filepath.Join(temporaryDirectory, "conflicting-tree.bundle"),
	)
	command.Dir = repositoryRoot
	if output, err := command.CombinedOutput(); err == nil {
		t.Fatalf("build accepted a regular file as another rootfs entry's parent:\n%s", output)
	}
}

func validCLIFixturePolicy() []byte {
	return []byte("{\"schema\":\"system-call-policy/v1\",\"profile\":\"python-data-v1\",\"target\":{\"os\":\"linux\",\"arch\":\"amd64\"},\"default_action\":{\"action\":\"errno\",\"errno\":1},\"rules\":[]}\n")
}

func writePythonSourceArchive(t *testing.T, archivePath string) {
	t.Helper()

	file, err := os.Create(archivePath)
	if err != nil {
		t.Fatal("create Python source archive:", err)
	}
	gzipWriter := gzip.NewWriter(file)
	gzipWriter.Header.ModTime = time.Unix(0, 0).UTC()
	gzipWriter.Header.OS = 255
	tarWriter := tar.NewWriter(gzipWriter)

	contents := []byte("fixture-python")
	header := &tar.Header{
		Name:     "python/bin/python3",
		Mode:     0o755,
		Size:     int64(len(contents)),
		Typeflag: tar.TypeReg,
		ModTime:  time.Unix(0, 0).UTC(),
		Format:   tar.FormatPAX,
	}
	if err := tarWriter.WriteHeader(header); err != nil {
		t.Fatal("write Python source header:", err)
	}
	if _, err := tarWriter.Write(contents); err != nil {
		t.Fatal("write Python source contents:", err)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal("close Python source tar:", err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal("close Python source gzip:", err)
	}
	if err := file.Close(); err != nil {
		t.Fatal("close Python source archive:", err)
	}
}

func writeReverseOrderedPythonSourceArchive(t *testing.T, archivePath string) {
	t.Helper()

	file, err := os.Create(archivePath)
	if err != nil {
		t.Fatal("create reverse-ordered Python source archive:", err)
	}
	gzipWriter := gzip.NewWriter(file)
	gzipWriter.Header.ModTime = time.Unix(0, 0).UTC()
	gzipWriter.Header.OS = 255
	tarWriter := tar.NewWriter(gzipWriter)
	for _, entry := range []struct {
		name     string
		contents string
	}{
		{name: "python/bin/python3", contents: "fixture-python"},
		{name: "python/bin/a-helper", contents: "helper"},
	} {
		header := &tar.Header{
			Name:     entry.name,
			Mode:     0o755,
			Size:     int64(len(entry.contents)),
			Typeflag: tar.TypeReg,
			ModTime:  time.Unix(0, 0).UTC(),
			Format:   tar.FormatPAX,
		}
		if err := tarWriter.WriteHeader(header); err != nil {
			t.Fatalf("write reverse-ordered source header %q: %v", entry.name, err)
		}
		if _, err := tarWriter.Write([]byte(entry.contents)); err != nil {
			t.Fatalf("write reverse-ordered source contents %q: %v", entry.name, err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal("close reverse-ordered source tar:", err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal("close reverse-ordered source gzip:", err)
	}
	if err := file.Close(); err != nil {
		t.Fatal("close reverse-ordered source archive:", err)
	}
}

func writeDeepPythonSourceArchive(t *testing.T, archivePath string) {
	t.Helper()
	file, err := os.Create(archivePath)
	if err != nil {
		t.Fatal("create deep Python source archive:", err)
	}
	gzipWriter := gzip.NewWriter(file)
	gzipWriter.Header.ModTime = time.Unix(0, 0).UTC()
	gzipWriter.Header.OS = 255
	tarWriter := tar.NewWriter(gzipWriter)
	writeEntry := func(name string) {
		contents := []byte("x")
		header := &tar.Header{
			Name:     name,
			Mode:     0o755,
			Size:     int64(len(contents)),
			Typeflag: tar.TypeReg,
			ModTime:  time.Unix(0, 0).UTC(),
			Format:   tar.FormatPAX,
		}
		if err := tarWriter.WriteHeader(header); err != nil {
			t.Fatalf("write deep source header %q: %v", name, err)
		}
		if _, err := tarWriter.Write(contents); err != nil {
			t.Fatalf("write deep source contents %q: %v", name, err)
		}
	}
	writeEntry("python/bin/python3")
	deepSegments := strings.Repeat("a/", 1960)
	for index := range 51 {
		writeEntry(fmt.Sprintf("python/deep-%02d/%sfile", index, deepSegments))
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal("close deep source tar:", err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal("close deep source gzip:", err)
	}
	if err := file.Close(); err != nil {
		t.Fatal("close deep source archive:", err)
	}
}

func writeConflictingPythonSourceArchive(t *testing.T, archivePath string) {
	t.Helper()
	file, err := os.Create(archivePath)
	if err != nil {
		t.Fatal("create conflicting Python source archive:", err)
	}
	gzipWriter := gzip.NewWriter(file)
	gzipWriter.Header.ModTime = time.Unix(0, 0).UTC()
	gzipWriter.Header.OS = 255
	tarWriter := tar.NewWriter(gzipWriter)
	for _, name := range []string{"python/a", "python/a/b", "python/bin/python3"} {
		contents := []byte("x")
		header := &tar.Header{
			Name:     name,
			Mode:     0o755,
			Size:     int64(len(contents)),
			Typeflag: tar.TypeReg,
			ModTime:  time.Unix(0, 0).UTC(),
			Format:   tar.FormatPAX,
		}
		if err := tarWriter.WriteHeader(header); err != nil {
			t.Fatalf("write conflicting source header %q: %v", name, err)
		}
		if _, err := tarWriter.Write(contents); err != nil {
			t.Fatalf("write conflicting source contents %q: %v", name, err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal("close conflicting source tar:", err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal("close conflicting source gzip:", err)
	}
	if err := file.Close(); err != nil {
		t.Fatal("close conflicting source archive:", err)
	}
}

func fileSHA256(t *testing.T, path string) string {
	t.Helper()

	file, err := os.Open(path)
	if err != nil {
		t.Fatal("open file for SHA-256:", err)
	}
	defer file.Close()

	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		t.Fatal("calculate SHA-256:", err)
	}
	return hex.EncodeToString(digest.Sum(nil))
}

func readOuterBundle(t *testing.T, bundlePath string) map[string][]byte {
	t.Helper()

	file, err := os.Open(bundlePath)
	if err != nil {
		t.Fatal("open Profile Bundle:", err)
	}
	defer file.Close()

	entries := make(map[string][]byte)
	reader := tar.NewReader(file)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			return entries
		}
		if err != nil {
			t.Fatal("read Profile Bundle:", err)
		}
		contents, err := io.ReadAll(reader)
		if err != nil {
			t.Fatalf("read Profile Bundle entry %q: %v", header.Name, err)
		}
		entries[header.Name] = contents
	}
}

func profileBundleRepositoryRoot(t *testing.T) string {
	t.Helper()

	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate Profile Bundle integration test")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(filename), ".."))
}
