package workspace_test

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestProfileBundleBuildRejectsUnenforceableSystemCallPolicies(t *testing.T) {
	repository := profileBundleRepositoryRoot(t)
	directory := t.TempDir()
	cli := filepath.Join(directory, "profile-bundle.exe")
	build := exec.Command("go", "build", "-o", cli, "./cmd/profile-bundle")
	build.Dir = repository
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build public Profile Bundle command: %v\n%s", err, output)
	}
	archive := filepath.Join(directory, "python-runtime.tar.gz")
	writePythonSourceArchive(t, archive)
	info, err := os.Stat(archive)
	if err != nil {
		t.Fatal(err)
	}
	lock := fmt.Sprintf(`{"schema":"profile-build-lock/v1","profile":"python-data-v1","target":{"os":"linux","arch":"amd64"},"python":{"version":"3.14.7","entrypoint":"/opt/python/bin/python3","packages":[]},"inputs":[{"id":"python-runtime","kind":"python-install-only","version":"3.14.7+test","source":"https://example.invalid/python-runtime.tar.gz","filename":"python-runtime.tar.gz","size":%d,"digest":"sha256:%s","archive_prefix":"python/","destination":"opt/python/","include":[],"exclude":[]}]}`, info.Size(), fileSHA256(t, archive))
	lockPath, initPath, policyPath := filepath.Join(directory, "lock.json"), filepath.Join(directory, "sandbox-init"), filepath.Join(directory, "policy.json")
	for name, contents := range map[string]string{lockPath: lock, initPath: "abc"} {
		if err := os.WriteFile(name, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	valid := string(validCLIFixturePolicy())
	buildPolicy := func(name, policy string) ([]byte, error) {
		t.Helper()
		if err := os.WriteFile(policyPath, []byte(policy), 0o644); err != nil {
			t.Fatal(err)
		}
		return exec.Command(cli, "build", "--lock", lockPath, "--source-cache", directory,
			"--sandbox-init", initPath, "--system-call-policy", policyPath,
			"--output", filepath.Join(directory, name+".bundle")).CombinedOutput()
	}
	// A complete, working fixture makes the rejection assertions independent
	// of missing downloads or an invalid Init/lock failing before the Policy.
	if output, err := buildPolicy("valid", valid); err != nil {
		t.Fatalf("valid control Policy could not build: %v\n%s", err, output)
	}
	oversized := oversizedArgumentPolicy(t, repository)
	for _, test := range []struct {
		name, policy, diagnostic string
	}{
		{"errno-action-bits", strings.Replace(valid, `"errno":1`, `"errno":65536`, 1), "errno"},
		{"duplicate-errno", strings.Replace(valid, `"errno":1`, `"errno":1,"errno":2`, 1), "duplicate"},
		{"case-alias", strings.Replace(valid, `"schema":`, `"Schema":`, 1), "unknown"},
		{"unknown-field", strings.Replace(valid, `"rules":[]`, `"rules":[],"fallback":"allow"`, 1), "unknown"},
		{"unknown-call", strings.Replace(valid, `"rules":[]`, `"rules":[{"names":["not_a_linux_syscall"],"action":"allow","arguments":[]}]`, 1), "unknown"},
		{"invalid-mask", strings.Replace(valid, `"rules":[]`, `"rules":[{"names":["clone"],"action":"allow","arguments":[{"index":0,"operator":"masked-equal","value":2,"mask":1}]}]`, 1), "mask"},
		{"invalid-index", strings.Replace(valid, `"rules":[]`, `"rules":[{"names":["clone"],"action":"allow","arguments":[{"index":6,"operator":"equal","value":0,"mask":0}]}]`, 1), "index"},
		{"wrong-architecture", strings.Replace(valid, `"arch":"amd64"`, `"arch":"arm64"`, 1), "amd64"},
		{"instruction-budget", oversized, "4096"},
	} {
		t.Run(test.name, func(t *testing.T) {
			output, err := buildPolicy(test.name, test.policy)
			if err == nil || !strings.Contains(string(output), test.diagnostic) {
				t.Fatalf("invalid Policy was not rejected for %s: %v\n%s", test.diagnostic, err, output)
			}
			if _, err := os.Stat(filepath.Join(directory, test.name+".bundle")); !os.IsNotExist(err) {
				t.Fatalf("rejected Policy published a Bundle: %v", err)
			}
		})
	}
}

func oversizedArgumentPolicy(t *testing.T, repository string) string {
	t.Helper()
	contents, err := os.ReadFile(filepath.Join(repository, "profiles", "python-data-v1", "system-call-policy.json"))
	if err != nil {
		t.Fatal(err)
	}
	var policy map[string]json.RawMessage
	if err := json.Unmarshal(contents, &policy); err != nil {
		t.Fatal(err)
	}
	var groups []struct {
		Names []string `json:"names"`
	}
	if err := json.Unmarshal(policy["rules"], &groups); err != nil {
		t.Fatal(err)
	}
	arguments := make([]map[string]any, 0, 6)
	for index := range 6 {
		arguments = append(arguments, map[string]any{"index": index, "operator": "greater-than", "value": 0, "mask": 0})
	}
	var rules []map[string]any
	for _, group := range groups {
		for _, name := range group.Names {
			rules = append(rules, map[string]any{"names": []string{name}, "action": "allow", "arguments": arguments})
		}
	}
	policy["rules"], err = json.Marshal(rules)
	if err != nil {
		t.Fatal(err)
	}
	contents, err = json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	return string(contents)
}
