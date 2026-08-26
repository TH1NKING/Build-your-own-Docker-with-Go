//go:build linux

package workspace_test

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

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
