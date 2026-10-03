#!/usr/bin/env bash
set -euo pipefail

# Real Python acceptance through the Supervisor protocol in a disposable Linux
# mount/PID/network namespace. Locked sources must already be downloaded.
repository_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
source "$repository_root/tests/sandbox-test-result.sh"
if [[ -z "${SANDBOX_TEST_BIN_DIR:-}" ]]; then
  sandbox_bin_dir="$repository_root/.cache/sandbox-init"
  source_cache="${PROFILE_BUNDLE_SOURCE_CACHE:-$repository_root/.cache/profile-sources}"
  mkdir -p -- "$sandbox_bin_dir"
  cd -- "$repository_root"
  export CGO_ENABLED=0
  go build -o "$sandbox_bin_dir/sandboxd" ./cmd/sandboxd
  go build -trimpath -buildvcs=false -ldflags=-buildid= -o "$sandbox_bin_dir/sandbox-init" ./cmd/sandbox-init
  go build -o "$sandbox_bin_dir/sandbox-waiting-init" ./tests/testdata/sandbox-waiting-init
  go build -o "$sandbox_bin_dir/profile-bundle" ./cmd/profile-bundle
  go test -c -tags='sandbox_root,profilebundle_root' -o "$sandbox_bin_dir/t05-tests" ./tests
  bundle_build_dir="$(mktemp -d "$sandbox_bin_dir/bundle-XXXXXX")"
  "$sandbox_bin_dir/profile-bundle" build \
    --lock profiles/python-data-v1/profile.lock.json --source-cache "$source_cache" \
    --system-call-policy profiles/python-data-v1/system-call-policy.json \
    --sandbox-init "$sandbox_bin_dir/sandbox-init" --output "$bundle_build_dir/python.bundle"
  mv -- "$bundle_build_dir/python.bundle" "$sandbox_bin_dir/python.bundle"
  rmdir -- "$bundle_build_dir"
else
  sandbox_bin_dir="$(cd -- "$SANDBOX_TEST_BIN_DIR" && pwd)"
fi
for binary in sandboxd sandbox-init sandbox-waiting-init profile-bundle t05-tests; do
  [[ -x "$sandbox_bin_dir/$binary" ]] || { echo "Missing executable: $sandbox_bin_dir/$binary" >&2; exit 1; }
done
[[ -f "$sandbox_bin_dir/python.bundle" ]] || { echo 'Missing python.bundle' >&2; exit 1; }
selector="${SANDBOX_TEST_RUN:-^TestSandbox(Execution|Lifecycle)}"
# List before entering namespaces so stale or mismatched binaries fail before
# privileged setup. Go separates a subtest selector with '/', if supplied.
inventory="$("$sandbox_bin_dir/t05-tests" -test.list="${selector%%/*}")"
mapfile -t selected_tests < <(printf '%s\n' "$inventory" | grep -E '^Test[^[:space:]]+$')
if (( ${#selected_tests[@]} == 0 )); then
  printf 'No Sandbox tests match %s; rebuild binaries from this checkout.\n' "$selector" >&2
  exit 1
fi
while IFS= read -r required_test; do
  [[ -z "$required_test" ]] && continue
  if ! printf '%s\n' "$inventory" | grep -Fxq -- "$required_test"; then
    printf 'Missing required contract in test binary: %s; rebuild binaries from this checkout.\n' "$required_test" >&2
    exit 1
  fi
done <<< "${SANDBOX_TEST_REQUIRED_TESTS:-}"
log_parent="${SANDBOX_TEST_LOG_DIR:-${SANDBOX_DEMO_LOG_DIR:-$repository_root/.cache/sandbox-tests}}"
mkdir -p -- "$log_parent"
log_parent="$(cd -- "$log_parent" && pwd)"
log_directory="$(mktemp -d "$log_parent/run-$(date -u +%Y%m%dT%H%M%SZ)-XXXXXX")"
printf 'Sandbox test logs: %s\n' "$log_directory"
{
  uname -sr
  printf 'Selector: %s\n' "$selector"
  printf 'Expected contracts: %s\n' "${#selected_tests[@]}"
  printf 'Binary directory: %s\n' "$sandbox_bin_dir"
  (cd -- "$sandbox_bin_dir" && sha256sum sandboxd sandbox-init t05-tests python.bundle)
} > "$log_directory/environment.log"
privilege=()
if [[ "$EUID" != 0 ]]; then privilege=(sudo -n); fi
sandbox_run_logged "$log_directory/execution.log" "${privilege[@]}" unshare --mount --pid --fork --mount-proc --net --propagation private \
  bash -c '
    set -euo pipefail
    # Stdin carries the trusted helper across sudo and a covered host /tmp.
    source /proc/self/fd/0
    exec </dev/null
    cd -- "$1"
    mount -t tmpfs -o size=1g,mode=1777,nosuid,nodev tmpfs /tmp
    mkdir /tmp/t05-bin
    cp -- ./sandboxd ./sandbox-init ./sandbox-waiting-init ./profile-bundle ./t05-tests ./python.bundle /tmp/t05-bin/
    cd /
    prepare_sandbox_test_cgroup sandbox-t05 "${2:-}"
    export SANDBOXD_CLI=/tmp/t05-bin/sandboxd SANDBOX_INIT_CLI=/tmp/t05-bin/sandbox-init
    export SANDBOX_WAITING_INIT_CLI=/tmp/t05-bin/sandbox-waiting-init
    export PROFILE_BUNDLE_CLI=/tmp/t05-bin/profile-bundle SANDBOX_EXECUTION_BUNDLE=/tmp/t05-bin/python.bundle
    uname -sr
    /tmp/t05-bin/t05-tests -test.v -test.run="${3:-^TestSandbox(Execution|Lifecycle)}" -test.timeout=300s
  ' sandbox-init "$sandbox_bin_dir" "${SANDBOX_TEST_CGROUP_PARENT:-}" "$selector" < "$repository_root/tests/sandbox-cgroup-fixture.sh"
sandbox_require_passed_tests "$log_directory/execution.log" "${selected_tests[@]}"
printf 'Sandbox tests PASS (%s contracts; no skipped tests)\n' "${#selected_tests[@]}" | tee "$log_directory/result.log"
