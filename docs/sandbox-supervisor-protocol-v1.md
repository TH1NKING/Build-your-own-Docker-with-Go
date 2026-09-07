# Sandbox Supervisor protocol v1

The Sandbox Supervisor protocol is the closed local boundary between an
unprivileged Worker Node and the privileged `sandboxd` process. It is available
only on Linux and does not expose shell commands, arbitrary host paths, mount
operations, process identifiers, or caller-selected isolation settings.

T03 establishes the transport and validation boundary. T04 adds actual
Sandbox creation when the Platform Operator enables subordinate-ID allocation.
T05 adds a verified Sandbox Init and sequential `execute_python` requests.
T06 adds explicit `destroy_sandbox`, request abandonment cleanup, and terminal
identifier protection within one Supervisor lifetime.
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

Creation also needs a root-owned writable cgroup v2 directory, selected by
`--cgroup-root` (default `/sys/fs/cgroup`). The directory must be real and must
not allow group or other writes. Linux must provide `cgroup.kill` (5.14+),
`clone3` with `CLONE_INTO_CGROUP`, and the `cpu`, `memory`, and `pids` controllers,
including `memory.swap.max`. The configured directory must have those
controllers available for delegation. The Supervisor enables them only within
that configured root, creates a persistent budget parent per Sandbox, and
places Init and each Execution in separate leaves. It does not move controllers
from cgroup v1 or fall back to incomplete enforcement. Unavailable controllers,
failed writes or unconfirmed limits return `operation_unavailable` before
Workload code starts. The cgroup directory is deployment configuration, never
a request parameter.

Trusted Resource Budget flags (defaults shown) are:

```text
--cpu-millis 2000
--memory-bytes 1073741824
--swap-bytes 0
--pids-limit 64
```

CPU uses a fixed 100,000-microsecond period; 2000 millicores produces
`cpu.max = 200000 100000`. It limits aggregate CPU bandwidth, not CPU affinity
or Execution duration. CPU values must be at least 10 millicores and fit the
quota conversion. Memory must be positive, swap nonnegative, and both must be
whole memory pages; PID count must be positive. Invalid budgets fail startup.
Budgets include Init and Workload tasks (including threads), and surviving
Workspace tmpfs charges remain under the same parent across Executions.
Very small valid policies may be insufficient to start or keep Init alive;
there is no promised minimum usable Python memory or task count.

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

The closed operation set is `create_sandbox`, `execute_python`, and
`destroy_sandbox`. Creation:

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

Destruction accepts only the opaque Sandbox identifier:

```json
{
  "schema": "sandbox-supervisor-request/v1",
  "request_id": "req-destroy",
  "operation": "destroy_sandbox",
  "parameters": { "sandbox_id": "run-001" }
}
```

The corresponding Go client method is `Client.DestroySandbox`. Its successful
result contains `sandbox_id`. Destruction terminates an active Execution along
with its Sandbox; it does not preserve the Workspace for reuse. Concurrent or
repeated destruction is idempotent. An unknown identifier acknowledges absence
without looking up or deleting a filesystem path, so pre-existing unowned
directories remain untouched. Protocol-only mode returns `operation_unavailable`.

During an in-flight creation or Execution, EOF (including `CloseWrite`), extra
request bytes, or a failed response write abandons the operation. Only after
acquiring its Sandbox may a request cancel that Sandbox. Rejected duplicates,
invalid requests, and `sandbox_busy` clients cannot cancel another operation.
Once a complete response has been written, normal socket closure leaves a live
Sandbox available for another Execution. Clients must keep both directions
open until receiving the complete response. Successful writing confirms local
delivery to the socket; it cannot prove the remote application consumed the
response. Callers must explicitly destroy a Sandbox after their Agent Run ends.

## Response

Every completed request on a connected client receives a v1 response containing exactly one
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
- `cleanup_failed`

`creation_failed` means the Supervisor could not complete or confirm the
private bootstrap. `sandbox_exists` rejects an active, retired, or pre-existing Sandbox
identifier without replacing it. `identity_range_exhausted` means all
configured subordinate-ID blocks are reserved by active creations or Sandboxes.

Errors do not echo raw malformed input or configured host paths.

`cleanup_failed` means an owned runtime directory could not be safely removed.
The Supervisor retains the failed cleanup and its identity reservation. It
never recursively deletes an unexpected tree or deletes a substituted directory.
After the Platform Operator repairs the filesystem condition, repeating
`destroy_sandbox` retries the cleanup. A successful destruction reply waits for
Init reaping, completion of any in-flight Execution cleanup, closure of inherited
channels, and removal of the owned temporary directory. Private mounts and
namespaces disappear with their final processes and handles.

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

T09 adds `terminal_reason` and `resource_usage` to this bounded result:

```json
{
  "execution_id": "step-1",
  "exit_code": 137,
  "stdout": "",
  "stderr": "",
  "truncated": false,
  "terminal_reason": "memory_limit",
  "resource_usage": {
    "oom_events": 1,
    "pid_limit_events": 0,
    "cpu_usec": 35000,
    "cpu_throttled_periods": 0,
    "cpu_throttled_usec": 0
  }
}
```

This is an illustrative result, not a benchmark. `exited` means the process
exited without an observed OOM or PID-budget event, including ordinary nonzero
exits. `memory_limit` and `pids_limit` come from Sandbox cgroup event increments,
never from exit code 137 alone. If both events occur, `memory_limit` takes
precedence and both increments are retained. CPU throttling does not itself
fail an Execution. All counters are deltas from before launcher startup to
after descendant termination; CPU includes trusted Init activity in that
interval. They are not per-process measurements or memory peaks.

The kernel enforces limits during execution. The Supervisor checks OOM and
PID events every 10 ms and kills the Execution group on exhaustion, including
when Python catches a failed allocation or fork. It then performs the normal
reap/readiness handshake. Kernel task-creation refusal does not depend on the
polling interval. End-of-execution counters are reread to capture events that
race with normal process exit. Failed event reads or cleanup are errors, not
ordinary successful results.

Init is born directly in its leaf cgroup and protected with host-configured
`oom_score_adj=-1000`. Before opening the existing launch gate, the Supervisor
sets the blocked Workload launcher's adjustment back to zero and confirms it.
`memory.oom.group=1` is set only on the Execution leaf. These measures do not
guarantee Init can survive every resource failure. If kernel evidence proves
exhaustion but Init cannot finish its handshake, the result retains that
resource reason with `exit_code=-1`, empty streams and `truncated=true` to
indicate unavailable output/status; that Sandbox is invalidated and torn down.
Usable Init state and complete cleanup are required for sequential reuse.

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

The installer materializes the separately verified Init at `/sandbox-init`,
the matching Policy at `/system-call-policy.json`, and
reserves empty `/proc`, `/workspace`, and `/tmp` directories. Before creation,
the Supervisor checks both copies' owner, mode, size, SHA-256 and
non-symlink status against the installed manifest, and validates and compiles
the Policy. The bootstrap mounts local
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

T08 pins Init's launch goroutine to one OS thread and clears that thread's
capability bounding, inheritable, and ambient sets before it forks Workloads.
Init retains effective/permitted authority for its trusted lifecycle operations.
The launcher runs as UID/GID 1000, locks its own OS thread, loads the fixed
read-only Policy, explicitly clears all active capabilities, sets
`no_new_privs`, and installs the filter before `exec` of Python on that thread.
Other launcher threads execute trusted Go code only and disappear at exec.
Python and all its later threads and descendants inherit these restrictions.
Any setup failure aborts the launch; there is no unrestricted fallback.

The initial Policy supports only Linux amd64, allows reviewed Python syscalls,
checks complete 64-bit arguments, and denies other architectures, x32 calls,
namespace-creating or unreviewed `clone` flags, and `clone3`. A denied call
returns `EPERM`. Python can catch this error and continue, so a denial does not
invent a new Execution terminal reason: callers observe normal Python output,
errors, and exit status. The protocol accepts no Policy or capability override.

One Init waiter owns `wait4` for both main and adopted children. A broken
control channel or inconsistent handshake invalidates the Sandbox. Init death
causes Linux to terminate the remaining PID namespace processes. Graceful
Supervisor shutdown closes pending clients, terminates Sandboxes, and waits for
their cleanup. Creation failures unwind only acquired resources; a failed
creation that never reached readiness may be retried after clean rollback.
Once a Sandbox reached readiness, its identifier is retired on termination.
Later creation using that identifier returns `sandbox_exists`; execution returns
`sandbox_not_found`. A failed cleanup keeps the identifier unavailable as well.

Retired identifiers are held in memory for this Supervisor lifetime. Clients
must choose globally fresh Sandbox IDs, including across Supervisor restarts.
T06 does not persist or reconstruct Sandbox state, reconcile crash leftovers,
or promise cross-restart idempotency. T09 adds Sandbox cgroup Resource Budgets
but no crash-recovery contract. Cgroup cleanup failures retain owned handles
and the Sandbox identity reservation for a later `destroy_sandbox` retry;
successful destruction waits for removal of the Execution, Init, and budget
groups. Complete mount, storage, output and
deadline enforcement remain T07 and T10–T12. This is not the complete
production security boundary.

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
concurrent request rejection, output capture and failure behavior. T06 also checks
idempotent and concurrent destruction, in-flight client disconnection, Init loss,
shutdown with incomplete clients, failed cleanup retries, and real kernel failures
at successive creation stages. Tests run
inside disposable namespaces and allocate only a temporary test cgroup subtree.
See the [T05 learning guide](learning/t05-sandbox-init.md) for the design tradeoffs.
The [T06 learning guide](learning/t06-sandbox-lifecycle.md) explains lifecycle
ownership, cancellation, cleanup ordering, and a complete client example.
