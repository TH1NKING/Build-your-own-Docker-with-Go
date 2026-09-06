#!/usr/bin/env bash
set -euo pipefail

# Real Python acceptance through the Supervisor protocol in a disposable Linux
# mount/PID/network namespace. Locked sources must already be downloaded.
repository_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
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
privilege=()
if [[ "$EUID" != 0 ]]; then privilege=(sudo -n); fi
exec "${privilege[@]}" unshare --mount --pid --fork --mount-proc --net --propagation private \
  bash -c '
    set -euo pipefail
    cd -- "$1"
    mount -t tmpfs -o size=1g,mode=1777,nosuid,nodev tmpfs /tmp
    mkdir /tmp/t05-bin
    cp -- ./sandboxd ./sandbox-init ./sandbox-waiting-init ./profile-bundle ./t05-tests ./python.bundle /tmp/t05-bin/
    cd /
    cgroup_mount=/sys/fs/cgroup
    if [[ "$(stat -f -c %t "$cgroup_mount")" != 63677270 ]]; then
      cgroup_mount=/sys/fs/cgroup/unified
    fi
    [[ "$(stat -f -c %t "$cgroup_mount")" == 63677270 ]] || { echo "cgroup v2 is required" >&2; exit 1; }
    cgroup_root="$(mktemp -d "$cgroup_mount/sandbox-t05-XXXXXX")"
    trap '\''rmdir -- "$cgroup_root"'\'' EXIT
    export SANDBOX_CGROUP_ROOT="$cgroup_root"
    export SANDBOXD_CLI=/tmp/t05-bin/sandboxd SANDBOX_INIT_CLI=/tmp/t05-bin/sandbox-init
    export SANDBOX_WAITING_INIT_CLI=/tmp/t05-bin/sandbox-waiting-init
    export PROFILE_BUNDLE_CLI=/tmp/t05-bin/profile-bundle SANDBOX_EXECUTION_BUNDLE=/tmp/t05-bin/python.bundle
    uname -sr
    /tmp/t05-bin/t05-tests -test.v -test.run="${SANDBOX_TEST_RUN:-^TestSandbox(Execution|Lifecycle)}" -test.timeout=300s
  ' sandbox-init "$sandbox_bin_dir"
