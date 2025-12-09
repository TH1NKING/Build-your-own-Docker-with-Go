#!/usr/bin/env bash
set -euo pipefail

ROOTFS="$HOME/mydocker/rootfs"

if [ ! -d "$ROOTFS" ]; then
	echo "rootfs not exists: $ROOTFS"
	exit 1
fi

cleanup_mount(){
	local MP="$1"
	if mountpoint -q "$MP"; then
		echo "umount $MP"
		sudo umount "$MP"
	else
		echo "$MP is not the mountpoint, skip"
	fi
}

cleanup_mount "$ROOTFS/proc"
cleanup_mount "$ROOTFS/sys"
cleanup_mount "$ROOTFS/dev"

echo "umount finished"
