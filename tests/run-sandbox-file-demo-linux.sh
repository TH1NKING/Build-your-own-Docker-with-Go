#!/usr/bin/env bash
set -euo pipefail

# The demo uses the same public Supervisor client and real-kernel fixtures as
# acceptance. Each invocation creates and cleans its own Sandbox and staging.
repository_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
case "${1:-all}" in
  normal) selector='^TestSandboxExecutionAttachmentsProcessReadOnlyInputAndExtractOutput$' ;;
  read-only) selector='^TestSandboxExecutionAttachmentsDenyMutationsAndPermitReuse$' ;;
  malicious) selector='^TestSandboxExecutionAttachmentsReject(UntrustedSourcesBeforeCreation|MalformedDeclarations)$' ;;
  all) selector='^TestSandboxExecutionAttachments(ProcessReadOnlyInputAndExtractOutput|DenyMutationsAndPermitReuse|RejectUntrustedSourcesBeforeCreation|RejectMalformedDeclarations)$' ;;
  *) echo "Usage: $0 [normal|read-only|malicious|all]" >&2; exit 2 ;;
esac
if [[ $# -gt 1 ]]; then echo 'Expected at most one demo selection' >&2; exit 2; fi
echo "Sandbox file demo: ${1:-all}"
echo 'Watch for DEMO lines, followed by PASS. Any failed assertion exits nonzero.'
export SANDBOX_TEST_RUN="$selector"
exec bash "$repository_root/tests/run-sandbox-init-linux.sh"
