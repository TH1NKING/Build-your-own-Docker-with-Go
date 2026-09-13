//go:build linux && sandbox_root && profilebundle_root

package workspace_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestSandboxExecutionAttachmentsProcessReadOnlyInputAndExtractOutput(t *testing.T) {
	fixture, identity := installedSandboxExecutionFixture(t)
	staging := sandboxAttachmentStaging(t, fixture)
	writeSandboxAttachment(t, staging, "csv-001", "item,amount\napple,12\npear,8\napple,5\n")
	t.Cleanup(startSandboxSupervisor(t, fixture, attachmentSupervisorArguments(staging)...))
	assertSandboxCreated(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath,
		attachmentCreateRequest(t, "run-input", identity, []map[string]string{{"staging_id": "csv-001", "name": "sales.csv"}})), "run-input")
	result := executeDeclaredSandboxPython(t, fixture, "run-input", "summarize", `import csv, json
with open('/workspace/input/sales.csv') as source:
    rows = list(csv.DictReader(source))
summary = {'rows': len(rows), 'total': sum(int(row['amount']) for row in rows)}
with open('summary.json', 'w') as output: json.dump(summary, output, sort_keys=True)
print('rows=3 total=25')`, []string{"summary.json"})
	if result.ExitCode != 0 || result.Stdout != "rows=3 total=25\n" || result.OutputError != "" || len(result.Outputs) != 1 || string(result.Outputs[0].Content) != `{"rows": 3, "total": 25}` {
		t.Fatalf("input-to-output journey failed: %+v", result)
	}
	t.Logf("DEMO normal: %s; extracted summary.json = %s; size=%d SHA256=%s", strings.TrimSpace(result.Stdout), result.Outputs[0].Content, result.Outputs[0].Size, result.Outputs[0].SHA256)
	assertSandboxDestroyed(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath, destroySandboxWireRequest(t, "run-input")), "run-input")
	content, err := os.ReadFile(filepath.Join(staging, "csv-001"))
	if err != nil || string(content) != "item,amount\napple,12\npear,8\napple,5\n" {
		t.Fatalf("destroy changed trusted source: %q, %v", content, err)
	}
}

func TestSandboxExecutionAttachmentsDenyMutationsAndPermitReuse(t *testing.T) {
	fixture, identity := installedSandboxExecutionFixture(t)
	staging := sandboxAttachmentStaging(t, fixture)
	writeSandboxAttachment(t, staging, "csv-001", "item,amount\napple,12\npear,8\napple,5\n")
	t.Cleanup(startSandboxSupervisor(t, fixture, attachmentSupervisorArguments(staging)...))
	assertSandboxCreated(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath,
		attachmentCreateRequest(t, "run-input", identity, []map[string]string{{"staging_id": "csv-001", "name": "sales.csv"}})), "run-input")
	probe := executeSandboxPython(t, fixture, "run-input", "mutate", `import errno, os
for action in [lambda: open('/workspace/input/sales.csv', 'w'),
               lambda: os.unlink('/workspace/input/sales.csv'),
               lambda: os.rename('/workspace/input/sales.csv', '/workspace/output/stolen'),
               lambda: open('/workspace/input/new', 'w'),
               lambda: os.chmod('/workspace/input/sales.csv', 0o666),
               lambda: os.chmod('/workspace/input', 0o777)]:
    try: action()
    except OSError as error: assert error.errno in [errno.EROFS, errno.EACCES, errno.EPERM, errno.EXDEV], error
    else: raise AssertionError('input mutation succeeded')
mounts = [line.split() for line in open('/proc/self/mountinfo')]
for path in ['/workspace/input', '/workspace/input/sales.csv']:
    selected = [m for m in mounts if m[4] == path]
    assert len(selected) == 1 and 'ro' in selected[0][5].split(','), (path, selected)
    assert all(flag in selected[0][5].split(',') for flag in ['nosuid', 'nodev', 'noexec'])
assert open('/workspace/input/sales.csv').read().endswith('apple,5\n')
print('six mutations denied; read-only mounts verified')`)
	if probe.ExitCode != 0 || probe.Stdout != "six mutations denied; read-only mounts verified\n" {
		t.Fatalf("read-only Attachment boundary failed: %+v", probe)
	}
	t.Logf("DEMO read-only: %s", strings.TrimSpace(probe.Stdout))
	reuse := executeSandboxPython(t, fixture, "run-input", "reuse", "assert open('/workspace/input/sales.csv').read().endswith('apple,5\\n'); open('after-denial', 'w').write('ok'); print('reused')")
	if reuse.ExitCode != 0 || reuse.Stdout != "reused\n" {
		t.Fatalf("denial prevented reuse: %+v", reuse)
	}
	t.Log("DEMO read-only: same Sandbox executed again; trusted input unchanged")
	assertSandboxDestroyed(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath, destroySandboxWireRequest(t, "run-input")), "run-input")
	content, err := os.ReadFile(filepath.Join(staging, "csv-001"))
	if err != nil || string(content) != "item,amount\napple,12\npear,8\napple,5\n" {
		t.Fatalf("destroy changed trusted source: %q, %v", content, err)
	}
}

func TestSandboxExecutionAttachmentsRejectUntrustedSourcesBeforeCreation(t *testing.T) {
	fixture, identity := installedSandboxExecutionFixture(t)
	staging := sandboxAttachmentStaging(t, fixture)
	writeSandboxAttachment(t, staging, "valid", "trusted")
	writeSandboxAttachment(t, staging, "writable", "unsealed")
	if err := os.Chmod(filepath.Join(staging, "writable"), 0o666); err != nil {
		t.Fatal(err)
	}
	writeSandboxAttachment(t, staging, "hard", "linked")
	if err := os.Link(filepath.Join(staging, "hard"), filepath.Join(staging, "hard-alias")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(staging, "symlink")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(staging, "directory"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(staging, "fifo"), 0o444); err != nil {
		t.Fatal(err)
	}
	writeSandboxAttachment(t, staging, "wrong-owner", "foreign")
	if err := os.Chown(filepath.Join(staging, "wrong-owner"), 200000, 300000); err != nil {
		t.Fatal(err)
	}
	file, err := os.Create(filepath.Join(staging, "oversized"))
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate((20 << 20) + 1); err != nil {
		t.Fatal(err)
	}
	file.Close()
	if err := os.Chmod(filepath.Join(staging, "oversized"), 0o444); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(startSandboxSupervisor(t, fixture, attachmentSupervisorArguments(staging)...))
	for index, source := range []string{"missing", "symlink", "directory", "fifo", "hard", "writable", "wrong-owner", "oversized", "../valid", "/etc/passwd"} {
		t.Run(source, func(t *testing.T) {
			id := fmt.Sprintf("rejected-%d", index)
			response := exchangeSandboxSupervisorMessage(t, fixture.socketPath, attachmentCreateRequest(t, id, identity, []map[string]string{{"staging_id": source, "name": "input.txt"}}))
			assertSandboxSupervisorErrorCode(t, response, "invalid_reference")
			assertSandboxCreated(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath, attachmentCreateRequest(t, id, identity, []map[string]string{{"staging_id": "valid", "name": "input.txt"}})), id)
			assertSandboxDestroyed(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath, destroySandboxWireRequest(t, id)), id)
			t.Logf("DEMO malicious: %q -> invalid_reference; corrected request reused the unconsumed Sandbox ID", source)
		})
	}
}

func TestSandboxExecutionAttachmentsRejectMalformedDeclarations(t *testing.T) {
	fixture, identity := installedSandboxExecutionFixture(t)
	staging := sandboxAttachmentStaging(t, fixture)
	writeSandboxAttachment(t, staging, "valid", "trusted")
	t.Cleanup(startSandboxSupervisor(t, fixture, attachmentSupervisorArguments(staging)...))
	for _, name := range []string{"../escape", "/absolute", ".", "..", "sub/file", "back\\slash", "", strings.Repeat("x", 129)} {
		assertSandboxSupervisorErrorCode(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath, attachmentCreateRequest(t, "invalid-name", identity, []map[string]string{{"staging_id": "valid", "name": name}})), "invalid_reference")
	}
	for _, raw := range []string{
		`[{"staging_id":"valid","name":"a","host_path":"/etc/passwd"}]`,
		`[{"staging_id":"valid","staging_id":"other","name":"a"}]`,
		`[{"staging_id":"valid","name":"a","name":"b"}]`,
		`[null]`, `"valid"`,
	} {
		payload := []byte(fmt.Sprintf(`{"schema":"sandbox-supervisor-request/v1","request_id":"invalid-json","operation":"create_sandbox","parameters":{"sandbox_id":"invalid-json","profile_identity":%q,"attachments":%s}}`, identity, raw))
		assertSandboxSupervisorErrorCode(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath, payload), "malformed_request")
	}
	for _, entries := range [][]map[string]string{
		{{"staging_id": "valid", "name": "same"}, {"staging_id": "valid", "name": "same"}},
		{{"staging_id": "valid", "name": "first"}, {"staging_id": "valid", "name": "second"}},
	} {
		assertSandboxSupervisorErrorCode(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath, attachmentCreateRequest(t, "duplicate", identity, entries)), "invalid_reference")
	}
}

func sandboxAttachmentStaging(t *testing.T, fixture sandboxSupervisorFixture) string {
	t.Helper()
	parent := filepath.Dir(fixture.sandboxRoot)
	if err := os.Chmod(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	staging := filepath.Join(parent, "attachments")
	if err := os.Mkdir(staging, 0o755); err != nil {
		t.Fatal(err)
	}
	return staging
}

func writeSandboxAttachment(t *testing.T, staging, id, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(staging, id), []byte(content), 0o444); err != nil {
		t.Fatal(err)
	}
}

func attachmentSupervisorArguments(staging string) []string {
	return []string{"--attachment-root", staging, "--subuid-start", "200000", "--subgid-start", "300000", "--subid-count", "65536", "--cgroup-root", os.Getenv("SANDBOX_CGROUP_ROOT")}
}

func attachmentCreateRequest(t *testing.T, id, identity string, attachments []map[string]string) []byte {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"schema": "sandbox-supervisor-request/v1", "request_id": "create", "operation": "create_sandbox", "parameters": map[string]any{"sandbox_id": id, "profile_identity": identity, "attachments": attachments}})
	if err != nil {
		t.Fatal(err)
	}
	return payload
}
