#!/usr/bin/env bash
set -euo pipefail

# ---------- config ----------
SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
ROOTFS="${ROOTFS:-$SCRIPT_DIR/rootfs}"
ROOTFS="$(readlink -f "$ROOTFS")"

SUBNET_CIDR="10.200.1.0/24"
HOST_IP="10.200.1.1/24"
CONT_IP="10.200.1.2/24"

export WAIT_STEP=0.05
export WAIT_MAX_ITERS=200

# ---------- helpers ----------
die() { echo "ERROR: $*" >&2; exit 1; }

need_root() {
  if [[ ${EUID:-$(id -u)} -ne 0 ]]; then
    exec sudo -E ROOTFS="$ROOTFS" "$0" "$@"
  fi
}

# ---------- child logic ----------
child_logic() {
  local rootfs="$1"
  local pidfile="$2"
  local gofile="$3"
  local cont_ip="$4"
  local host_gw="$5"


  # 1. 握手阶段
  echo "$$" > "$pidfile"
  for ((i=0; i<WAIT_MAX_ITERS; i++)); do
    [[ -e "$gofile" ]] && break
    sleep "$WAIT_STEP"
  done
  [[ -e "$gofile" ]] || { echo "Child: timeout waiting for network"; exit 1; }

  # 2. 挂载命名空间
  mount --make-rprivate /

  # 3. 修复 /dev
  if ! mountpoint -q "$rootfs/dev"; then
    mount -t tmpfs -o mode=755,nosuid tmpfs "$rootfs/dev"
  fi
  if [[ ! -c "$rootfs/dev/null" ]]; then
      mknod -m 666 "$rootfs/dev/null"    c 1 3
      mknod -m 666 "$rootfs/dev/zero"    c 1 5
      mknod -m 666 "$rootfs/dev/random"  c 1 8
      mknod -m 666 "$rootfs/dev/urandom" c 1 9
      mknod -m 666 "$rootfs/dev/tty"     c 5 0
      mknod -m 600 "$rootfs/dev/console" c 5 1
      mkdir -p "$rootfs/dev/pts" "$rootfs/dev/shm"
  fi
  if ! mountpoint -q "$rootfs/dev/pts"; then
      mount -t devpts -o newinstance,ptmxmode=0666,mode=0620,gid=5 devpts "$rootfs/dev/pts"
      ln -sf /dev/pts/ptmx "$rootfs/dev/ptmx"
  fi

  # 4. 配置网络 (已修复 BUG)
  local veth_found=""
  
  # 增加循环重试，防止父进程移动网卡稍微慢一点点导致找不到
  for ((j=0; j<50; j++)); do
      # 获取所有网卡名 -> 过滤出vethc开头的 -> 使用 cut 去掉 @ 后面的内容
      veth_found=$(ip -o link show | awk -F': ' '{print $2}' | cut -d'@' -f1 | grep '^vethc-' | head -n1) || true
      
      if [[ -n "$veth_found" ]]; then
          break
      fi
      sleep 0.1
  done

  if [[ -z "$veth_found" ]]; then
    echo "ERROR: Child could not find veth interface (timeout)!" >&2
    ip link show >&2
    exit 1
  fi

  # 现在 veth_found 是干净的名字 (如 vethc-12345)，改名不会报错了
  ip link set "$veth_found" name eth0
  ip link set eth0 up
  ip addr add "$cont_ip" dev eth0
  ip route replace default via "$host_gw" dev eth0


  ip addr add 127.0.0.1/8 dev lo || true
  ip link set lo up

  hostname mini-container
  
  # 5. 进入隔离环境
  exec unshare --pid --fork bash -ceu '
    rootfs="$1"
    if mountpoint -q "$rootfs/proc"; then umount -l "$rootfs/proc"; fi
    mount -t proc proc "$rootfs/proc"
    if ! mountpoint -q "$rootfs/sys"; then mount -t sysfs sysfs "$rootfs/sys"; fi

    exec chroot "$rootfs" /bin/sh -c "
      export PS1=\"\u@container:/# \"
      echo \"Container Ready!\"
      echo \"Try: ping 10.200.1.1\"
      exec /bin/sh
    "
  ' -- "$rootfs"
}
export -f child_logic

# ---------- network worker ----------
setup_network_worker() {
  local pidfile="$1"
  local gofile="$2"

  # 等待 PID 文件
  for ((i=0; i<WAIT_MAX_ITERS; i++)); do
    [[ -s "$pidfile" ]] && break
    sleep "$WAIT_STEP"
  done
  
  if [[ ! -s "$pidfile" ]]; then echo "NetWorker: No PID found"; return 1; fi
  local target_pid
  target_pid="$(cat "$pidfile")"

  # 关键修复：等待目标进程的 Network Namespace 文件就绪
  # 如果这里不等，ip link set netns 可能会失败，导致网卡留在宿主机 (解决问题2 & 4)
  for ((i=0; i<WAIT_MAX_ITERS; i++)); do
    [[ -e "/proc/$target_pid/ns/net" ]] && break
    sleep "$WAIT_STEP"
  done

  if [[ ! -e "/proc/$target_pid/ns/net" ]]; then
    echo "NetWorker: /proc/$target_pid/ns/net not found. Process died?"
    return 1
  fi

  local veth_host="vethh-$target_pid"
  local veth_cont="vethc-$target_pid"

  # 创建并配置
  ip link del "$veth_host" 2>/dev/null || true
  ip link add "$veth_host" type veth peer name "$veth_cont"
  ip addr add "$HOST_IP" dev "$veth_host"
  ip link set "$veth_host" up
  
  # 移动到容器
  ip link set "$veth_cont" netns "$target_pid"
  
  # 通知子进程
  : > "$gofile"
}

# ---------- main ----------
[[ -d "$ROOTFS" ]] || die "rootfs missing"
need_root "$@"

# 清理旧的标记文件
PIDFILE="/tmp/minict.pid"
GOFILE="/tmp/minict.go"
rm -f "$PIDFILE" "$GOFILE"

# 启动网络配置工 (后台)
setup_network_worker "$PIDFILE" "$GOFILE" &
WORKER_PID=$!

# 注册退出清理
cleanup() {
  kill "$WORKER_PID" 2>/dev/null || true
  rm -f "$PIDFILE" "$GOFILE"
  # 尝试清理可能残留的宿主机端网卡
  if [[ -f "$PIDFILE" ]]; then
     local p
     p=$(cat "$PIDFILE")
     ip link del "vethh-$p" 2>/dev/null || true
  fi
}
trap cleanup EXIT

echo "Starting container..."

# 前台启动容器
HOST_GW="${HOST_IP%%/*}"
unshare --mount --uts --ipc --net --fork \
  bash -c 'child_logic "$@"' -- \
  "$ROOTFS" "$PIDFILE" "$GOFILE" "$CONT_IP" "$HOST_GW"
