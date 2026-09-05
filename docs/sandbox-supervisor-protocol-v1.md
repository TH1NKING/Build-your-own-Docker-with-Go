# Sandbox Supervisor protocol v1

The Sandbox Supervisor protocol is the closed local boundary between an
unprivileged Worker Node and the privileged `sandboxd` process. It is available
only on Linux and does not expose shell commands, arbitrary host paths, mount
operations, process identifiers, or caller-selected isolation settings.

T03 establishes the transport and validation boundary. T04 adds actual
Sandbox creation when the Platform Operator enables subordinate-ID allocation.
The default protocol-only mode still returns `operation_unavailable` for a
valid `create_sandbox` request whose Profile exists.

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

Creation additionally requires trusted startup configuration:

```text
--subuid-start <first-reserved-host-uid>
--subgid-start <first-reserved-host-gid>
--subid-count <reserved-ids-in-each-range>
```

All three values default to zero. All-zero configuration disables creation
and permits the existing unprivileged protocol tests. Otherwise, `sandboxd`
must run as root, both starts must be nonzero, and the count must be a positive
multiple of 65,536. The complete ranges must fit in usable Linux UID/GID
space; neither host ID zero nor the reserved ID `4294967295` is mapped.

Each active Sandbox reserves a distinct block of 65,536 IDs from each range.
Internal IDs `0..65535` map to that block; concurrent Sandboxes managed by the
same Supervisor never share a block. A count of 131,072 therefore permits two
active Sandboxes. Supplementary host groups are cleared during child startup.

The Platform Operator must reserve these ranges against system accounts,
other Supervisor instances, and other subordinate-ID users before startup.
`sandboxd` does not read or update `/etc/subuid` or `/etc/subgid`, or detect
external allocations. The flags declare an already-reserved allocation;
they do not make arbitrary host IDs safe to use. The request protocol cannot
override any of these settings.

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
- `creation_failed`
- `sandbox_exists`
- `identity_range_exhausted`

`creation_failed` means the Supervisor could not complete or confirm the
private bootstrap. `sandbox_exists` rejects an active or pre-existing Sandbox
identifier without replacing it. `identity_range_exhausted` means all
configured subordinate-ID blocks are reserved by active creations or Sandboxes.

Errors do not echo raw malformed input or configured host paths.

A successful creation returns only the opaque Sandbox identifier:

```json
{
  "schema": "sandbox-supervisor-response/v1",
  "request_id": "req-001",
  "result": {
    "sandbox_id": "run-001"
  }
}
```

The response exposes no host PID, path, UID/GID allocation, or private
bootstrap channel. A successful response means that the trusted bootstrap
reported completion of root setup; it does not promise indefinite liveness
or that a Workload can execute.

## Creation and lifetime boundary

The Supervisor opens the selected root-owned, immutable Profile through its
trusted Profile store. It opens each `sha256/<digest>/rootfs` component without
following symlinks and checks root ownership and the installer's exact modes:
`0755` for `sha256`, `0555` for the Profile directory and root filesystem.
It re-executes its own binary in new user, mount, PID,
and network namespaces. This private bootstrap accepts inherited handles,
not caller paths or commands. It runs as namespace PID 1 and internal UID 0,
which maps to the allocated non-root host ID.

The bootstrap makes mount propagation recursively private, mounts temporary
staging inside its own namespace, and non-recursively bind-mounts the Profile
root. The new root is remounted read-only with `nosuid` and `nodev`, preserving
applicable existing mount restrictions. `pivot_root(".", ".")` followed by
detaching the old root needs no writable `put_old` directory in the immutable
Profile. The temporary staging disappears with the detached old root.

The Supervisor starts the child from an already-open Profile directory on a
locked OS thread with an unshared `CLONE_FS` context. The inherited current
directory is translated into the child's mount namespace. This avoids
resolving protected host ancestors under subordinate credentials, and avoids
using an inherited directory descriptor as a bind source in the parent mount
namespace. The Supervisor's other threads keep their own current directory.
The child checks its current directory against the supplied Profile handle
and closes that handle before reporting readiness.

The private bootstrap environment sets `GOMAXPROCS=1` and
`GODEBUG=containermaxprocs=0,updatemaxprocs=0` so the Go runtime does not retain
host cgroup file descriptors while discovering or updating CPU parallelism.
This is descriptor hygiene, not a Workload Resource Budget. Readiness leaves
no host directory or regular-file descriptors in the trusted bootstrap;
private pipes and runtime event descriptors remain.

Before serving, the Supervisor marks inherited descriptors above standard I/O
close-on-exec. This includes descriptors supplied by a shell or service manager;
Go's `ExtraFiles` alone is not an inheritance whitelist. The child receives
only explicitly remapped private handles, and diagnostics always use a pipe
even when the Supervisor's stderr is a host log file. The Supervisor's own
runtime descriptors are marked, not forcibly closed.

T04 leaves this trusted PID 1 idle on a lifetime-only pipe. There is no
Execution operation, shell, or caller-controlled executable. The T05 Sandbox
Init contract, including sequential Executions and child reaping, is not yet
implemented. Graceful Supervisor shutdown terminates and waits for its idle
Sandboxes; creation failures release resources acquired by that attempt, and
loss of the lifetime pipe causes the idle bootstrap to exit. This is not the
full T06 lifecycle, restart recovery, or cleanup guarantee for Workloads.
Workspace mounts, capability reduction, System Call Policy enforcement, and
Resource Budgets remain subsequent work.

## Verification

Run the protocol acceptance suite as an ordinary Linux user, not through
`sudo`:

```sh
go test ./tests -run '^TestSandboxSupervisor' -count=1
```

The suite launches the real `sandboxd` command and communicates over a real
Unix socket. It fails if the client process is root and does not grant the
client mount, namespace, or cgroup privileges.

Run the separate privileged creation suite from an ordinary account inside
a disposable Linux VM, with Go, `sudo`, and util-linux installed:

```sh
bash tests/run-sandbox-creation-linux.sh
```

The script builds the Supervisor, Profile installer, and trusted conformance
probe, then runs tests tagged `sandbox_root,profilebundle_root` in an outer
mount/PID/network namespace with a private proc mount. It supplies absolute
paths through `SANDBOXD_CLI`, `PROFILE_BUNDLE_CLI`, and `SANDBOX_ROOT_PROBE`.
Test allocations are fixture values for the disposable environment, not
deployment recommendations.

The harness observes real `/proc` mappings, mount topology and descriptors,
and uses host-side `nsenter` to execute the trusted probe inside the Sandbox.
The fixture deliberately places that probe at the Profile's Python entrypoint;
it is not CPython and is not a product Execution API. These tests establish
the checked kernel properties, not a complete security claim for arbitrary
Workloads. See the [T04 learning guide](learning/t04-sandbox-creation.md).
