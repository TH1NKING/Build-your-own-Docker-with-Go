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
    # Minimal /dev: tmpfs + essential nodes + devpts
    mount -t tmpfs -o mode=755,nosuid tmpfs "$ROOTFS/dev"

    # essential device nodes (character devices)
    mknod -m 666 "$ROOTFS/dev/null"    c 1 3
    mknod -m 666 "$ROOTFS/dev/zero"    c 1 5
    mknod -m 666 "$ROOTFS/dev/random"  c 1 8
    mknod -m 666 "$ROOTFS/dev/urandom" c 1 9
    mknod -m 666 "$ROOTFS/dev/tty"     c 5 0
    mknod -m 600 "$ROOTFS/dev/console" c 5 1

    mkdir -p "$ROOTFS/dev/pts" "$ROOTFS/dev/shm"
    mount -t devpts -o newinstance,ptmxmode=0666,mode=0620,gid=5 devpts "$ROOTFS/dev/pts"
    ln -sf /dev/pts/ptmx "$ROOTFS/dev/ptmx"
    mount -t tmpfs -o mode=1777,nosuid tmpfs "$ROOTFS/dev/shm"
  fi


  if ! mountpoint -q "$ROOTFS/sys"; then
    mount -t sysfs sysfs "$ROOTFS/sys"
  fi

  if ! mountpoint -q "$ROOTFS/proc"; then
    mount -t proc proc "$ROOTFS/proc"
  fi

  # UTS namespace: set a distinct hostname
  hostname mini-container
	# NET namespace: bring up loopback (minimum viable)
  if command -v ip >/dev/null 2>&1; then
    ip link set lo up

    # ensure 127.0.0.1/8 exists (usually already there, keep it safe)
    ip -4 addr show dev lo | grep -q "127\.0\.0\.1/8" || ip addr add 127.0.0.1/8 dev lo || true

  else
    echo "WARN: 'ip' not found on host; cannot set lo up automatically" >&2
  fi


  exec chroot "$ROOTFS" /bin/sh -c "
    echo \"Welcome to new mini-container!\"
    echo \"PID: \$\$\"
    echo \"Hostname: \$(hostname)\"
    echo \"type exit to quit\"
    exec /bin/sh
  "
' _ "$ROOTFS"
