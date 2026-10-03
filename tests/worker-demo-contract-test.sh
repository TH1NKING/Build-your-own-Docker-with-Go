#!/usr/bin/env bash
set -euo pipefail

# These fixtures exercise only the evidence gate and selectors. They do not
# claim PostgreSQL, HTTPS or kernel acceptance and never execute the runner.
repository_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
source "$repository_root/tests/run-worker-demo-linux.sh"
temporary="$(mktemp -d)"
trap 'rm -f -- "$temporary"/*.log "$temporary/diagnostic"; rmdir -- "$temporary"' EXIT

expect_rejected() {
  if worker_require_passing_tests "$@" > "$temporary/diagnostic" 2>&1; then
    echo 'Worker evidence gate accepted an incomplete or failing fixture' >&2
    exit 1
  fi
}

for mode in normal capacity recovery expiry all acceptance; do
  inventory="$(worker_demo_required_tests "$mode")"
  mapfile -t required_tests <<< "$inventory"
  selector="$(worker_demo_selector "$mode")"
  [[ "${#required_tests[@]}" -gt 0 ]] || { echo "Empty inventory: $mode" >&2; exit 1; }
  : > "$temporary/complete.log"
  for required_test in "${required_tests[@]}"; do
    [[ "$required_test" =~ $selector ]] || { echo "Selector omitted $required_test" >&2; exit 1; }
    if [[ "$mode" != acceptance && "${required_test}UnexpectedSuffix" =~ $selector ]]; then
      echo "Focused selector also matched a different test: $required_test" >&2
      exit 1
    fi
    grep -Eq "^func $required_test\\(t \\*testing.T\\)" "$repository_root"/tests/*_test.go || {
      echo "Required contract does not exist: $required_test" >&2; exit 1;
    }
    printf '%s\n' "--- PASS: $required_test (0.01s)" >> "$temporary/complete.log"
    if [[ "$required_test" == TestWorkerExecutionRecoversTheSameSandboxAfterHTTPSDisconnection ]]; then
      for scenario in during_execution pending_result pending_result_blackhole; do
        printf '    --- PASS: %s/%s (0.01s)\n' "$required_test" "$scenario" >> "$temporary/complete.log"
      done
    fi
  done
  [[ ! TestUnrelated =~ $selector ]] || { echo 'Selector included unrelated tests' >&2; exit 1; }
  echo PASS >> "$temporary/complete.log"
  worker_require_passing_tests "$temporary/complete.log" "${required_tests[@]}"

  # Go's test binary exits successfully for a selector with no matching tests.
  printf 'testing: warning: no tests to run\nPASS\n' > "$temporary/zero.log"
  expect_rejected "$temporary/zero.log" "${required_tests[@]}"
  expect_rejected "$temporary/complete.log"

  grep -v -F -- "--- PASS: ${required_tests[0]} (" "$temporary/complete.log" > "$temporary/missing.log"
  expect_rejected "$temporary/missing.log" "${required_tests[@]}"
  grep -v '^PASS$' "$temporary/complete.log" > "$temporary/unfinished.log"
  expect_rejected "$temporary/unfinished.log" "${required_tests[@]}"

  for bad_result in SKIP FAIL; do
    cp -- "$temporary/complete.log" "$temporary/bad.log"
    printf '    --- %s: TestExtra/nested (0.01s)\n' "$bad_result" >> "$temporary/bad.log"
    expect_rejected "$temporary/bad.log" "${required_tests[@]}"
  done
  if [[ "$mode" == recovery || "$mode" == all || "$mode" == acceptance ]]; then
    grep -v '/pending_result_blackhole ' "$temporary/complete.log" > "$temporary/stale.log"
    expect_rejected "$temporary/stale.log" "${required_tests[@]}"
  fi
done

[[ "$(worker_demo_selector acceptance)" == '^(TestWorkerExecution|TestSandboxInspection)' ]] || {
  echo 'Full acceptance must retain its broad selector' >&2; exit 1;
}
if worker_demo_required_tests unknown > "$temporary/diagnostic" 2>&1; then
  echo 'Unknown suite was accepted' >&2; exit 1
fi
for script in run-worker-demo-linux.sh run-worker-execution-linux.sh; do
  set +e
  bash "$repository_root/tests/$script" unknown > "$temporary/diagnostic" 2>&1
  status=$?
  set -e
  [[ "$status" == 2 ]] || { echo "Invalid $script arguments must fail before prerequisites" >&2; exit 1; }
done
echo 'Worker demo selectors and evidence gates PASS (synthetic logs only)'
