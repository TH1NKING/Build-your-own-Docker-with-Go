#!/usr/bin/env bash
set -euo pipefail

ROOTFS="${ROOTFS:-$HOME/mydocker/rootfs}"

if [[ ! -d "$ROOTFS" ]]; then
  echo "rootfs not exists: $ROOTFS"
  echo "please prepare rootfs before running this script"
  exit 1
fi

# require root once
if [[ ${EUID:-$(id -u)} -ne 0 ]]; then
  exec sudo -E "$0" "$@"
fi

mkdir -p "$ROOTFS/dev" "$ROOTFS/sys" "$ROOTFS/proc"

unshare --mount --uts --ipc --net --pid --fork bash -ceu '
  ROOTFS="$1"

  # avoid mount propagation surprises
  mount --make-rprivate /

  # mounts live ONLY inside this mount namespace
  if ! mountpoint -q "$ROOTFS/dev"; then
    mount --bind /dev "$ROOTFS/dev"
  fi

  if ! mountpoint -q "$ROOTFS/sys"; then
    mount -t sysfs sysfs "$ROOTFS/sys"
  fi

  if ! mountpoint -q "$ROOTFS/proc"; then
    mount -t proc proc "$ROOTFS/proc"
  fi

  # UTS namespace: set a distinct hostname
  hostname mini-container

  exec chroot "$ROOTFS" /bin/sh -c "
    echo \"Welcome to new mini-container!\"
    echo \"PID: \$\$\"
    echo \"Hostname: \$(hostname)\"
    echo \"type exit to quit\"
    exec /bin/sh
  "
' _ "$ROOTFS"
