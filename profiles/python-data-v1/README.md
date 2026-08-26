# python-data-v1

This directory contains the reviewed, version-controlled inputs for the first
Runtime Profile.

- `profile.lock.json` pins every external root-filesystem input by version,
  immutable source URL, size, and SHA-256.
- `system-call-policy.json` is the digest-bound, default-deny candidate System
  Call Policy. T08 must add and verify the reviewed rules before production
  Workload execution.

The profile targets `linux/amd64`, provides CPython 3.14.7 and standard-library
data modules, and intentionally contains no third-party Python packages. The
builder removes package-installation tooling and consumes only a preverified
source cache; it never downloads dependencies itself.

See [Profile Bundle v1](../../docs/profile-bundle-v1.md) for the public format,
build, installation, and trust contracts.
