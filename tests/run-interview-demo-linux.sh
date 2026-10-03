#!/usr/bin/env bash
set -euo pipefail

repository_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
cd -- "$repository_root"
usage() {
  echo 'Usage: bash tests/run-interview-demo-linux.sh doctor|prepare|run [all|runtime|files|lifecycle|security|resources|worker|capacity|recovery|expiry]|verify'
}
operation="${1:-}"
selection="${2:-all}"
case "$operation" in doctor|prepare|verify) [[ $# == 1 ]] || { usage >&2; exit 2; } ;;
  run) [[ $# -le 2 ]] || { usage >&2; exit 2; }
    case "$selection" in all|runtime|files|lifecycle|security|resources|worker|capacity|recovery|expiry) ;; *) usage >&2; exit 2 ;; esac ;;
  -h|--help) usage; exit 0 ;;
  *) usage >&2; exit 2 ;;
esac
[[ "$(uname -s)" == Linux && "$(uname -m)" == x86_64 ]] || {
  echo 'The locked Python Profile requires x86_64 Linux; use the Ubuntu VM.' >&2; exit 1;
}
for command in bash python3 sha256sum; do
  command -v "$command" >/dev/null || { echo "Missing command: $command" >&2; exit 1; }
done
build_parent="$repository_root/.cache/interview-builds"
build_pointer="$build_parent/current"

doctor() {
  local command
  for command in go make unshare nsenter mount mountpoint findmnt stat ip setpriv timeout tar; do
    command -v "$command" >/dev/null || { echo "Missing command: $command" >&2; return 1; }
  done
  ip -Version 2>/dev/null | grep -q iproute2 || {
    echo 'Install iproute2; the BusyBox ip applet does not provide the required veth commands.' >&2; return 1;
  }
  local required installed
  required="$(awk '$1 == "go" { print $2; exit }' go.mod)"
  installed="$(GOTOOLCHAIN=local go env GOVERSION)"
  python3 - "$required" "$installed" <<'PY'
import re, sys
def version(text):
    match = re.fullmatch(r'(?:go)?(\d+)\.(\d+)(?:\.(\d+))?', text)
    if not match:
        raise SystemExit('A released Go toolchain is required')
    return tuple(int(x or 0) for x in match.groups())
if version(sys.argv[2]) < version(sys.argv[1]):
    raise SystemExit('Install Go >= ' + sys.argv[1] + ' before preparing offline binaries')
PY
  local privilege=()
  if [[ "$EUID" != 0 ]]; then
    command -v sudo >/dev/null || { echo 'Missing command: sudo' >&2; return 1; }
    sudo -n true || { echo 'Run sudo -v in this terminal, then retry doctor.' >&2; return 1; }
    privilege=(sudo -n)
  fi
  # Probe mount/PID/network/user namespaces and a fresh cgroup child. Never
  # enable controllers or change the supplied parent on behalf of the operator.
  "${privilege[@]}" unshare --mount --pid --fork --mount-proc --net --propagation private \
    bash -c 'set -euo pipefail
      source /proc/self/fd/0
      exec </dev/null
      prepare_sandbox_test_cgroup interview-preflight "${1:-}"
      test -e "$SANDBOX_CGROUP_ROOT/cgroup.kill"
      unshare --user --map-root-user true
    ' doctor "${SANDBOX_TEST_CGROUP_PARENT:-}" < tests/sandbox-cgroup-fixture.sh
  printf 'INTERVIEW_ENVIRONMENT_READY kernel=%s go=%s\n' "$(uname -r)" "$installed"
  echo 'This is a capability preflight. The selected kernel/DB scenarios must still pass.'
}

prepare() {
  command -v go >/dev/null || { echo 'Install the Go version required by go.mod.' >&2; return 1; }
  python3 tests/prepare-profile-sources.py
  mkdir -p -- "$build_parent"
  local build_dir command
  build_dir="$(mktemp -d "$build_parent/build-$(date -u +%Y%m%dT%H%M%SZ)-XXXXXX")"
  # Detect source edits while the compiler is reading this checkout.
  python3 -c 'import runpy,json; m=runpy.run_path("tests/interview-build-manifest.py"); print(json.dumps(m["sources"](m["ROOT"]),sort_keys=True))' > "$build_dir/source-inputs.json"
  # Ambient workspaces and build flags may point Go at inputs outside this
  # checkout, which the source manifest cannot attest.
  export CGO_ENABLED=0 GOTOOLCHAIN=local GOWORK=off GOFLAGS=
  go mod download
  for command in sandboxd worker agentctl control-plane runtime-lab profile-bundle; do
    go build -o "$build_dir/$command" "./cmd/$command"
  done
  go build -trimpath -buildvcs=false -ldflags=-buildid= -o "$build_dir/sandbox-init" ./cmd/sandbox-init
  go build -o "$build_dir/sandbox-waiting-init" ./tests/testdata/sandbox-waiting-init
  go build -o "$build_dir/sandbox-root-probe" ./tests/testdata/sandbox-root-probe
  go test -c -tags='sandbox_root,profilebundle_root' -o "$build_dir/t05-tests" ./tests
  cp -- "$build_dir/t05-tests" "$build_dir/sandbox-creation-tests"
  "$build_dir/profile-bundle" build --lock profiles/python-data-v1/profile.lock.json \
    --source-cache "${PROFILE_BUNDLE_SOURCE_CACHE:-$repository_root/.cache/profile-sources}" \
    --system-call-policy profiles/python-data-v1/system-call-policy.json \
    --sandbox-init "$build_dir/sandbox-init" --output "$build_dir/python.bundle"
  # Check the pre-compilation snapshot while creating the stamp. A separate
  # check followed by a fresh snapshot could stamp changed source as built.
  python3 tests/interview-build-manifest.py write "$build_dir" --expected-sources "$build_dir/source-inputs.json"
  # A pointer is published only after every product and source has been hashed.
  printf '%s\n' "$build_dir" > "$build_dir/current"
  mv -- "$build_dir/current" "$build_pointer"
  echo "INTERVIEW_PREPARED: $build_dir"
  echo 'Next: sudo -v && bash tests/run-interview-demo-linux.sh doctor'
}

if [[ "$operation" == prepare ]]; then prepare; exit; fi
if [[ "$operation" == doctor ]]; then doctor; exit; fi
[[ -f "$build_pointer" ]] || { echo 'Run prepare first.' >&2; exit 1; }
IFS= read -r SANDBOX_TEST_BIN_DIR < "$build_pointer"
export SANDBOX_TEST_BIN_DIR
python3 tests/interview-build-manifest.py check "$SANDBOX_TEST_BIN_DIR"
if [[ "$operation" == verify || "$selection" == all || "$selection" == worker || "$selection" == capacity || "$selection" == recovery || "$selection" == expiry ]]; then
  [[ -n "${AGENT_TEST_DATABASE_URL:-}" ]] || {
    echo 'Set AGENT_TEST_DATABASE_URL to a dedicated disposable PostgreSQL 17 database; see docs/interview-demo.md.' >&2; exit 1;
  }
  echo 'Checking the dedicated PostgreSQL connection from this host (read-only)...'
  AGENT_DATABASE_URL="$AGENT_TEST_DATABASE_URL" \
    "$SANDBOX_TEST_BIN_DIR/agentctl" migrate --status --timeout=5s || {
      echo 'Database preflight failed; check host access rules, port and test credentials before rehearsing.' >&2; exit 1;
    }
fi
doctor
mkdir -p .cache/interview-runs
log_dir="$(mktemp -d "$repository_root/.cache/interview-runs/run-$(date -u +%Y%m%dT%H%M%SZ)-XXXXXX")"
printf 'Interview logs: %s\n' "$log_dir"
cp -- "$SANDBOX_TEST_BIN_DIR/manifest.json" "$log_dir/build-manifest.json"
{
  printf 'operation=%s selection=%s\n' "$operation" "$selection"
  uname -sr
  GOTOOLCHAIN=local go version
} > "$log_dir/environment.log"
trap 'status=$?; if (( status != 0 )); then printf "FAILED exit=%s; inspect %s\n" "$status" "$log_dir" >&2; fi' EXIT
run_logged() { local name="$1"; shift; "$@" 2>&1 | tee "$log_dir/$name.log"; }
export GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local GOWORK=off GOFLAGS=
if [[ "$operation" == verify ]]; then
  [[ "$EUID" != 0 ]] || { echo 'Run verify as an ordinary user; its kernel runners elevate separately.' >&2; exit 1; }
  run_logged check make check
  run_logged migrations make test-migrations
  run_logged worker-api make test-worker-api
  run_logged runtime bash tests/run-runtime-lab-demo-linux.sh
  run_logged sandbox bash tests/run-sandbox-acceptance-linux.sh
  run_logged worker bash tests/run-worker-execution-linux.sh
  echo 'INTERVIEW_VERIFY_PASS' | tee "$log_dir/result.log"
  exit
fi
case "$selection" in
  all)
    run_logged runtime bash tests/run-runtime-lab-demo-linux.sh
    run_logged sandbox bash tests/run-sandbox-demo-linux.sh all
    run_logged worker bash tests/run-worker-demo-linux.sh all ;;
  runtime) run_logged runtime bash tests/run-runtime-lab-demo-linux.sh ;;
  files|lifecycle|security|resources) run_logged "$selection" bash tests/run-sandbox-demo-linux.sh "$selection" ;;
  worker) run_logged worker bash tests/run-worker-demo-linux.sh normal ;;
  capacity|recovery|expiry) run_logged "$selection" bash tests/run-worker-demo-linux.sh "$selection" ;;
esac
echo "INTERVIEW_DEMO_PASS selection=$selection" | tee "$log_dir/result.log"
