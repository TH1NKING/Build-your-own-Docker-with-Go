# T04 acceptance record — 2026-09-05

Scope: [issue #10](https://github.com/TH1NKING/Build-your-own-Docker-with-Go/issues/10).
Implementation baseline: `63a38451ba43d81e203b02daadbd11db5c40bc5b`.

## Executed verification

- Native Linux Go `1.26.1 linux/amd64` on
  `5.15.153.1-microsoft-standard-WSL2`.
- Repository source was copied into a disposable mount/PID/network namespace.
  Formatting checks, `go vet ./...`, `go test ./...`, `go build ./...`, and
  Shell syntax checks passed. Ordinary tests ran as UID/GID 1000 with cleared
  supplementary groups, including the real Unix socket protocol suite.
- `bash tests/run-sandbox-creation-linux.sh` passed all nine top-level creation
  tests as root in a separate namespace with a private temporary filesystem.
  These cover exact UID/GID mappings, cleared host groups, root replacement,
  read-only/private mounts, absent host filesystem handles, an in-Sandbox
  marker/write probe, concurrent disjoint IDs, invalid configuration, invalid
  Profile ownership/symlinks, preservation of existing directories, kernel
  mapping rejection and launcher-inherited directory handles.
- Windows `go test ./...` and `go vet ./...` passed for the platform-applicable
  packages. Windows results are not evidence of kernel isolation.

The checked-in GitHub Actions job runs the same creation script on Ubuntu.
This record reports local execution; a hosted CI run has not been dispatched.
The production CPython content/reproducibility suite was not rerun for T04;
the creation tests install real test Bundles with a clearly identified trusted
conformance executable, not CPython.

## Observed red-to-green cases

1. Creation initially failed because the Supervisor had no subordinate-ID
   configuration or implementation. The mapped, live Sandbox test now passes.
2. The filesystem-boundary test found Go runtime cgroup descriptors surviving
   root replacement. Bootstrap runtime settings now prevent their retention.
3. A Profile directory transferred to an untrusted owner was initially
   accepted. Handle-based ownership/mode checks now reject it.
4. Review identified a shell-inherited host directory FD. A real launcher
   reproduced the leak at FD 9; startup close-on-exec marking fixes the test.

An initial bind-mount attempt using inherited parent directory FDs failed with
`EINVAL`. The final implementation inherits an independently scoped cwd,
which the kernel translates into the new mount namespace. The learning guide
explains this distinction and the associated tradeoffs.

## Standards

Initial review found one P1 violation of ADR-0017: `ExtraFiles` did not exclude
non-CLOEXEC descriptors inherited from the Supervisor's launcher. The change
now seals inherited descriptors before serving, preserves runtime handles,
and explicitly passes only required handles across exec. The regression test
and follow-up review confirmed the fix. No unresolved standards violations or
actionable smell-baseline findings remain.

## Spec

No unresolved findings. The implementation and kernel tests cover all three
T04 criteria: configured nonzero host identity mapping, a read-only Profile
root installed through `pivot_root` with old-root detachment, and an actual
in-namespace probe that cannot reach a host-only marker.

Final review counts: Standards 0 unresolved (one P1 fixed); Spec 0 unresolved.

## Remaining boundary

This is creation acceptance, not full Sandbox security acceptance. Product
Execution, a production Sandbox Init, complete failure recovery, capabilities,
System Call Policy, controlled mounts/Workspace and Resource Budgets remain
the responsibility of subsequent tickets. The trusted bootstrap is idle.
See [the T04 learning guide](../learning/t04-sandbox-creation.md) for the
implementation explanation and reproducible commands.
