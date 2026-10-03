//go:build linux

package workspace_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestRuntimeLabRejectsInvalidOptionsBeforeAllocating(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "runtime-lab")
	build := exec.Command("go", "build", "-o", binary, "./cmd/runtime-lab")
	build.Dir = repositoryRoot(t)
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build Runtime Lab: %v\n%s", err, output)
	}
	for _, test := range []struct {
		name string
		args []string
		want string
	}{
		{"traversal-id", []string{"-id", "../escape", "10.200.1.2"}, "-id must contain"},
		{"long-id", []string{"-id", "abcdefabcdef", "10.200.1.2"}, "-id must contain"},
		{"bad-ip", []string{"not-an-ip"}, "IP must be an IPv4"},
		{"wrong-subnet", []string{"192.168.1.2"}, "IP must be an IPv4"},
		{"wrong-prefix", []string{"10.200.1.2/16"}, "IP must be an IPv4"},
		{"gateway", []string{"10.200.1.1"}, "bridge, network, or broadcast"},
		{"broadcast", []string{"10.200.1.255"}, "bridge, network, or broadcast"},
		{"negative-cpu", []string{"-c", "-1", "10.200.1.2"}, "-c must be a nonnegative"},
		{"negative-memory", []string{"-m", "-1", "10.200.1.2"}, "-m must be a positive"},
		{"malformed-memory", []string{"-m", "100MB", "10.200.1.2"}, "-m must be a positive"},
		{"overflow-memory", []string{"-m", "18446744073709551616G", "10.200.1.2"}, "-m exceeds"},
		{"missing-child-arguments", []string{"child-mode"}, "child-mode requires"},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			command := exec.Command(binary, test.args...)
			command.Dir = directory
			output, err := command.CombinedOutput()
			if err == nil || command.ProcessState.ExitCode() != 2 {
				t.Fatalf("want usage exit 2, got %v: %s", err, output)
			}
			if !strings.Contains(string(output), test.want) {
				t.Fatalf("want %q in output: %s", test.want, output)
			}
			entries, err := os.ReadDir(directory)
			if err != nil || len(entries) != 0 {
				t.Fatalf("invalid command changed its working directory: %v, %v", entries, err)
			}
		})
	}
}

func TestGoRuntimeLabCommandsExposeHelp(t *testing.T) {
	repositoryRoot := repositoryRoot(t)

	for _, commandPath := range []string{"./cmd/runtime-lab", "./with_Go"} {
		commandPath := commandPath
		t.Run(commandPath, func(t *testing.T) {
			command := exec.Command("go", "run", commandPath, "--help")
			command.Dir = repositoryRoot

			output, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("run %s --help: %v\n%s", commandPath, err, output)
			}

			help := string(output)
			for _, option := range []string{"-c int", "-m string"} {
				if !strings.Contains(help, option) {
					t.Errorf("help for %s does not describe %q:\n%s", commandPath, option, help)
				}
			}
		})
	}
}

func TestShellRuntimeLabScriptsPassSyntaxCheck(t *testing.T) {
	repositoryRoot := repositoryRoot(t)
	bashPath, err := exec.LookPath("bash")
	if err != nil {
		t.Fatal("find bash for Runtime Lab syntax checks:", err)
	}

	for _, scriptPath := range []string{
		"with_shell/net-setup.sh",
		"with_shell/rm-container.sh",
		"with_shell/run-mini-container.sh",
	} {
		scriptPath := scriptPath
		t.Run(scriptPath, func(t *testing.T) {
			command := exec.Command(bashPath, "-n", scriptPath)
			command.Dir = repositoryRoot

			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("bash -n %s: %v\n%s", scriptPath, err, output)
			}
		})
	}
}

func repositoryRoot(t *testing.T) string {
	t.Helper()

	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate Runtime Lab integration test")
	}

	return filepath.Clean(filepath.Join(filepath.Dir(filename), ".."))
}
