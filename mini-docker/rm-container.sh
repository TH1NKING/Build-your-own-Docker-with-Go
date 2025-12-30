#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
CONTAINERS_DIR="$SCRIPT_DIR/containers"

if [[ $# -lt 1 ]]; then
    echo "Usage: $0 <CONTAINER_ID> [CONTAINER_ID...]"
    echo "Example: $0 e4042f47"
    ls -1 "$CONTAINERS_DIR" 2>/dev/null | head -n 5 | awk '{print "Found: " $1}'
    exit 1
fi

# 检查是否是 Root，如果不是则自动 sudo
if [[ ${EUID:-$(id -u)} -ne 0 ]]; then
    exec sudo "$0" "$@"
fi

for CONT_ID in "$@"; do
    TARGET_DIR="$CONTAINERS_DIR/$CONT_ID"

    if [[ ! -d "$TARGET_DIR" ]]; then
        echo "Warning: Container '$CONT_ID' not found at $TARGET_DIR"
        continue
    fi

    echo "Removing container: $CONT_ID ..."

    # 1. 核心步骤：安全卸载 OverlayFS
    MERGED_DIR="$TARGET_DIR/merged"
    
    # 尝试懒卸载所有的挂载点 (包括 proc, sys, dev 等可能残留的)
    if mountpoint -q "$MERGED_DIR/dev/pts"; then umount -l "$MERGED_DIR/dev/pts"; fi
    if mountpoint -q "$MERGED_DIR/dev"; then umount -l "$MERGED_DIR/dev"; fi
    if mountpoint -q "$MERGED_DIR/proc"; then umount -l "$MERGED_DIR/proc"; fi
    if mountpoint -q "$MERGED_DIR/sys"; then umount -l "$MERGED_DIR/sys"; fi

    # 卸载根文件系统
    if mountpoint -q "$MERGED_DIR"; then
        echo "  - Unmounting rootfs..."
        umount "$MERGED_DIR" || umount -l "$MERGED_DIR"
    fi

    # 2. 删除目录
    echo "  - Deleting data..."
    rm -rf "$TARGET_DIR"
    
    echo "Done: $CONT_ID removed."
done