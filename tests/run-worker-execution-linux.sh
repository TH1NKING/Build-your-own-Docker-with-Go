#!/usr/bin/env bash
set -euo pipefail

# The trusted Worker and Control Plane keep the test host network so they can
# reach the dedicated PostgreSQL instance. Each Workload still receives its own
# Network Policy none namespace from the real Sandbox Supervisor.
repository_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
[[ -n "${AGENT_TEST_DATABASE_URL:-}" ]] || {
  echo 'AGENT_TEST_DATABASE_URL must point to a dedicated test PostgreSQL instance' >&2
  exit 1
}
if [[ -n "${WORKER_ACCEPTANCE_LOG_DIR:-}" ]]; then
  mkdir -p -- "$WORKER_ACCEPTANCE_LOG_DIR"
  log_dir="$(cd -- "$WORKER_ACCEPTANCE_LOG_DIR" && pwd)"
  for previous_log in build.log environment.log execution.log result.log; do
    [[ ! -e "$log_dir/$previous_log" ]] || {
      echo 'WORKER_ACCEPTANCE_LOG_DIR already contains acceptance logs; choose a fresh directory' >&2
      exit 1
    }
  done
else
  mkdir -p -- "$repository_root/.cache/worker-acceptance"
  log_dir="$(mktemp -d "$repository_root/.cache/worker-acceptance/run-$(date -u +%Y%m%dT%H%M%SZ)-XXXXXX")"
fi
printf 'Worker acceptance logs: %s\n' "$log_dir"

if [[ -z "${SANDBOX_TEST_BIN_DIR:-}" ]]; then
  sandbox_bin_dir="$log_dir/bin"
  source_cache="${PROFILE_BUNDLE_SOURCE_CACHE:-$repository_root/.cache/profile-sources}"
  mkdir -p -- "$sandbox_bin_dir"
  (
    cd -- "$repository_root"
    export CGO_ENABLED=0
    go build -o "$sandbox_bin_dir/sandboxd" ./cmd/sandboxd
    go build -trimpath -buildvcs=false -ldflags=-buildid= -o "$sandbox_bin_dir/sandbox-init" ./cmd/sandbox-init
    go build -o "$sandbox_bin_dir/sandbox-waiting-init" ./tests/testdata/sandbox-waiting-init
    go build -o "$sandbox_bin_dir/worker" ./cmd/worker
    go build -o "$sandbox_bin_dir/profile-bundle" ./cmd/profile-bundle
    go test -c -tags='sandbox_root,profilebundle_root' -o "$sandbox_bin_dir/t05-tests" ./tests
    "$sandbox_bin_dir/profile-bundle" build \
      --lock profiles/python-data-v1/profile.lock.json --source-cache "$source_cache" \
      --system-call-policy profiles/python-data-v1/system-call-policy.json \
      --sandbox-init "$sandbox_bin_dir/sandbox-init" --output "$sandbox_bin_dir/python.bundle"
  ) 2>&1 | tee "$log_dir/build.log"
else
  sandbox_bin_dir="$(cd -- "$SANDBOX_TEST_BIN_DIR" && pwd)"
fi
for binary in sandboxd sandbox-init sandbox-waiting-init worker profile-bundle t05-tests; do
  [[ -x "$sandbox_bin_dir/$binary" ]] || { echo "Missing executable: $sandbox_bin_dir/$binary" >&2; exit 1; }
done
[[ -f "$sandbox_bin_dir/python.bundle" ]] || { echo 'Missing python.bundle' >&2; exit 1; }
{
  uname -sr
  date -u +%Y-%m-%dT%H:%M:%SZ
  sha256sum "$sandbox_bin_dir/sandboxd" "$sandbox_bin_dir/worker" "$sandbox_bin_dir/t05-tests" "$sandbox_bin_dir/python.bundle"
} > "$log_dir/environment.log"

privilege=()
if [[ "$EUID" != 0 ]]; then privilege=(sudo -n); fi
{
  cat -- "$repository_root/tests/sandbox-cgroup-fixture.sh"
  printf '\nexport AGENT_TEST_DATABASE_URL=%q\n' "$AGENT_TEST_DATABASE_URL"
} | "${privilege[@]}" unshare --mount --pid --fork --mount-proc --propagation private \
  bash -c '
    set -euo pipefail
    # Stdin carries the helper and test-only connection string across sudo,
    # without exposing the connection string in process arguments or logs.
    source /proc/self/fd/0
    exec </dev/null
    cd -- "$1"
    mount -t tmpfs -o size=1g,mode=1777,nosuid,nodev tmpfs /tmp
    mkdir /tmp/worker-bin
    cp -- ./sandboxd ./sandbox-init ./sandbox-waiting-init ./worker ./profile-bundle ./t05-tests ./python.bundle /tmp/worker-bin/
    cd /
    prepare_sandbox_test_cgroup sandbox-worker "${2:-}"
    export SANDBOXD_CLI=/tmp/worker-bin/sandboxd SANDBOX_INIT_CLI=/tmp/worker-bin/sandbox-init
    export SANDBOX_WAITING_INIT_CLI=/tmp/worker-bin/sandbox-waiting-init
    export WORKER_CLI=/tmp/worker-bin/worker
    export PROFILE_BUNDLE_CLI=/tmp/worker-bin/profile-bundle SANDBOX_EXECUTION_BUNDLE=/tmp/worker-bin/python.bundle
    uname -sr
    /tmp/worker-bin/t05-tests -test.v -test.run="^TestWorkerExecution" -test.timeout=300s
  ' worker-execution "$sandbox_bin_dir" "${SANDBOX_TEST_CGROUP_PARENT:-}" \
  2>&1 | tee "$log_dir/execution.log"

if grep -Eq '^[[:space:]]*--- SKIP:' "$log_dir/execution.log"; then
  echo 'Worker acceptance must not skip tests' >&2
  exit 1
fi
required_tests=(
  TestWorkerExecutionRunsPythonAndReportsItsBoundedResult
  TestWorkerExecutionRetriesLostAcknowledgementWithoutRunningPythonAgain
  TestWorkerExecutionCommandUsesAnUnprivilegedAccountAndPrivateCA
  TestWorkerExecutionPreservesDeclaredBinaryOutputs
)
for required_test in "${required_tests[@]}"; do
  if ! grep -Eq "^--- PASS: $required_test " "$log_dir/execution.log"; then
    echo "Worker acceptance did not pass $required_test; rebuild test commands" >&2
    exit 1
  fi
done
if ! grep -Eq '^PASS$' "$log_dir/execution.log"; then
  echo 'Worker acceptance did not finish successfully' >&2
  exit 1
fi
printf '%s\n' 'T24_WORKER_EXECUTION_PASS' | tee "$log_dir/result.log"
