//go:build linux

package workspace_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSandboxRunnerRequiresCompletePassingContracts(t *testing.T) {
	for _, test := range []struct {
		name string
		log  string
		pass bool
	}{
		{"complete", "--- PASS: TestContract (0.01s)\nPASS\n", true},
		{"zero-tests", "testing: warning: no tests to run\nPASS\n", false},
		{"missing-final-pass", "--- PASS: TestContract (0.01s)\n", false},
		{"stale-contract", "--- PASS: TestOldContract (0.01s)\nPASS\n", false},
		{"same-prefix", "--- PASS: TestContractOther (0.01s)\nPASS\n", false},
		{"skip", "--- SKIP: TestContract (0.01s)\nPASS\n", false},
		{"skipped-child", "    --- SKIP: TestContract/probe (0.01s)\n--- PASS: TestContract (0.01s)\nPASS\n", false},
		{"failed-child", "    --- FAIL: TestContract/probe (0.01s)\n--- PASS: TestContract (0.01s)\nPASS\n", false},
		{"child-only", "    --- PASS: TestContract/probe (0.01s)\nPASS\n", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			logFile := filepath.Join(t.TempDir(), "execution.log")
			if err := os.WriteFile(logFile, []byte(test.log), 0o600); err != nil {
				t.Fatal(err)
			}
			command := exec.Command("bash", "-c", `set -euo pipefail; source "$1"; sandbox_require_passed_tests "$2" TestContract`, "test", filepath.Join(repositoryRoot(t), "tests", "sandbox-test-result.sh"), logFile)
			output, err := command.CombinedOutput()
			if (err == nil) != test.pass {
				t.Fatalf("pass=%t, want %t: %v\n%s", err == nil, test.pass, err, output)
			}
		})
	}
}

func TestSandboxRunnerPreservesRunnerAndLogFailures(t *testing.T) {
	for _, test := range []struct {
		name    string
		logFile string
		command string
		code    int
	}{
		{"runner-fails-after-pass", filepath.Join(t.TempDir(), "execution.log"), "printf 'PASS\\n'; exit 23", 23},
		{"log-write-fails", "/dev/full", "printf 'PASS\\n'", 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			command := exec.Command("bash", "-c", `set -euo pipefail; source "$1"; sandbox_run_logged "$2" bash -c "$3"`, "test", filepath.Join(repositoryRoot(t), "tests", "sandbox-test-result.sh"), test.logFile, test.command)
			output, err := command.CombinedOutput()
			exit, ok := err.(*exec.ExitError)
			if !ok || exit.ExitCode() != test.code {
				t.Fatalf("exit=%v, want %d\n%s", err, test.code, output)
			}
		})
	}
}

func TestSandboxDemoRejectsMissingContractsBeforePrivilege(t *testing.T) {
	for _, test := range []struct {
		name      string
		inventory string
		message   string
	}{
		{"empty-binary", "", "No Sandbox tests match"},
		{"stale-binary", "TestSandboxExecutionAttachmentsProcessReadOnlyInputAndExtractOutput\n", "Missing required contract in test binary"},
	} {
		t.Run(test.name, func(t *testing.T) {
			bin := t.TempDir()
			for _, name := range []string{"sandboxd", "sandbox-init", "sandbox-waiting-init", "profile-bundle"} {
				if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\nexit 99\n"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			// A harmless binary double: no namespace, mount or cgroup operation
			// should be reached when a prepared binary lacks the demo inventory.
			stub := "#!/bin/sh\ncat <<'INVENTORY'\n" + test.inventory + "INVENTORY\n"
			if err := os.WriteFile(filepath.Join(bin, "t05-tests"), []byte(stub), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(bin, "python.bundle"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			command := exec.Command("bash", filepath.Join(repositoryRoot(t), "tests", "run-sandbox-file-demo-linux.sh"), "all")
			command.Env = append(os.Environ(), "SANDBOX_TEST_BIN_DIR="+bin, "SANDBOX_TEST_RUN=ambient-selector-must-be-overridden")
			output, err := command.CombinedOutput()
			if err == nil || !strings.Contains(string(output), test.message) {
				t.Fatalf("expected early inventory failure %q: %v\n%s", test.message, err, output)
			}
		})
	}
}
