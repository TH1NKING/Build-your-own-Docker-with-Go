#!/usr/bin/env bash
# Sourced by the two privileged acceptance runners, inside their disposable
# namespace. Only a fresh child is owned; never alter or remove the parent.

prepare_sandbox_test_cgroup() {
  local prefix="$1" cgroup_parent="${2:-}" delegated controller
  if [[ -z "$cgroup_parent" ]]; then
    read -r cgroup_parent < <(findmnt --first-only --noheadings --raw --types cgroup2 --output TARGET) || {
      echo "cgroup v2 is required; set SANDBOX_TEST_CGROUP_PARENT to a delegated parent" >&2
      return 1
    }
  fi
  if [[ "$cgroup_parent" != /* || ! -d "$cgroup_parent" ]] ||
     [[ "$(stat -f -c %t "$cgroup_parent")" != 63677270 ]]; then
    echo "Sandbox test cgroup parent must be an absolute cgroup v2 directory: $cgroup_parent" >&2
    return 1
  fi
  cgroup_parent="$(cd -- "$cgroup_parent" && pwd -P)"
  delegated=" $(<"$cgroup_parent/cgroup.subtree_control") "
  for controller in cpu memory pids; do
    if [[ "$delegated" != *" $controller "* ]]; then
      echo "Sandbox test cgroup parent has not delegated $controller: $cgroup_parent; set SANDBOX_TEST_CGROUP_PARENT to a parent with cpu, memory and pids delegated" >&2
      return 1
    fi
  done
  SANDBOX_CGROUP_ROOT="$(mktemp -d "$cgroup_parent/$prefix-XXXXXX")"
  export SANDBOX_CGROUP_ROOT
  trap cleanup_sandbox_test_cgroup EXIT
}

cleanup_sandbox_test_cgroup() {
  local test_status=$?
  if ! rmdir -- "$SANDBOX_CGROUP_ROOT"; then
    echo "Sandbox acceptance leaked cgroup state: $SANDBOX_CGROUP_ROOT" >&2
    exit 1
  fi
  exit "$test_status"
}
