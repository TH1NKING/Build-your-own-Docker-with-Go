//go:build linux

package workspace_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/TH1NKING/Build-your-own-Docker-with-Go/internal/sandboxsupervisor"
)

func TestSandboxSupervisorClientReceivesStructuredProfileNotFound(t *testing.T) {
	fixture := newSafeSandboxSupervisorFixture(t)
	stopSupervisor := startSandboxSupervisor(t, fixture)
	t.Cleanup(stopSupervisor)

	requestContext, cancelRequest := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelRequest()

	client := sandboxsupervisor.NewClient(fixture.socketPath)
	response, err := client.CreateSandbox(requestContext, sandboxsupervisor.CreateSandboxRequest{
		RequestID:       "req-001",
		SandboxID:       "run-001",
		ProfileIdentity: "sha256:" + strings.Repeat("0", 64),
	})
	if err != nil {
		t.Fatal("request Sandbox creation through the public Supervisor client:", err)
	}

	if response.Schema != "sandbox-supervisor-response/v1" {
		t.Fatalf("response schema = %q, want sandbox-supervisor-response/v1", response.Schema)
	}
	if response.RequestID != "req-001" {
		t.Fatalf("response request ID = %q, want req-001", response.RequestID)
	}
	if response.Result != nil {
		t.Fatalf("missing Profile unexpectedly produced a result: %#v", response.Result)
	}
	if response.Error == nil {
		t.Fatal("missing Profile produced no structured protocol error")
	}
	if response.Error.Code != "profile_not_found" {
		t.Fatalf(
			"missing Profile error code = %q, want profile_not_found",
			response.Error.Code,
		)
	}
}

func TestSandboxSupervisorRejectsUnsupportedProtocolVersion(t *testing.T) {
	fixture := startEmptySandboxSupervisor(t)
	responsePayload := exchangeSandboxSupervisorMessage(t, fixture.socketPath, []byte(`{
  "schema": "sandbox-supervisor-request/v2",
  "request_id": "req-version",
  "operation": "create_sandbox",
  "parameters": {
    "sandbox_id": "run-001",
    "profile_identity": "sha256:0000000000000000000000000000000000000000000000000000000000000000"
  }
}`))

	var response struct {
		Schema    string `json:"schema"`
		RequestID string `json:"request_id"`
		Result    any    `json:"result"`
		Error     *struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(responsePayload, &response); err != nil {
		t.Fatal("decode unsupported-version response:", err)
	}
	if response.Schema != "sandbox-supervisor-response/v1" || response.RequestID != "req-version" {
		t.Fatalf("unsupported-version response identity = %#v", response)
	}
	if response.Result != nil {
		t.Fatalf("unsupported protocol version unexpectedly produced a result: %#v", response.Result)
	}
	if response.Error == nil || response.Error.Code != "unsupported_version" {
		t.Fatalf("unsupported protocol version response error = %#v, want unsupported_version", response.Error)
	}
}

func TestSandboxSupervisorRejectsUnknownOperation(t *testing.T) {
	fixture := startEmptySandboxSupervisor(t)
	responsePayload := exchangeSandboxSupervisorMessage(t, fixture.socketPath, []byte(`{
  "schema": "sandbox-supervisor-request/v1",
  "request_id": "req-operation",
  "operation": "run_host_command",
  "parameters": {
    "sandbox_id": "run-001",
    "profile_identity": "sha256:0000000000000000000000000000000000000000000000000000000000000000"
  }
}`))

	var response struct {
		Schema    string `json:"schema"`
		RequestID string `json:"request_id"`
		Result    any    `json:"result"`
		Error     *struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(responsePayload, &response); err != nil {
		t.Fatal("decode unknown-operation response:", err)
	}
	if response.Schema != "sandbox-supervisor-response/v1" || response.RequestID != "req-operation" {
		t.Fatalf("unknown-operation response identity = %#v", response)
	}
	if response.Result != nil {
		t.Fatalf("unknown operation unexpectedly produced a result: %#v", response.Result)
	}
	if response.Error == nil || response.Error.Code != "unknown_operation" {
		t.Fatalf("unknown operation response error = %#v, want unknown_operation", response.Error)
	}
}

func TestSandboxSupervisorRejectsMalformedRequest(t *testing.T) {
	fixture := startEmptySandboxSupervisor(t)
	validProfileIdentity := "sha256:" + strings.Repeat("0", 64)
	tests := []struct {
		name    string
		payload string
	}{
		{
			name:    "truncated JSON",
			payload: `{"schema":"sandbox-supervisor-request/v1"`,
		},
		{
			name: "unknown host path field",
			payload: `{
  "schema":"sandbox-supervisor-request/v1",
  "request_id":"req-unknown-field",
  "operation":"create_sandbox",
  "parameters":{"sandbox_id":"run-001","profile_identity":"` + validProfileIdentity + `"},
  "workspace_path":"/etc"
}`,
		},
		{
			name: "unknown operation parameter",
			payload: `{
  "schema":"sandbox-supervisor-request/v1",
  "request_id":"req-unknown-parameter",
  "operation":"create_sandbox",
  "parameters":{
    "sandbox_id":"run-001",
    "profile_identity":"` + validProfileIdentity + `",
    "host_path":"/etc"
  }
}`,
		},
		{
			name: "duplicate operation field",
			payload: `{
  "schema":"sandbox-supervisor-request/v1",
  "request_id":"req-duplicate",
  "operation":"create_sandbox",
  "operation":"run_host_command",
  "parameters":{"sandbox_id":"run-001","profile_identity":"` + validProfileIdentity + `"}
}`,
		},
		{
			name: "noncanonical envelope field case",
			payload: `{
  "schema":"sandbox-supervisor-request/v1",
  "request_id":"req-field-case",
  "Operation":"create_sandbox",
  "parameters":{"sandbox_id":"run-001","profile_identity":"` + validProfileIdentity + `"}
}`,
		},
		{
			name: "case-variant semantic duplicate",
			payload: `{
  "schema":"sandbox-supervisor-request/v1",
  "request_id":"req-case-duplicate",
  "operation":"create_sandbox",
  "Operation":"run_host_command",
  "parameters":{"sandbox_id":"run-001","profile_identity":"` + validProfileIdentity + `"}
}`,
		},
		{
			name: "noncanonical parameter field case",
			payload: `{
  "schema":"sandbox-supervisor-request/v1",
  "request_id":"req-parameter-case",
  "operation":"create_sandbox",
  "parameters":{"Sandbox_ID":"run-001","profile_identity":"` + validProfileIdentity + `"}
}`,
		},
		{
			name: "trailing JSON value",
			payload: `{
  "schema":"sandbox-supervisor-request/v1",
  "request_id":"req-trailing",
  "operation":"create_sandbox",
  "parameters":{"sandbox_id":"run-001","profile_identity":"` + validProfileIdentity + `"}
} {}`,
		},
		{
			name: "missing parameters",
			payload: `{
  "schema":"sandbox-supervisor-request/v1",
  "request_id":"req-missing",
  "operation":"create_sandbox"
}`,
		},
		{
			name: "overlong request ID",
			payload: `{
  "schema":"sandbox-supervisor-request/v1",
  "request_id":"` + strings.Repeat("r", 65) + `",
  "operation":"create_sandbox",
  "parameters":{"sandbox_id":"run-001","profile_identity":"` + validProfileIdentity + `"}
}`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			responsePayload := exchangeSandboxSupervisorMessage(t, fixture.socketPath, []byte(test.payload))
			var response struct {
				Schema string `json:"schema"`
				Result any    `json:"result"`
				Error  *struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(responsePayload, &response); err != nil {
				t.Fatal("decode malformed-request response:", err)
			}
			if response.Schema != "sandbox-supervisor-response/v1" {
				t.Fatalf("malformed-request response schema = %q", response.Schema)
			}
			if response.Result != nil {
				t.Fatalf("malformed request unexpectedly produced a result: %#v", response.Result)
			}
			if response.Error == nil || response.Error.Code != "malformed_request" {
				t.Fatalf("malformed request response error = %#v, want malformed_request", response.Error)
			}
		})
	}
}

func TestSandboxSupervisorRejectsEscapingProfileReference(t *testing.T) {
	fixture := startEmptySandboxSupervisor(t)
	validProfileIdentity := "sha256:" + strings.Repeat("0", 64)
	tests := []struct {
		name            string
		sandboxID       string
		profileIdentity string
	}{
		{name: "absolute Profile path", sandboxID: "run-001", profileIdentity: "/etc"},
		{name: "traversing Profile path", sandboxID: "run-001", profileIdentity: "../../outside"},
		{name: "noncanonical Profile digest", sandboxID: "run-001", profileIdentity: "sha256:" + strings.Repeat("A", 64)},
		{name: "traversing Sandbox ID", sandboxID: "../run-001", profileIdentity: validProfileIdentity},
		{name: "backslash Sandbox ID", sandboxID: `run\outside`, profileIdentity: validProfileIdentity},
		{name: "NUL Sandbox ID", sandboxID: "run\x00outside", profileIdentity: validProfileIdentity},
		{name: "overlong Sandbox ID", sandboxID: strings.Repeat("a", 65), profileIdentity: validProfileIdentity},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			responsePayload := exchangeSandboxSupervisorMessage(
				t,
				fixture.socketPath,
				createSandboxWireRequest(t, "req-reference", test.sandboxID, test.profileIdentity),
			)
			assertSandboxSupervisorErrorCode(t, responsePayload, "invalid_reference")
		})
	}

	t.Run("Profile symlink escapes configured store", func(t *testing.T) {
		digest := strings.Repeat("0", 64)
		outsideProfile := t.TempDir()
		profilePath := filepath.Join(fixture.profileStore, "sha256", digest)
		if err := os.Symlink(outsideProfile, profilePath); err != nil {
			t.Fatal("create escaping Profile symlink:", err)
		}
		responsePayload := exchangeSandboxSupervisorMessage(
			t,
			fixture.socketPath,
			createSandboxWireRequest(t, "req-symlink", "run-001", "sha256:"+digest),
		)
		assertSandboxSupervisorErrorCode(t, responsePayload, "invalid_reference")
	})
}

func TestSandboxSupervisorPublishesRestrictedUnixSocket(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Fatal("Sandbox Supervisor protocol acceptance must run as an unprivileged client")
	}
	fixture := startEmptySandboxSupervisor(t)

	socketInfo, err := os.Lstat(fixture.socketPath)
	if err != nil {
		t.Fatal("stat published Sandbox Supervisor socket:", err)
	}
	if socketInfo.Mode()&os.ModeSocket == 0 {
		t.Fatalf("published Supervisor endpoint mode = %v, want Unix socket", socketInfo.Mode())
	}
	if permissions := socketInfo.Mode().Perm(); permissions != 0o660 {
		t.Fatalf("Sandbox Supervisor socket permissions = %04o, want 0660", permissions)
	}
	socketStat, ok := socketInfo.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatal("Sandbox Supervisor socket has no Linux ownership metadata")
	}
	if int(socketStat.Uid) != os.Geteuid() || int(socketStat.Gid) != os.Getegid() {
		t.Fatalf(
			"Sandbox Supervisor socket owner = %d:%d, want process owner %d:%d",
			socketStat.Uid,
			socketStat.Gid,
			os.Geteuid(),
			os.Getegid(),
		)
	}

	directoryInfo, err := os.Stat(filepath.Dir(fixture.socketPath))
	if err != nil {
		t.Fatal("stat Sandbox Supervisor socket directory:", err)
	}
	if permissions := directoryInfo.Mode().Perm(); permissions != 0o750 {
		t.Fatalf("Sandbox Supervisor socket directory permissions = %04o, want 0750", permissions)
	}

	responsePayload := exchangeSandboxSupervisorMessage(
		t,
		fixture.socketPath,
		createSandboxWireRequest(t, "req-permissions", "run-001", "sha256:"+strings.Repeat("0", 64)),
	)
	assertSandboxSupervisorErrorCode(t, responsePayload, "profile_not_found")
}

func TestSandboxSupervisorRejectsOversizedRequest(t *testing.T) {
	fixture := startEmptySandboxSupervisor(t)
	responsePayload := exchangeSandboxSupervisorDeclaredFrame(t, fixture.socketPath, 64<<10+1, nil)
	assertSandboxSupervisorErrorCode(t, responsePayload, "request_too_large")

	recoveryResponse := exchangeSandboxSupervisorMessage(
		t,
		fixture.socketPath,
		createSandboxWireRequest(t, "req-after-oversize", "run-001", "sha256:"+strings.Repeat("0", 64)),
	)
	assertSandboxSupervisorErrorCode(t, recoveryResponse, "profile_not_found")
}

func TestSandboxSupervisorPreservesUnexpectedSocketPathEntry(t *testing.T) {
	commandPath := sandboxdCommandPath(t)

	tests := []struct {
		name   string
		create func(t *testing.T, socketPath string) func(t *testing.T, socketPath string)
	}{
		{
			name: "regular file",
			create: func(t *testing.T, socketPath string) func(t *testing.T, socketPath string) {
				if err := os.WriteFile(socketPath, []byte("sentinel"), 0o640); err != nil {
					t.Fatal("create socket-path sentinel file:", err)
				}
				return func(t *testing.T, socketPath string) {
					contents, err := os.ReadFile(socketPath)
					if err != nil || string(contents) != "sentinel" {
						t.Fatalf("socket-path sentinel after rejected start = %q, err=%v", contents, err)
					}
				}
			},
		},
		{
			name: "directory",
			create: func(t *testing.T, socketPath string) func(t *testing.T, socketPath string) {
				if err := os.Mkdir(socketPath, 0o750); err != nil {
					t.Fatal("create directory at socket path:", err)
				}
				return func(t *testing.T, socketPath string) {
					info, err := os.Stat(socketPath)
					if err != nil || !info.IsDir() {
						t.Fatalf("socket-path directory was not preserved: info=%#v err=%v", info, err)
					}
				}
			},
		},
		{
			name: "symlink",
			create: func(t *testing.T, socketPath string) func(t *testing.T, socketPath string) {
				target := filepath.Join(filepath.Dir(socketPath), "target")
				if err := os.WriteFile(target, []byte("safe"), 0o640); err != nil {
					t.Fatal("create socket symlink target:", err)
				}
				if err := os.Symlink(target, socketPath); err != nil {
					t.Fatal("create symlink at socket path:", err)
				}
				return func(t *testing.T, socketPath string) {
					gotTarget, err := os.Readlink(socketPath)
					if err != nil || gotTarget != target {
						t.Fatalf("socket-path symlink target = %q, want %q, err=%v", gotTarget, target, err)
					}
					contents, err := os.ReadFile(target)
					if err != nil || string(contents) != "safe" {
						t.Fatalf("socket symlink target after rejected start = %q, err=%v", contents, err)
					}
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newSafeSandboxSupervisorFixture(t)
			verify := test.create(t, fixture.socketPath)

			assertSandboxdRejectsStartup(t, commandPath, fixture, 2*time.Second, "existing "+test.name)
			verify(t, fixture.socketPath)
		})
	}
}

func TestSandboxSupervisorRejectsWritableRuntimeRoot(t *testing.T) {
	commandPath := sandboxdCommandPath(t)

	for _, rootName := range []string{"socket directory", "Profile store", "Sandbox root"} {
		t.Run(rootName, func(t *testing.T) {
			fixture := newSafeSandboxSupervisorFixture(t)
			unsafeRoot := map[string]string{
				"socket directory": filepath.Dir(fixture.socketPath),
				"Profile store":    fixture.profileStore,
				"Sandbox root":     fixture.sandboxRoot,
			}[rootName]
			if err := os.Chmod(unsafeRoot, 0o770); err != nil {
				t.Fatalf("make %s group-writable: %v", rootName, err)
			}

			assertSandboxdRejectsStartup(t, commandPath, fixture, time.Second, "group-writable "+rootName)
		})
	}
}

func TestSandboxSupervisorRejectsSocketDirectoryWithoutWorkerAccess(t *testing.T) {
	fixture := newSafeSandboxSupervisorFixture(t)
	socketDirectory := filepath.Dir(fixture.socketPath)
	if err := os.Chmod(socketDirectory, 0o700); err != nil {
		t.Fatal("remove Worker group access from socket directory:", err)
	}

	assertSandboxdRejectsStartup(
		t,
		sandboxdCommandPath(t),
		fixture,
		time.Second,
		"socket directory without Worker group access",
	)
}

func TestSandboxSupervisorServesAnotherClientWhileRequestIsIncomplete(t *testing.T) {
	fixture := startEmptySandboxSupervisor(t)
	slowConnection, err := net.DialTimeout("unix", fixture.socketPath, 5*time.Second)
	if err != nil {
		t.Fatal("connect incomplete Sandbox Supervisor client:", err)
	}
	defer slowConnection.Close()
	if _, err := slowConnection.Write([]byte{0, 0}); err != nil {
		t.Fatal("write partial Sandbox Supervisor frame header:", err)
	}

	started := time.Now()
	responsePayload := exchangeSandboxSupervisorMessage(
		t,
		fixture.socketPath,
		createSandboxWireRequest(t, "req-concurrent", "run-001", "sha256:"+strings.Repeat("0", 64)),
	)
	if elapsed := time.Since(started); elapsed >= time.Second {
		t.Fatalf("complete request waited %v behind an incomplete client, want under one second", elapsed)
	}
	assertSandboxSupervisorErrorCode(t, responsePayload, "profile_not_found")
}

func TestSandboxSupervisorDoesNotClaimSandboxCreationBeforeIsolationExists(t *testing.T) {
	fixture := startEmptySandboxSupervisor(t)
	digest := strings.Repeat("1", 64)
	if err := os.Mkdir(filepath.Join(fixture.profileStore, "sha256", digest), 0o550); err != nil {
		t.Fatal("create installed Profile fixture:", err)
	}

	responsePayload := exchangeSandboxSupervisorMessage(
		t,
		fixture.socketPath,
		createSandboxWireRequest(t, "req-existing-profile", "run-001", "sha256:"+digest),
	)
	assertSandboxSupervisorErrorCode(t, responsePayload, "operation_unavailable")
}

func createSandboxWireRequest(t *testing.T, requestID, sandboxID, profileIdentity string) []byte {
	t.Helper()

	request := struct {
		Schema     string `json:"schema"`
		RequestID  string `json:"request_id"`
		Operation  string `json:"operation"`
		Parameters struct {
			SandboxID       string `json:"sandbox_id"`
			ProfileIdentity string `json:"profile_identity"`
		} `json:"parameters"`
	}{
		Schema:    "sandbox-supervisor-request/v1",
		RequestID: requestID,
		Operation: "create_sandbox",
	}
	request.Parameters.SandboxID = sandboxID
	request.Parameters.ProfileIdentity = profileIdentity
	payload, err := json.Marshal(request)
	if err != nil {
		t.Fatal("encode raw Sandbox creation request:", err)
	}
	return payload
}

func assertSandboxSupervisorErrorCode(t *testing.T, responsePayload []byte, wantCode string) {
	t.Helper()

	var response struct {
		Result any `json:"result"`
		Error  *struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(responsePayload, &response); err != nil {
		t.Fatal("decode Sandbox Supervisor error response:", err)
	}
	if response.Result != nil {
		t.Fatalf("rejected Sandbox Supervisor request unexpectedly produced a result: %#v", response.Result)
	}
	if response.Error == nil || response.Error.Code != wantCode {
		t.Fatalf("Sandbox Supervisor error = %#v, want %s", response.Error, wantCode)
	}
}

type sandboxSupervisorFixture struct {
	socketPath   string
	profileStore string
	sandboxRoot  string
}

func shortSandboxSupervisorTemporaryDirectory(t *testing.T) string {
	t.Helper()

	directory, err := os.MkdirTemp("", "sd-")
	if err != nil {
		t.Fatal("create short Sandbox Supervisor temporary directory:", err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(directory); err != nil {
			t.Errorf("remove Sandbox Supervisor temporary directory %q: %v", directory, err)
		}
	})
	return directory
}

func newSafeSandboxSupervisorFixture(t *testing.T) sandboxSupervisorFixture {
	t.Helper()

	temporaryDirectory := shortSandboxSupervisorTemporaryDirectory(t)
	fixture := sandboxSupervisorFixture{
		socketPath:   filepath.Join(temporaryDirectory, "run", "sandboxd.sock"),
		profileStore: filepath.Join(temporaryDirectory, "profiles"),
		sandboxRoot:  filepath.Join(temporaryDirectory, "sandboxes"),
	}
	for _, directory := range []string{
		filepath.Dir(fixture.socketPath),
		filepath.Join(fixture.profileStore, "sha256"),
		fixture.sandboxRoot,
	} {
		if err := os.MkdirAll(directory, 0o750); err != nil {
			t.Fatalf("create Sandbox Supervisor fixture directory %q: %v", directory, err)
		}
	}
	return fixture
}

func startEmptySandboxSupervisor(t *testing.T) sandboxSupervisorFixture {
	t.Helper()

	fixture := newSafeSandboxSupervisorFixture(t)
	stopSupervisor := startSandboxSupervisor(t, fixture)
	t.Cleanup(stopSupervisor)
	return fixture
}

func exchangeSandboxSupervisorMessage(t *testing.T, socketPath string, requestPayload []byte) []byte {
	t.Helper()

	return exchangeSandboxSupervisorDeclaredFrame(t, socketPath, uint32(len(requestPayload)), requestPayload)
}

func exchangeSandboxSupervisorDeclaredFrame(
	t *testing.T,
	socketPath string,
	declaredLength uint32,
	payload []byte,
) []byte {
	t.Helper()

	connection, err := net.DialTimeout("unix", socketPath, 5*time.Second)
	if err != nil {
		t.Fatal("connect to Sandbox Supervisor socket:", err)
	}
	defer connection.Close()
	if err := connection.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal("set raw Sandbox Supervisor request deadline:", err)
	}

	var header [4]byte
	binary.BigEndian.PutUint32(header[:], declaredLength)
	if _, err := connection.Write(header[:]); err != nil {
		t.Fatal("write declared Sandbox Supervisor request length:", err)
	}
	if len(payload) > 0 {
		if _, err := connection.Write(payload); err != nil {
			t.Fatal("write declared Sandbox Supervisor request payload:", err)
		}
	}

	if _, err := io.ReadFull(connection, header[:]); err != nil {
		t.Fatal("read Sandbox Supervisor response length:", err)
	}
	responseLength := binary.BigEndian.Uint32(header[:])
	if responseLength == 0 || responseLength > 64<<10 {
		t.Fatalf("Sandbox Supervisor response length = %d, want 1..65536", responseLength)
	}
	responsePayload := make([]byte, responseLength)
	if _, err := io.ReadFull(connection, responsePayload); err != nil {
		t.Fatal("read Sandbox Supervisor response payload:", err)
	}
	return responsePayload
}

func sandboxdCommandPath(t *testing.T) string {
	t.Helper()

	commandPath := os.Getenv("SANDBOXD_CLI")
	if commandPath != "" {
		return commandPath
	}

	commandPath = filepath.Join(t.TempDir(), "sandboxd")
	build := exec.Command("go", "build", "-o", commandPath, "./cmd/sandboxd")
	build.Dir = repositoryRoot(t)
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build public sandboxd command: %v\n%s", err, output)
	}
	return commandPath
}

func assertSandboxdRejectsStartup(
	t *testing.T,
	commandPath string,
	fixture sandboxSupervisorFixture,
	timeout time.Duration,
	reason string,
) {
	t.Helper()

	commandContext, cancelCommand := context.WithTimeout(context.Background(), timeout)
	defer cancelCommand()
	command := exec.CommandContext(
		commandContext,
		commandPath,
		"--socket", fixture.socketPath,
		"--profile-store", fixture.profileStore,
		"--sandbox-root", fixture.sandboxRoot,
	)
	if output, err := command.CombinedOutput(); err == nil {
		t.Fatalf("sandboxd accepted %s\n%s", reason, output)
	} else if errors.Is(commandContext.Err(), context.DeadlineExceeded) {
		t.Fatalf("sandboxd did not reject %s before serving\n%s", reason, output)
	}
}

func startSandboxSupervisor(t *testing.T, fixture sandboxSupervisorFixture) func() {
	t.Helper()

	commandPath := sandboxdCommandPath(t)

	var output bytes.Buffer
	command := exec.Command(
		commandPath,
		"--socket", fixture.socketPath,
		"--profile-store", fixture.profileStore,
		"--sandbox-root", fixture.sandboxRoot,
	)
	command.Stdout = &output
	command.Stderr = &output
	if err := command.Start(); err != nil {
		t.Fatal("start public sandboxd command:", err)
	}

	exited := make(chan error, 1)
	go func() {
		exited <- command.Wait()
	}()

	startupDeadline := time.Now().Add(5 * time.Second)
	for {
		info, err := os.Lstat(fixture.socketPath)
		if err == nil && info.Mode()&os.ModeSocket != 0 {
			break
		}
		select {
		case err := <-exited:
			t.Fatalf("sandboxd exited before publishing its Unix socket: %v\n%s", err, output.String())
		default:
		}
		if time.Now().After(startupDeadline) {
			_ = command.Process.Kill()
			<-exited
			t.Fatalf("sandboxd did not publish its Unix socket within five seconds\n%s", output.String())
		}
		time.Sleep(10 * time.Millisecond)
	}

	return func() {
		if err := command.Process.Signal(os.Interrupt); err != nil {
			t.Errorf("stop sandboxd: %v", err)
			return
		}
		select {
		case err := <-exited:
			if err != nil {
				t.Errorf("sandboxd exited after interrupt: %v\n%s", err, output.String())
			}
		case <-time.After(5 * time.Second):
			_ = command.Process.Kill()
			<-exited
			t.Errorf("sandboxd did not stop within five seconds\n%s", output.String())
		}
	}
}
