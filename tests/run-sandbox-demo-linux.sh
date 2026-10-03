#!/usr/bin/env bash
set -euo pipefail

# A bounded interview walkthrough backed by real Linux assertions. The normal
# command rebuilds this checkout; SANDBOX_TEST_BIN_DIR is for prepared binaries.
repository_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
source "$repository_root/tests/sandbox-test-result.sh"
files=(
  TestSandboxExecutionAttachmentsProcessReadOnlyInputAndExtractOutput
  TestSandboxExecutionAttachmentsDenyMutationsAndPermitReuse
  TestSandboxExecutionAttachmentsRejectUntrustedSourcesBeforeCreation
  TestSandboxExecutionAttachmentsRejectMalformedDeclarations
)
lifecycle=(
  TestSandboxExecutionKeepsInitAndWorkspaceAcrossSequentialPython
  TestSandboxExecutionReapsDetachedDescendantsBeforeReuse
  TestSandboxExecutionDeadlinePreservesInitAndWorkspace
)
security=(
  TestSandboxExecutionSecurityDropsPrivilegesAcrossExec
  TestSandboxExecutionSecurityEnforcesSystemCallPolicy
  TestSandboxExecutionSecurityDeniesNamespaceFlagsAndAlternateABIs
  TestSandboxExecutionHardeningNetworkNoneHasNoIngressOrEgress
)
resources=(
  TestSandboxExecutionResourceMemoryExhaustion
  TestSandboxExecutionResourceCPUThrottlesWithoutFailing
  TestSandboxExecutionResourcePIDExhaustionKillsCaughtFailureAndDescendants
  TestSandboxExecutionStorageConfiguredBytesRefuseWritesAndPermitReuse
  TestSandboxExecutionReportsFailureAndBoundsOutputWithoutLosingInit
)
if (( $# > 1 )); then echo 'Expected at most one demo selection' >&2; exit 2; fi
case "${1:-all}" in
  files) contracts=("${files[@]}") ;;
  lifecycle) contracts=("${lifecycle[@]}") ;;
  security) contracts=("${security[@]}") ;;
  resources) contracts=("${resources[@]}") ;;
  all) contracts=("${files[@]}" "${lifecycle[@]}" "${security[@]}" "${resources[@]}") ;;
  *) echo "Usage: $0 [files|lifecycle|security|resources|all]" >&2; exit 2 ;;
esac
printf 'Sandbox interview demo: %s (%s contracts)\n' "${1:-all}" "${#contracts[@]}"
echo 'DEMO lines explain verified results. Completion requires every contract to pass without skips.'
sandbox_select_contracts "${contracts[@]}"
exec bash "$repository_root/tests/run-sandbox-init-linux.sh"
