#!/usr/bin/env bash

# Shared contract inventory: the runner and the small shell regression suite
# source this file. Executing it selects a bounded, real Worker demonstration.
worker_demo_required_tests() {
  case "$1" in
    normal)
      printf '%s\n' \
        TestWorkerExecutionRunsPythonAndReportsItsBoundedResult \
        TestWorkerExecutionCommandUsesAnUnprivilegedAccountAndPrivateCA \
        TestWorkerExecutionPreservesDeclaredBinaryOutputs
      ;;
    capacity)
      printf '%s\n' \
        TestWorkerAPICompetingClaimsHaveOneOwner \
        TestWorkerAPICapacityBoundsConcurrentClaimsForOneWorker \
        TestWorkerAPICapacityWaitsForCleanupRelease \
        TestWorkerExecutionCapacityRunsTwoSandboxesAndQueuesTheThird \
        TestWorkerExecutionCleanupFailureRetainsCapacityAndStopsTheNode
      ;;
    recovery)
      printf '%s\n' \
        TestWorkerExecutionRetriesLostAcknowledgementWithoutRunningPythonAgain \
        TestWorkerExecutionRecoversTheSameSandboxAfterHTTPSDisconnection \
        TestWorkerExecutionHealthyHeartbeatsDoNotHideReportFailure \
        TestWorkerExecutionCommittedResultAcknowledgementHasABoundedWait
      ;;
    expiry)
      printf '%s\n' \
        TestWorkerAPIRecoveryWindowExpiresWithoutReassignmentOrCapacityRelease \
        TestWorkerExecutionRecoveryExpiryCleansUpWithoutReplayingWork
      ;;
    all)
      local mode
      for mode in normal capacity recovery expiry; do worker_demo_required_tests "$mode"; done
      ;;
    acceptance)
      # Preserve the full Linux acceptance inventory independently of the
      # focused demo cases. The runner also retains its broad test selector.
      printf '%s\n' \
        TestWorkerExecutionRunsPythonAndReportsItsBoundedResult \
        TestWorkerExecutionRetriesLostAcknowledgementWithoutRunningPythonAgain \
        TestWorkerExecutionCommandUsesAnUnprivilegedAccountAndPrivateCA \
        TestWorkerExecutionPreservesDeclaredBinaryOutputs \
        TestWorkerExecutionCapacityRunsTwoSandboxesAndQueuesTheThird \
        TestWorkerExecutionCleanupFailureRetainsCapacityAndStopsTheNode \
        TestWorkerExecutionRecoversTheSameSandboxAfterHTTPSDisconnection \
        TestWorkerExecutionRecoveryExpiryCleansUpWithoutReplayingWork \
        TestWorkerExecutionHealthyHeartbeatsDoNotHideReportFailure \
        TestWorkerExecutionCommittedResultAcknowledgementHasABoundedWait \
        TestSandboxInspectionRequiresTheOriginalLiveSandbox \
        TestSandboxInspectionDuringExecutionDoesNotCancelTheWorkload \
        TestSandboxInspectionRejectsLostInitDespiteRetainedResult
      ;;
    *) printf 'Unknown Worker suite: %s\n' "$1" >&2; return 2 ;;
  esac
}

worker_demo_selector() {
  local mode="$1" inventory
  inventory="$(worker_demo_required_tests "$mode")" || return
  if [[ "$mode" == acceptance ]]; then
    printf '%s\n' '^(TestWorkerExecution|TestSandboxInspection)'
  else
    local tests=()
    mapfile -t tests <<< "$inventory"
    local IFS='|'
    printf '^(%s)$\n' "${tests[*]}"
  fi
}

worker_require_passing_tests() {
  local log_file="$1" required_test scenario
  shift
  if [[ $# == 0 ]]; then
    echo 'Worker suite must require at least one test' >&2
    return 1
  fi
  if grep -Eq '^[[:space:]]*--- (SKIP|FAIL):|^FAIL$' "$log_file"; then
    echo 'Worker suite rejects skipped or failed tests' >&2
    return 1
  fi
  for required_test in "$@"; do
    if ! grep -Eq "^--- PASS: $required_test \\(" "$log_file"; then
      printf 'Worker suite did not pass %s; rebuild test commands\n' "$required_test" >&2
      return 1
    fi
    if [[ "$required_test" == TestWorkerExecutionRecoversTheSameSandboxAfterHTTPSDisconnection ]]; then
      # A passing parent alone must not hide a stale binary missing the
      # result-pending or unresponsive-HTTPS recovery scenarios.
      for scenario in during_execution pending_result pending_result_blackhole; do
        if ! grep -Eq "^[[:space:]]*--- PASS: $required_test/$scenario \\(" "$log_file"; then
          printf 'Worker recovery suite did not pass %s/%s\n' "$required_test" "$scenario" >&2
          return 1
        fi
      done
    fi
  done
  if ! grep -qx 'PASS' "$log_file"; then
    echo 'Worker suite did not finish successfully' >&2
    return 1
  fi
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  set -euo pipefail
  case "${1:-all}" in
    normal|capacity|recovery|expiry|all) ;;
    *) echo "Usage: $0 [normal|capacity|recovery|expiry|all]" >&2; exit 2 ;;
  esac
  if [[ $# -gt 1 ]]; then echo 'Expected at most one demo selection' >&2; exit 2; fi
  repository_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
  printf 'Worker demo: %s (real PostgreSQL, HTTPS and Linux Sandboxes)\n' "${1:-all}"
  echo 'Watch DEMO lines for observed state; only the final WORKER_DEMO marker means the whole selected suite passed.'
  exec bash "$repository_root/tests/run-worker-execution-linux.sh" --demo "${1:-all}"
fi
