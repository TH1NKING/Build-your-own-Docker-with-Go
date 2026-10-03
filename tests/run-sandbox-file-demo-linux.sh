#!/usr/bin/env bash
set -euo pipefail

# The demo uses the same public Supervisor client and real-kernel fixtures as
# acceptance. Each invocation creates and cleans its own Sandbox and staging.
repository_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
source "$repository_root/tests/sandbox-test-result.sh"
normal=TestSandboxExecutionAttachmentsProcessReadOnlyInputAndExtractOutput
read_only=TestSandboxExecutionAttachmentsDenyMutationsAndPermitReuse
malicious=(TestSandboxExecutionAttachmentsRejectUntrustedSourcesBeforeCreation TestSandboxExecutionAttachmentsRejectMalformedDeclarations)
case "${1:-all}" in
  normal) contracts=("$normal") ;;
  read-only) contracts=("$read_only") ;;
  malicious) contracts=("${malicious[@]}") ;;
  all) contracts=("$normal" "$read_only" "${malicious[@]}") ;;
  *) echo "Usage: $0 [normal|read-only|malicious|all]" >&2; exit 2 ;;
esac
if [[ $# -gt 1 ]]; then echo 'Expected at most one demo selection' >&2; exit 2; fi
echo "Sandbox file demo: ${1:-all}"
echo 'Watch for DEMO lines, followed by PASS. Any failed assertion exits nonzero.'
sandbox_select_contracts "${contracts[@]}"
exec bash "$repository_root/tests/run-sandbox-init-linux.sh"
