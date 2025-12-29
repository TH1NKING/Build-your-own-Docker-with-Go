#!/usr/bin/env bash
set -euo pipefail

ROOTFS="${ROOTFS:-$HOME/mydocker/rootfs}"

if [[ ! -d "$ROOTFS" ]]; then
  echo "rootfs not exists: $ROOTFS"
  exit 1
fi

if [[ ${EUID:-$(id -u)} -ne 0 ]]; then
  exec sudo -E "$0" "$@"
fi

cleanup_mount() {
  local mp="$1"
  if mountpoint -q "$mp"; then
    echo "umount $mp"
    umount "$mp" || {
      echo "umount failed (busy?): $mp"
      echo "try lazy umount: $mp"
      umount -l "$mp"
    }
  else
    echo "$mp is not a mountpoint, skip"
  fi
}

# reverse order of mounting
cleanup_mount "$ROOTFS/proc"
cleanup_mount "$ROOTFS/sys"
cleanup_mount "$ROOTFS/dev"

echo "umount finished"
