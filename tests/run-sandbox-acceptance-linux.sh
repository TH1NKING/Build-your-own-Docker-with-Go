#!/usr/bin/env bash
set -euo pipefail

# T15 joins the real-kernel Sandbox security suites. Run make check separately
# as an ordinary account; this entry point is not a whole-product release gate.
# The existing runners own their disposable namespaces and cgroup cleanup.
repository_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
log_parent="${SANDBOX_ACCEPTANCE_LOG_DIR:-$repository_root/.cache/sandbox-acceptance}"
mkdir -p -- "$log_parent"
log_parent="$(cd -- "$log_parent" && pwd)"
log_directory="$(mktemp -d "$log_parent/run-$(date -u +%Y%m%dT%H%M%SZ)-XXXXXX")"
printf 'Sandbox acceptance logs: %s\n' "$log_directory"
{
  uname -sr
  printf 'Execution suite: ^TestSandbox(Execution|Lifecycle)\n'
  printf 'Creation suite: ^TestSandboxCreation\n'
} > "$log_directory/environment.log"

# A prebuilt directory can contain a passing older suite. Require the T07/T13
# contracts by name as well as the broad selectors, and update this inventory
# together with any intentional test rename or replacement.
required_execution_tests=(
  TestSandboxExecutionHardeningExposesOnlyRequiredKernelViews
  TestSandboxExecutionHardeningDoesNotInheritHostFilesOrSockets
  TestSandboxExecutionHardeningNetworkNoneHasNoIngressOrEgress
  TestSandboxExecutionAttachmentsProcessReadOnlyInputAndExtractOutput
  TestSandboxExecutionAttachmentsDenyMutationsAndPermitReuse
  TestSandboxExecutionAttachmentsRejectUntrustedSourcesBeforeCreation
  TestSandboxExecutionAttachmentsRejectMalformedDeclarations
  TestSandboxExecutionAttachmentsClientAcceptsSixteenWithoutLeakingHandlesOrSpendingOutputSlots
  TestSandboxExecutionAttachmentsDisabledRejectsInputsAndKeepsEmptyCreateAvailable
  TestSandboxExecutionAttachmentsEnforceExactAggregateByteBudget
  TestSandboxExecutionAttachmentsRejectUnsafeStartupRoot
  TestSandboxExecutionAttachmentsInaccessibleAncestorRollsBackAndAllowsCorrectedRetry
)

run_logged() {
  local log_name="$1"
  shift
  # Save both statuses: a failed runner or a failed log write must fail the gate.
  set +e
  "$@" 2>&1 | tee "$log_directory/$log_name"
  local statuses=("${PIPESTATUS[@]}")
  set -e
  if (( statuses[0] != 0 )); then
    printf 'Sandbox acceptance failed (%s, exit %s); logs: %s\n' \
      "$log_name" "${statuses[0]}" "$log_directory" >&2
    exit "${statuses[0]}"
  fi
  if (( statuses[1] != 0 )); then
    printf 'Sandbox acceptance could not retain %s\n' "$log_name" >&2
    exit "${statuses[1]}"
  fi
}

require_complete_suite() {
  local log_name="$1" test_prefix="$2" required_test missing=0
  shift 2
  if grep -Eq '^[[:space:]]*--- SKIP:' "$log_directory/$log_name"; then
    printf 'Sandbox acceptance rejects skipped tests in %s\n' "$log_name" >&2
    exit 1
  fi
  for required_test in "$@"; do
    if ! grep -Eq "^--- PASS: $required_test \\(" "$log_directory/$log_name"; then
      printf 'Missing required passing contract: %s\n' "$required_test" >&2
      missing=1
    fi
  done
  if (( missing != 0 )); then
    printf 'Rebuild the Sandbox commands, test binaries and Python Bundle from this checkout before T15 acceptance.\n' >&2
    exit 1
  fi
  if ! grep -Eq "^--- PASS: $test_prefix" "$log_directory/$log_name" ||
     ! grep -qx 'PASS' "$log_directory/$log_name"; then
    printf 'Sandbox acceptance has no complete passing suite in %s\n' "$log_name" >&2
    exit 1
  fi
}

run_execution_suite() {
  # An ambient focused selector must never silently narrow joint acceptance.
  SANDBOX_TEST_RUN='^TestSandbox(Execution|Lifecycle)' \
    bash "$repository_root/tests/run-sandbox-init-linux.sh"
}

if [[ -z "${SANDBOX_TEST_BIN_DIR:-}" ]]; then
  # The Execution runner builds the commands, tagged test binary and Python
  # Bundle once. Creation uses those same binaries plus its trusted probe.
  run_logged execution.log run_execution_suite
  require_complete_suite execution.log 'TestSandbox(Execution|Lifecycle)' "${required_execution_tests[@]}"
  sandbox_bin_dir="$repository_root/.cache/sandbox-init"
  cd -- "$repository_root"
  run_logged creation-build.log env CGO_ENABLED=0 go build \
    -o "$sandbox_bin_dir/sandbox-root-probe" ./tests/testdata/sandbox-root-probe
  cp -- "$sandbox_bin_dir/t05-tests" "$sandbox_bin_dir/sandbox-creation-tests"
else
  sandbox_bin_dir="$(cd -- "$SANDBOX_TEST_BIN_DIR" && pwd)"
  run_logged execution.log run_execution_suite
  require_complete_suite execution.log 'TestSandbox(Execution|Lifecycle)' "${required_execution_tests[@]}"
fi

run_creation_suite() {
  SANDBOX_TEST_BIN_DIR="$sandbox_bin_dir" \
    bash "$repository_root/tests/run-sandbox-creation-linux.sh"
}
run_logged creation.log run_creation_suite
require_complete_suite creation.log 'TestSandboxCreation'
printf 'T15 Sandbox kernel acceptance PASS (no skipped tests)\n' | tee "$log_directory/result.log"
