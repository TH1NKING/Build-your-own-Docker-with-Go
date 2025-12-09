#!/usr/bin/env bash
set -euo pipefail

ROOTFS="$HOME/mydocker/rootfs"

if [ ! -d "$ROOTFS" ]; then
	echo "rootfs not exists:$ROOTFS"
	echo "please preparing rootfs following the previous steps before running this script"
	exit 1
fi

sudo mkdir -p "$ROOTFS/dev" "$ROOTFS/sys" "$ROOTFS/proc"

if ! mountpoint -q "$ROOTFS/dev"; then
	echo "mount --bind /dev -> $ROOTFS/dev"
	sudo mount --bind /dev "$ROOTFS/dev"
else
	echo "$ROOTFS/dev is already been mountpoint, skip"
fi

if ! mountpoint -q "$ROOTFS/sys"; then 
	echo "mount -t sysfs sys -> $ROOTFS/sys"
	sudo mount -t sysfs sys "$ROOTFS/sys"

else 
	echo "$ROOTFS/sys is already been mountpoint, skip"
fi

if mountpoint -q "$ROOTFS/proc"; then
	echo "$ROOTFS/proc already mounted, should umount"
	sudo umount "$ROOTFS/proc"
fi

sudo unshare --mount --uts --ipc --net --pid --fork /usr/sbin/chroot "$ROOTFS" /bin/sh -c '
	# mount -t proc proc /proc
	echo "Welcome to new mini-container!"
	echo "PID:$$"
	echo "Hostname:$(hostname)"
	echo "type exit to quit"
	exec /bin/sh
'
