# python-data-v1

This directory contains the reviewed, version-controlled inputs for the first
Runtime Profile.

- `profile.lock.json` pins every external root-filesystem input by version,
  immutable source URL, size, and SHA-256.
- `system-call-policy.json` is the digest-bound, default-deny System Call Policy.
  T08 applies it before each Python Workload: ordinary data work and reviewed
  process/thread creation are allowed; unreviewed calls, namespace creation,
  alternative syscall ABIs, and unreviewed `clone` flags return `EPERM`.

The reviewed `clone` flag set is `0x017d4fff`: the legacy exit-signal byte,
`CLONE_VM`, `CLONE_FS`, `CLONE_FILES`, `CLONE_SIGHAND`, `CLONE_VFORK`,
`CLONE_THREAD`, `CLONE_SYSVSEM`, `CLONE_SETTLS`, `CLONE_PARENT_SETTID`,
`CLONE_CHILD_CLEARTID`, `CLONE_CHILD_SETTID`, and `CLONE_DETACHED`. The last
flag is obsolete and ignored by Linux, but the locked musl pthread launcher
still supplies it. The policy masks all other 64-bit flags, including namespace
and unknown high bits. `clone3` remains denied; no errno fallback is needed by
the locked runtime. The legacy `clone` exit-signal byte does not expose the
`CLONE_NEWTIME` functionality available through denied `clone3`/`unshare`.

The profile targets `linux/amd64`, provides CPython 3.14.7 and standard-library
data modules, and intentionally contains no third-party Python packages. The
builder removes package-installation tooling and consumes only a preverified
source cache; it never downloads dependencies itself.

Build the production Sandbox Init from `cmd/sandbox-init` with `CGO_ENABLED=0`
for `linux/amd64` and pass that binary to `profile-bundle build --sandbox-init`.
Its SHA-256 is recorded separately from the locked root filesystem. Installation
places the verified executable at `/sandbox-init`, the verified Policy at
`/system-call-policy.json`, and reserves empty `/proc`,
`/workspace`, and `/tmp` mountpoints before making the root immutable.

See [Profile Bundle v1](../../docs/profile-bundle-v1.md) for the public format,
build, installation, and trust contracts.
