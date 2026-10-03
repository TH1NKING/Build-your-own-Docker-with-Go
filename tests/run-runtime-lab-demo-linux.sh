#!/usr/bin/env bash
set -euo pipefail

# Runtime Lab is an educational chroot/network demo, not the production
# Sandbox security boundary. Keep its bridge, mounts and cgroups disposable.
repository_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
ip -Version 2>/dev/null | grep -q iproute2 || {
  echo 'Runtime Lab requires iproute2, not the BusyBox ip applet.' >&2; exit 1;
}
source_cache="${PROFILE_BUNDLE_SOURCE_CACHE:-$repository_root/.cache/profile-sources}"
python3 "$repository_root/tests/prepare-profile-sources.py" --cache "$source_cache" --check
source_cache="$(cd -- "$source_cache" && pwd)"
archive="$(python3 - "$repository_root/profiles/python-data-v1/profile.lock.json" "$source_cache" <<'PY'
import json
from pathlib import Path
import sys
with open(sys.argv[1], encoding="utf-8") as stream:
    entries = [entry for entry in json.load(stream)["inputs"] if entry["id"] == "musl-runtime-loader"]
if len(entries) != 1:
    raise SystemExit("lock must identify exactly one Alpine Runtime Lab source")
print(Path(sys.argv[2]) / entries[0]["filename"])
PY
)"

runtime_cli="${RUNTIME_LAB_CLI:-${SANDBOX_TEST_BIN_DIR:+$SANDBOX_TEST_BIN_DIR/runtime-lab}}"
if [[ -z "$runtime_cli" ]]; then
  runtime_cli="$repository_root/.cache/runtime-lab-demo/runtime-lab"
  mkdir -p -- "$(dirname -- "$runtime_cli")"
  (cd -- "$repository_root" && CGO_ENABLED=0 go build -o "$runtime_cli" ./cmd/runtime-lab)
fi
[[ -x "$runtime_cli" ]] || { echo "Missing executable: $runtime_cli" >&2; exit 1; }
runtime_cli="$(python3 -c 'from pathlib import Path; import sys; print(Path(sys.argv[1]).resolve(strict=True))' "$runtime_cli")"
privilege=()
if [[ "$EUID" != 0 ]]; then privilege=(sudo -n); fi

exec "${privilege[@]}" unshare --mount --pid --fork --mount-proc --net --propagation private \
  bash -c '
    set -euo pipefail
    source /proc/self/fd/0
    exec </dev/null
    # Open inputs before hiding the host /tmp, where a checkout may reside.
    exec 3<"$1" 4<"$2"
    cd /
    mount -t tmpfs -o size=512m,mode=1777,nosuid,nodev tmpfs /tmp
    mkdir /tmp/runtime-lab-demo
    cd /tmp/runtime-lab-demo
    cat <&3 > ./runtime-lab
    chmod 0755 ./runtime-lab
    mkdir rootfs
    tar -xzof /proc/self/fd/4 -C rootfs
    exec 3<&- 4<&-
    prepare_sandbox_test_cgroup runtime-lab-demo "${3:-}"
    # The old Lab uses a fixed /sys/fs/cgroup/mydocker path. Cover that mount
    # with our fresh, empty subtree so it cannot touch an existing Lab run.
    printf "+cpu +memory\n" > "$SANDBOX_CGROUP_ROOT/cgroup.subtree_control"
    mount --bind "$SANDBOX_CGROUP_ROOT" /sys/fs/cgroup
    cgroup_covered=1
    cleanup_runtime_lab() {
      local status="${1:-0}"
      if [[ "$cgroup_covered" == 1 ]]; then
        if umount /sys/fs/cgroup; then cgroup_covered=0; else status=1; fi
      fi
      if [[ "$cgroup_covered" == 0 ]]; then
        if [[ -d "$SANDBOX_CGROUP_ROOT/mydocker" ]]; then
          rmdir -- "$SANDBOX_CGROUP_ROOT/mydocker" || status=1
        fi
        rmdir -- "$SANDBOX_CGROUP_ROOT" || status=1
      fi
      return "$status"
    }
    trap "status=\$?; cleanup_runtime_lab \"\$status\"; exit \$?" EXIT
    echo "Runtime Lab: disposable mount/PID/network namespaces; independent cgroup subtree"
    uname -sr
    timeout -k 3 20 ./runtime-lab -m 64M -c 20 10.200.1.2 /bin/sh -ec "
      test \"\$\$\" -eq 1
      printf \"RUNTIME_LAB_PID1=%s\\n\" \"\$\$\"
      hostname
      ip addr show dev eth0 | grep 10.200.1.2/24
      ping -c 1 -W 2 10.200.1.1
      echo RUNTIME_LAB_BRIDGE_PING_OK
      echo overlay-state > /interview-demo-state
      cat /proc/self/cgroup
    " > first.log 2>&1 || { cat first.log; exit 1; }
    cat first.log
    first_id="$(sed -n "s/^=== Allocating New Container ID: \([0-9a-f]\{8\}\) ===$/\1/p" first.log)"
    [[ "$first_id" =~ ^[0-9a-f]{8}$ ]]
    grep -Fxq RUNTIME_LAB_PID1=1 first.log
    grep -Fxq RUNTIME_LAB_BRIDGE_PING_OK first.log
    [[ "$(<"containers/$first_id/upper/interview-demo-state")" == overlay-state ]]
    [[ ! -e rootfs/interview-demo-state ]]
    echo RUNTIME_LAB_OVERLAY_LOWER_UNCHANGED

    status=0
    timeout -k 3 20 ./runtime-lab 10.200.1.2 /bin/sh -c "exit 7" > failure.log 2>&1 || status=$?
    cat failure.log
    [[ "$status" == 7 ]] || { echo "Expected child exit 7, got $status" >&2; exit 1; }
    second_id="$(sed -n "s/^=== Allocating New Container ID: \([0-9a-f]\{8\}\) ===$/\1/p" failure.log)"
    [[ "$second_id" =~ ^[0-9a-f]{8}$ && "$first_id" != "$second_id" ]]
    echo "RUNTIME_LAB_FAILURE_EXIT=7; distinct IDs: $first_id $second_id"

    timeout -k 3 20 ./runtime-lab -id "$first_id" 10.200.1.2 /bin/sh -ec "
      test \"\$(cat /interview-demo-state)\" = overlay-state
      echo RUNTIME_LAB_RESUME_OK
    " > resume.log 2>&1 || { cat resume.log; exit 1; }
    cat resume.log
    grep -Fxq RUNTIME_LAB_RESUME_OK resume.log
    [[ ! -e rootfs/interview-demo-state ]]
    cleanup_runtime_lab 0
    trap - EXIT
    echo RUNTIME_LAB_DEMO_PASS
  ' runtime-lab-demo "$runtime_cli" "$archive" "${SANDBOX_TEST_CGROUP_PARENT:-}" \
  < "$repository_root/tests/sandbox-cgroup-fixture.sh"
