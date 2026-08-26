# Profile Bundle v1

Profile Bundle v1 is the published build and installation contract for the
`python-data-v1` Runtime Profile. It is intentionally narrow: the format binds
one locked Linux root filesystem, one externally supplied Sandbox Init, and one
System Call Policy. It does not define Sandbox Init behavior or enforce the
System Call Policy.

## Trust and identity

The complete Bundle is identified by the SHA-256 of its exact bytes. `build`
prints that lowercase 64-character digest. A Worker installation must obtain
the expected digest from trusted configuration and pass it explicitly to
`install`; component digests inside the Bundle prove internal consistency but
are not an independent trust anchor.

The installer publishes a verified Bundle at:

```text
<profile-store>/sha256/<bundle-sha256>/
```

It never accepts an installation path from Bundle contents and never replaces
an existing identity. Reinstalling the same digest succeeds only after the
newly verified staging tree and the existing tree compare exactly.

## Canonical outer format

The Bundle is an uncompressed POSIX tar with these entries in this exact order:

```text
manifest.json
profile.lock.json
system-call-policy.json
sandbox-init
rootfs.tar
```

All entries are regular files owned by numeric UID/GID zero, use an epoch
timestamp, and have canonical modes: metadata and `rootfs.tar` are `0444`, and
Sandbox Init is `0555`. Missing, duplicate, reordered, altered, or additional
entries are invalid.

`manifest.json` uses schema `profile-bundle-manifest/v1`. It fixes the profile
to `python-data-v1`, the target to `linux/amd64`, the Python version and
entrypoint, and the closed component list. Every component record contains its
role, path, format, installed mode, byte size, and `sha256:<lowercase-hex>`
digest. The manifest does not contain the Bundle digest, avoiding a
self-referential identity.

The System Call Policy envelope is versioned as well. Each rule contains
sorted syscall `names`, the `allow` action, and zero or more argument
predicates. A predicate fixes an argument index from 0 through 5, one supported
comparison operator, a value, and a mask used only by `masked-equal`. T02
validates and binds this schema; T08 remains responsible for reviewing the
actual calls and applying the rules to the kernel.

## Canonical root filesystem

`rootfs.tar` is a deterministic PAX tar. Entries are sorted by slash-relative
path and normalized to UID/GID zero, epoch time, and read-only modes. Version 1
allows regular files and relative in-root symbolic links. Hard links, devices,
FIFOs, sockets, set-ID bits, absolute paths, dot segments, and escaping links
are rejected.

The installer verifies the complete `rootfs.tar` component before parsing it.
It then extracts into a new root-only staging directory, creates symbolic links
only after regular files, makes directories read-only from the leaves upward,
syncs the result, and atomically renames the completed tree into the
content-addressed store. A failure can leave no selectable final directory.
If syncing the publication directory fails after rename, the installer first
renames the identity back into staging and removes it before reporting failure;
an additional rollback failure is reported explicitly as an uncertain host
filesystem failure.

Version 1 bounds one source artifact to 256 MiB, one root-filesystem file to
256 MiB, the normalized root filesystem to 1 GiB and 100,000 entries, and paths
to 4,096 bytes with 255-byte segments. These format-safety bounds are distinct
from the smaller runtime Resource Budget enforced by later execution tickets.

Read-only mounting, User Namespaces, `pivot_root`, and old-root detachment are
owned by T04. System Call Policy enforcement, `no_new_privs`, and capability
removal are owned by T08.

## Locked production inputs

[`profiles/python-data-v1/profile.lock.json`](../profiles/python-data-v1/profile.lock.json)
is the reviewed build input. Every external artifact has an exact version,
immutable URL, byte size, and SHA-256:

- CPython 3.14.7 from the 2026-08-25 `python-build-standalone` release;
- the musl 1.2.5 runtime loader from Alpine 3.22.5 minirootfs.

The first profile deliberately declares no third-party Python packages. It
retains standard-library data facilities such as CSV, JSON, and SQLite. `pip`,
`ensurepip`, and the noninteractive profile's unused terminfo database are
excluded by the reviewed recipe. Adding dependencies changes the reviewed
input and Bundle identity; broadening the System Call Policy requires a new
Runtime Profile version after T08 conformance.

The current System Call Policy is a default-deny candidate with no allow rules.
It is format-valid and digest-bound, but the profile must not be promoted for
Workload execution until T08 supplies and verifies the reviewed allowlist on a
real Linux kernel.

## Commands

Prepare the exact files named in the lock in a trusted source cache, verifying
their sizes and SHA-256 values before use. Building itself performs no network
access:

```text
profile-bundle build \
  --lock profiles/python-data-v1/profile.lock.json \
  --source-cache <verified-source-cache> \
  --system-call-policy profiles/python-data-v1/system-call-policy.json \
  --sandbox-init <externally-supplied-init> \
  --output python-data-v1.bundle
```

Installation is Linux-only, must run as root, and requires an existing
root-owned store that is not writable by group or other:

```text
profile-bundle install \
  --bundle python-data-v1.bundle \
  --expected-sha256 <trusted-bundle-sha256> \
  --store /var/lib/agent-sandbox/profiles
```

On success, `install` prints the relative immutable identity
`sha256/<bundle-sha256>`, not a re-resolved absolute path. Callers keep the
trusted Profile store handle/configuration separate from that identity.

Windows can build and inspect deterministic Bundle bytes from locked source
archives. It cannot substantiate root ownership or Linux atomic installation;
those properties are required CI checks on Ubuntu.

CI downloads and verifies the locked inputs before entering a new network
namespace. Both production builds and the chroot content smoke therefore run
without network access and assert reviewed rootfs and Policy component digests.
