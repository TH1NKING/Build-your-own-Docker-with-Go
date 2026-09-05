#!/usr/bin/env bash
set -euo pipefail

# Run only on a Linux test machine. All test mounts, processes, networking and
# temporary files live in a disposable namespace, including a private /tmp.
repository_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
if [[ -z "${SANDBOX_TEST_BIN_DIR:-}" ]]; then
  sandbox_bin_dir="$repository_root/.cache/sandbox-creation"
  mkdir -p -- "$sandbox_bin_dir"
  cd -- "$repository_root"
  export CGO_ENABLED=0
  go build -o "$sandbox_bin_dir/sandboxd" ./cmd/sandboxd
  go build -o "$sandbox_bin_dir/profile-bundle" ./cmd/profile-bundle
  go build -o "$sandbox_bin_dir/sandbox-root-probe" ./tests/testdata/sandbox-root-probe
  go test -c -tags='sandbox_root,profilebundle_root' \
    -o "$sandbox_bin_dir/sandbox-creation-tests" ./tests
else
  sandbox_bin_dir="$(cd -- "$SANDBOX_TEST_BIN_DIR" && pwd)"
fi

for binary in sandboxd profile-bundle sandbox-root-probe sandbox-creation-tests; do
  [[ -x "$sandbox_bin_dir/$binary" ]] || { echo "Missing executable: $sandbox_bin_dir/$binary" >&2; exit 1; }
done

privilege=()
if [[ "$EUID" != 0 ]]; then
  privilege=(sudo -n)
fi

exec "${privilege[@]}" unshare --mount --pid --fork --mount-proc --net --propagation private \
  bash -c '
    set -euo pipefail
    cd -- "$1"
    mount -t tmpfs -o size=512m,mode=1777,nosuid,nodev tmpfs /tmp
    # Inherited cwd keeps the binaries reachable even when the checkout or
    # supplied bin directory is under the now-covered host /tmp.
    mkdir /tmp/t04-bin
    cp -- ./sandboxd ./profile-bundle ./sandbox-root-probe ./sandbox-creation-tests /tmp/t04-bin/
    cd /
    export SANDBOXD_CLI=/tmp/t04-bin/sandboxd
    export PROFILE_BUNDLE_CLI=/tmp/t04-bin/profile-bundle
    export SANDBOX_ROOT_PROBE=/tmp/t04-bin/sandbox-root-probe
    uname -sr
    exec /tmp/t04-bin/sandbox-creation-tests -test.v -test.run="^TestSandboxCreation" -test.timeout=120s
  ' sandbox-creation "$sandbox_bin_dir"
