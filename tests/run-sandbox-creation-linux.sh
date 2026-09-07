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
  go build -trimpath -buildvcs=false -ldflags=-buildid= -o "$sandbox_bin_dir/sandbox-init" ./cmd/sandbox-init
  go build -o "$sandbox_bin_dir/profile-bundle" ./cmd/profile-bundle
  go build -o "$sandbox_bin_dir/sandbox-root-probe" ./tests/testdata/sandbox-root-probe
  go test -c -tags='sandbox_root,profilebundle_root' \
    -o "$sandbox_bin_dir/sandbox-creation-tests" ./tests
else
  sandbox_bin_dir="$(cd -- "$SANDBOX_TEST_BIN_DIR" && pwd)"
fi

for binary in sandboxd sandbox-init profile-bundle sandbox-root-probe sandbox-creation-tests; do
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
    cp -- ./sandboxd ./sandbox-init ./profile-bundle ./sandbox-root-probe ./sandbox-creation-tests /tmp/t04-bin/
    cd /
    cgroup_parent="${2:-}"
    if [[ -z "$cgroup_parent" ]]; then
      read -r cgroup_parent < <(findmnt --first-only --noheadings --raw --types cgroup2 --output TARGET) || {
        echo "cgroup v2 is required; set SANDBOX_TEST_CGROUP_PARENT to a delegated parent" >&2
        exit 1
      }
    fi
    if [[ "$cgroup_parent" != /* || ! -d "$cgroup_parent" ]] ||
       [[ "$(stat -f -c %t "$cgroup_parent")" != 63677270 ]]; then
      echo "Sandbox test cgroup parent must be an absolute cgroup v2 directory: $cgroup_parent" >&2
      exit 1
    fi
    cgroup_parent="$(cd -- "$cgroup_parent" && pwd -P)"
    delegated=" $(<"$cgroup_parent/cgroup.subtree_control") "
    for controller in cpu memory pids; do
      if [[ "$delegated" != *" $controller "* ]]; then
        echo "Sandbox test cgroup parent has not delegated $controller: $cgroup_parent; set SANDBOX_TEST_CGROUP_PARENT to a parent with cpu, memory and pids delegated" >&2
        exit 1
      fi
    done
    cgroup_root="$(mktemp -d "$cgroup_parent/sandbox-t04-XXXXXX")"
    cleanup_cgroup() {
      test_status=$?
      if ! rmdir -- "$cgroup_root"; then
        echo "Sandbox acceptance leaked cgroup state: $cgroup_root" >&2
        exit 1
      fi
      exit "$test_status"
    }
    trap cleanup_cgroup EXIT
    export SANDBOX_CGROUP_ROOT="$cgroup_root"
    export SANDBOXD_CLI=/tmp/t04-bin/sandboxd
    export SANDBOX_INIT_CLI=/tmp/t04-bin/sandbox-init
    export PROFILE_BUNDLE_CLI=/tmp/t04-bin/profile-bundle
    export SANDBOX_ROOT_PROBE=/tmp/t04-bin/sandbox-root-probe
    uname -sr
    /tmp/t04-bin/sandbox-creation-tests -test.v -test.run="^TestSandboxCreation" -test.timeout=120s
  ' sandbox-creation "$sandbox_bin_dir" "${SANDBOX_TEST_CGROUP_PARENT:-}"
