#!/usr/bin/env bash
# Shared by the real-kernel runners. An exit status of zero alone is not proof
# that Go ran a test: an empty selector and skipped tests both exit successfully.

sandbox_run_logged() {
  local log_file="$1"
  shift
  local statuses
  if "$@" 2>&1 | tee "$log_file"; then
    return 0
  else
    statuses=("${PIPESTATUS[@]}")
    if (( statuses[0] != 0 )); then
      printf 'Sandbox runner failed (exit %s); log: %s\n' "${statuses[0]}" "$log_file" >&2
      return "${statuses[0]}"
    fi
    printf 'Sandbox runner could not retain log: %s\n' "$log_file" >&2
    return "${statuses[1]}"
  fi
}

sandbox_require_passed_tests() {
  local log_file="$1" required_test missing=0
  shift
  if grep -Eq '^[[:space:]]*--- (SKIP|FAIL):' "$log_file"; then
    printf 'Sandbox validation rejects skipped or failed tests; log: %s\n' "$log_file" >&2
    return 1
  fi
  if ! grep -Eq '^--- PASS: Test[^[:space:]]+ \(' "$log_file" ||
     ! grep -qx 'PASS' "$log_file"; then
    printf 'Sandbox validation has no complete passing test suite; log: %s\n' "$log_file" >&2
    return 1
  fi
  for required_test in "$@"; do
    # Use a fixed string so test names cannot accidentally become regexes.
    if ! grep -F -- "--- PASS: $required_test (" "$log_file" | grep -q '^--- PASS:'; then
      printf 'Missing required passing contract: %s\n' "$required_test" >&2
      missing=1
    fi
  done
  (( missing == 0 ))
}

sandbox_select_contracts() {
  # Each argument is an exact top-level Go test name from this checkout.
  export SANDBOX_TEST_REQUIRED_TESTS
  export SANDBOX_TEST_RUN
  SANDBOX_TEST_REQUIRED_TESTS="$(printf '%s\n' "$@")"
  local joined
  joined="$(IFS='|'; printf '%s' "$*")"
  SANDBOX_TEST_RUN="^($joined)\$"
}
