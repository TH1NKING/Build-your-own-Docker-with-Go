//go:build linux && profilebundle_production

package workspace_test

import (
	"bytes"
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPythonDataV1ProductionBuildIsByteReproducible(t *testing.T) {
	cliPath := requiredEnvironment(t, "PROFILE_BUNDLE_CLI")
	repositoryRoot := requiredEnvironment(t, "PROFILE_BUNDLE_REPOSITORY_ROOT")
	sourceCache := requiredEnvironment(t, "PROFILE_BUNDLE_SOURCE_CACHE")

	temporaryDirectory := t.TempDir()
	initPath := productionSandboxInit(t)
	bundlePaths := []string{
		filepath.Join(temporaryDirectory, "first.bundle"),
		filepath.Join(temporaryDirectory, "second.bundle"),
	}
	reportedDigests := make([]string, len(bundlePaths))
	for index, bundlePath := range bundlePaths {
		command := exec.Command(
			cliPath, "build",
			"--lock", filepath.Join(repositoryRoot, "profiles", "python-data-v1", "profile.lock.json"),
			"--source-cache", sourceCache,
			"--system-call-policy", filepath.Join(repositoryRoot, "profiles", "python-data-v1", "system-call-policy.json"),
			"--sandbox-init", initPath,
			"--output", bundlePath,
		)
		if index == 0 {
			command.Env = append(os.Environ(), "TZ=UTC")
		} else {
			command.Env = append(os.Environ(), "TZ=Asia/Shanghai")
		}
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("build production python-data-v1 Profile Bundle %d: %v\n%s", index+1, err, output)
		}
		reportedDigests[index] = strings.TrimSpace(string(output))
	}

	first, err := os.ReadFile(bundlePaths[0])
	if err != nil {
		t.Fatal("read first production Profile Bundle:", err)
	}
	second, err := os.ReadFile(bundlePaths[1])
	if err != nil {
		t.Fatal("read second production Profile Bundle:", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("identical locked inputs produced different Profile Bundle bytes")
	}
	digest := sha256.Sum256(first)
	wantDigest := hex.EncodeToString(digest[:])
	if reportedDigests[0] != wantDigest || reportedDigests[1] != wantDigest {
		t.Fatalf("reported production Bundle digests = %#v, want %q", reportedDigests, wantDigest)
	}
	var manifest struct {
		Components []struct {
			Role   string `json:"role"`
			Digest string `json:"digest"`
		} `json:"components"`
	}
	if err := json.Unmarshal(readOuterBundle(t, bundlePaths[0])["manifest.json"], &manifest); err != nil {
		t.Fatal("decode production Profile Bundle manifest:", err)
	}
	wantComponentDigests := map[string]string{
		"rootfs":             "sha256:cdfacca4d70a2045c924b491e9d7516fbf3c67dbce950b70f1207c204d3a46aa",
		"system-call-policy": "sha256:6047a041a38f29123ac52175fdcbf7b6597645f0706124a34e2b51a07a7ce22d",
		"sandbox-init":       "sha256:" + fileSHA256(t, initPath),
	}
	for _, component := range manifest.Components {
		if want, ok := wantComponentDigests[component.Role]; ok {
			if component.Digest != want {
				t.Fatalf("production %s digest = %q, want reviewed %q", component.Role, component.Digest, want)
			}
			delete(wantComponentDigests, component.Role)
		}
	}
	if len(wantComponentDigests) != 0 {
		t.Fatalf("production manifest omitted reviewed component digests: %#v", wantComponentDigests)
	}
}

func TestPythonDataV1ProductionRuntimeProvidesLockedStandardLibrary(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Fatal("profilebundle_production tests must run as root on a disposable Linux environment")
	}
	cliPath := requiredEnvironment(t, "PROFILE_BUNDLE_CLI")
	repositoryRoot := requiredEnvironment(t, "PROFILE_BUNDLE_REPOSITORY_ROOT")
	sourceCache := requiredEnvironment(t, "PROFILE_BUNDLE_SOURCE_CACHE")

	temporaryDirectory := t.TempDir()
	initPath := productionSandboxInit(t)
	bundlePath := filepath.Join(temporaryDirectory, "python-data-v1.bundle")
	build := exec.Command(
		cliPath, "build",
		"--lock", filepath.Join(repositoryRoot, "profiles", "python-data-v1", "profile.lock.json"),
		"--source-cache", sourceCache,
		"--system-call-policy", filepath.Join(repositoryRoot, "profiles", "python-data-v1", "system-call-policy.json"),
		"--sandbox-init", initPath,
		"--output", bundlePath,
	)
	buildOutput, err := build.CombinedOutput()
	if err != nil {
		t.Fatalf("build production python-data-v1 Profile Bundle: %v\n%s", err, buildOutput)
	}
	bundleDigest := strings.TrimSpace(string(buildOutput))

	storePath := filepath.Join(temporaryDirectory, "profile-store")
	if err := os.Mkdir(storePath, 0o755); err != nil {
		t.Fatal("create production Profile store:", err)
	}
	install := exec.Command(
		cliPath, "install",
		"--bundle", bundlePath,
		"--expected-sha256", bundleDigest,
		"--store", storePath,
	)
	if output, err := install.CombinedOutput(); err != nil {
		t.Fatalf("install production python-data-v1 Profile Bundle: %v\n%s", err, output)
	}

	rootfsPath := filepath.Join(storePath, "sha256", bundleDigest, "rootfs")
	if got, want := fileSHA256(t, filepath.Join(rootfsPath, "sandbox-init")), fileSHA256(t, initPath); got != want {
		t.Fatalf("installed production Sandbox Init SHA-256 = %q, want built executable %q", got, want)
	}
	smoke := strings.Join([]string{
		"import csv, importlib.util, json, platform, sqlite3",
		"assert platform.python_version() == '3.14.7'",
		"assert importlib.util.find_spec('pip') is None",
		"assert importlib.util.find_spec('ensurepip') is None",
		"assert json.loads('{\"answer\": 42}')['answer'] == 42",
		"assert next(csv.reader(['a,b'])) == ['a', 'b']",
		"assert sqlite3.connect(':memory:').execute('select 42').fetchone()[0] == 42",
		"print('profile-smoke-ok')",
	}, "; ")
	python := exec.Command("chroot", rootfsPath, "/opt/python/bin/python3", "-I", "-c", smoke)
	output, err := python.CombinedOutput()
	if err != nil {
		t.Fatalf("run production python-data-v1 content smoke: %v\n%s", err, output)
	}
	if string(output) != "profile-smoke-ok\n" {
		t.Fatalf("production python-data-v1 smoke output = %q, want %q", output, "profile-smoke-ok\n")
	}
}

func productionSandboxInit(t *testing.T) string {
	t.Helper()
	initPath := requiredEnvironment(t, "PROFILE_BUNDLE_SANDBOX_INIT")
	executable, err := elf.Open(initPath)
	if err != nil {
		t.Fatal("open production Sandbox Init ELF:", err)
	}
	defer executable.Close()
	if executable.Machine != elf.EM_X86_64 {
		t.Fatal("production Sandbox Init must target linux/amd64")
	}
	for _, program := range executable.Progs {
		if program.Type == elf.PT_INTERP {
			t.Fatal("production Sandbox Init must be statically linked")
		}
	}
	return initPath
}

func requiredEnvironment(t *testing.T, name string) string {
	t.Helper()
	value := os.Getenv(name)
	if value == "" {
		t.Fatalf("%s must be set", name)
	}
	return value
}
