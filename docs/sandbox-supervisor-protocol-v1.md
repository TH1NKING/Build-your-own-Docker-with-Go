# Sandbox Supervisor protocol v1

The Sandbox Supervisor protocol is the closed local boundary between an
unprivileged Worker Node and the privileged `sandboxd` process. It is available
only on Linux and does not expose shell commands, arbitrary host paths, mount
operations, process identifiers, or caller-selected isolation settings.

This version establishes the transport and validation boundary required by
T03. T04 implements actual Sandbox creation. Until then, a valid
`create_sandbox` request whose Profile exists returns `operation_unavailable`
instead of claiming that an isolated Sandbox was created.

## Trusted startup configuration

`sandboxd` requires three absolute paths:

```text
--socket <socket-path>
--profile-store <profile-store-root>
--sandbox-root <sandbox-runtime-root>
```

The socket parent, Profile store, and Sandbox root must be real directories
owned by the `sandboxd` effective UID and must not permit group or other writes.
The socket parent has the stricter mode and group contract `0750
<sandboxd-uid>:<sandboxd-effective-gid>`. In production, run `sandboxd` as
`root:<worker-group>` and provision that directory as `0750
root:<worker-group>` before startup. The published socket is `0660` and inherits
that process group, allowing only root and the dedicated Worker group to open
it.

`sandboxd` refuses to overwrite a pre-existing file, directory, symlink, or
active socket at the configured socket path.

## Framing

Each connection carries exactly one request and one response, then closes.
Each message consists of:

1. A four-byte unsigned big-endian payload length.
2. One UTF-8 JSON payload of that exact length.

Control messages are limited to 65,536 bytes. A declared larger request is
rejected before its payload is read or allocated. Connections have a bounded
deadline, and the Supervisor serves up to 16 control connections concurrently
so an incomplete client cannot block unrelated requests. Bulk Workload input,
output, or file contents do not belong in this protocol.

JSON decoding is strict. Unknown fields, duplicate fields, trailing values,
invalid UTF-8, missing required fields, and malformed JSON are rejected.

## Request

The only operation recognized by this protocol version is `create_sandbox`:

```json
{
  "schema": "sandbox-supervisor-request/v1",
  "request_id": "req-001",
  "operation": "create_sandbox",
  "parameters": {
    "sandbox_id": "run-001",
    "profile_identity": "sha256:0000000000000000000000000000000000000000000000000000000000000000"
  }
}
```

`request_id` and `sandbox_id` are opaque identifiers of one to 64 ASCII
characters. They begin with a lowercase letter or digit; later characters may
also contain `-` or `_`. `profile_identity` is exactly `sha256:` followed by 64
lowercase hexadecimal characters.

The client never supplies a Profile path, Sandbox path, Workspace path, host
PID, UID, GID, command, environment, mount, Network Policy, or Resource Budget.
`sandboxd` derives filesystem locations beneath its already-open configured
roots. Profile symlinks and references that cannot be resolved beneath the
Profile store are rejected.

## Response

Every decodable request receives a v1 response envelope containing exactly one
of `result` or `error`:

```json
{
  "schema": "sandbox-supervisor-response/v1",
  "request_id": "req-001",
  "error": {
    "code": "profile_not_found"
  }
}
```

Stable v1 error codes are:

- `unsupported_version`
- `unknown_operation`
- `malformed_request`
- `invalid_reference`
- `request_too_large`
- `profile_not_found`
- `operation_unavailable`

Errors do not echo raw malformed input or configured host paths.

## Verification

Run the protocol acceptance suite as an ordinary Linux user, not through
`sudo`:

```sh
go test ./tests -run '^TestSandboxSupervisor' -count=1
```

The suite launches the real `sandboxd` command and communicates over a real
Unix socket. It fails if the client process is root and does not grant the
client mount, namespace, or cgroup privileges.
