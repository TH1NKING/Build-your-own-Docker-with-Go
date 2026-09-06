# Sandbox Supervisor protocol v1

The Sandbox Supervisor protocol is the closed local boundary between an
unprivileged Worker Node and the privileged `sandboxd` process. It is available
only on Linux and does not expose shell commands, arbitrary host paths, mount
operations, process identifiers, or caller-selected isolation settings.

T03 establishes the transport and validation boundary. T04 adds actual
Sandbox creation when the Platform Operator enables subordinate-ID allocation.
T05 adds a verified Sandbox Init and sequential `execute_python` requests.
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

Execution also needs a root-owned writable cgroup v2 directory, selected by
`--cgroup-root` (default `/sys/fs/cgroup`). The directory must be real and must
not allow group or other writes. Linux must provide `cgroup.kill` (5.14+).
The Supervisor creates an exclusive child cgroup for each Execution and
removes it after its processes exit. The directory is deployment configuration,
never a request parameter. A missing or unsupported cgroup v2 location returns
`operation_unavailable` before any Python code starts.

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
so an incomplete client cannot block unrelated requests. T05 permits bounded
Python source, stdin and output in the control frame; bulk file transfer remains
outside this protocol. Reading a request has a five-second deadline. Executions
have a 60-second private transport guard and a 65-second response write deadline.

JSON decoding is strict. Unknown fields, duplicate fields, trailing values,
invalid UTF-8, missing required fields, and malformed JSON are rejected.

## Request

The closed operation set is `create_sandbox` and `execute_python`. Creation:

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

Execution requests select only an existing Sandbox and a bounded Python
Workload; the fixed interpreter is `/opt/python/bin/python3 -I -B -c <source>`:

```json
{
  "schema": "sandbox-supervisor-request/v1",
  "request_id": "req-step-1",
  "operation": "execute_python",
  "parameters": {
    "sandbox_id": "run-001",
    "execution_id": "step-1",
    "source": "open('answer.txt', 'w').write('42'); print('saved')",
    "stdin": ""
  }
}
```

`execution_id` follows the same identifier grammar. `source` is nonempty,
contains no NUL byte, and is at most 32 KiB of UTF-8; optional `stdin` is at most 8 KiB. The total encoded
frame must still fit 64 KiB. An Execution runs as internal UID/GID 1000 with no
supplementary groups, a fixed environment, and `/workspace/output` as its
current directory. Only standard input/output/error reach Python. Concurrent
requests for the same Sandbox are rejected with `sandbox_busy`; they are not
queued. `execution_id` correlates a result, not an idempotency key: T05 has no
durable dispatch/result store. Never automatically retry an uncertain request.

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
- `sandbox_not_found`
- `sandbox_busy`
- `execution_failed`

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
bootstrap channel. A successful response means that root setup completed and
the verified Init entered its private request loop; it does not promise
indefinite liveness or availability of Execution cgroup resources.

Successful Executions return `execution_id`, `exit_code`, separate `stdout`
and `stderr`, and `truncated`. A nonzero Python exit is still an Execution
Result, not a protocol error. T05 retains at most 4 KiB per stream and drains
the remainder without retaining it; invalid UTF-8 is replaced. These small
preliminary results fit the current control frame. T11 will implement the full
configured output budget and Execution Result contract.

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
applicable existing mount restrictions. A fresh proc filesystem for the child's
PID namespace is mounted at the new root's `/proc` before detaching the old
root, while the inherited proc is still visible for the kernel's user-namespace
mount checks. It never binds the host proc view into the Sandbox.
`pivot_root(".", ".")` followed by
detaching the old root needs no writable `put_old` directory in the immutable
Profile. The temporary staging disappears with the detached old root.

The installer materializes the separately verified Init at `/sandbox-init` and
reserves empty `/proc`, `/workspace`, and `/tmp` directories. Before creation,
the Supervisor checks the installed Init's owner, mode, size, SHA-256 and
non-symlink status against its installed manifest. The bootstrap mounts local
proc, a private Workspace tmpfs and a private temporary tmpfs, then `exec`s
`/sandbox-init`, preserving PID 1. Init itself signals readiness. The Profile
root stays read-only; only `/workspace/output` and `/tmp` are writable by UID
1000. Workspace (512 MiB, 5002 inodes including scaffolding) and temporary
storage (16 MiB, 1024 inodes) have conservative fixed caps in this slice;
configurable policy, exact accounting and Resource Budget outcomes remain T10.

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

The Supervisor and Init use inherited request/response pipes. They are marked
close-on-exec before starting any Workload. A private launcher first waits on
its own one-use pipe; the Supervisor resolves that direct child's namespace
PID to the host PID and attaches it to the Execution cgroup before granting
permission to exec Python. Init waits for the main child and reports its exit.
The Supervisor then uses `cgroup.kill`, waits for `populated 0`, and asks Init
to reap adopted descendants and drain output before returning the result.
Process groups and `setsid` do not let descendants outlive an Execution.

One Init waiter owns `wait4` for both main and adopted children. A broken
control channel or inconsistent handshake invalidates the Sandbox. Init death
causes Linux to terminate the remaining PID namespace processes. Graceful
Supervisor shutdown terminates and waits for its Sandboxes. Full destroy and
restart reconciliation remain T06; complete mount, capability, System Call
Policy and configurable Resource Budget enforcement remain T07–T12. T05 does
not establish the full production security boundary.

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
`SANDBOX_INIT_CLI` names the real statically linked Init used in these fixtures.
Test allocations are fixture values for the disposable environment, not
deployment recommendations.

The harness observes real `/proc` mappings, mount topology and descriptors,
and uses host-side `nsenter` to execute the trusted probe inside the Sandbox.
The fixture deliberately places that probe at the Profile's Python entrypoint;
it is not CPython and is not a product Execution API. These tests establish
the checked kernel properties, not a complete security claim for arbitrary
Workloads. See the [T04 learning guide](learning/t04-sandbox-creation.md).

To exercise real locked CPython through the public Supervisor protocol, first
download the locked inputs described in [Profile Bundle v1](profile-bundle-v1.md),
then run:

```sh
PROFILE_BUNDLE_SOURCE_CACHE=/absolute/locked-source-cache bash tests/run-sandbox-init-linux.sh
```

This builds the actual Init, rebuilds and installs its Profile Bundle, and
checks sequential state reuse, internal identity, descendant cleanup,
concurrent request rejection, output capture and failure behavior. Tests run
inside disposable namespaces and allocate only a temporary test cgroup subtree.
See the [T05 learning guide](learning/t05-sandbox-init.md) for the design tradeoffs.
