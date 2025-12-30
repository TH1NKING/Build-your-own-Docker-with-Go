#!/usr/bin/env bash
set -euo pipefail

BR_IF="${BR_IF:-br0}"
SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
BASE_IMAGE="${BASE_IMAGE:-$SCRIPT_DIR/rootfs}"
BASE_IMAGE="$(readlink -f "$BASE_IMAGE")"
CONTAINERS_DIR="$SCRIPT_DIR/containers"
mkdir -p "$CONTAINERS_DIR"

# ---------- 1. 优先检查 Root 权限 ----------
# 必须最先做！而且必须传递 "$@" (所有参数)
if [[ ${EUID:-$(id -u)} -ne 0 ]]; then
    # 使用 "$@" 保留所有原始参数 (包括 -m, -c 等)
    exec sudo -E BASE_IMAGE="$BASE_IMAGE" "$0" "$@"
fi

# ---------- 2. 参数解析 (现在是在 Root 环境下) ----------

# 初始化默认值
MEM_LIMIT=""
CPU_LIMIT=""
USE_ID=""

# 解析循环
while [[ $# -gt 0 ]]; do
    case $1 in
        -m|--memory)
            if [[ -z "${2:-}" ]]; then
                echo "Error: Option $1 requires an argument." >&2; exit 1
            fi
            MEM_LIMIT="$2"
            shift 2
            ;;
        -c|--cpu)
            if [[ -z "${2:-}" ]]; then
                echo "Error: Option $1 requires an argument." >&2; exit 1
            fi
            CPU_LIMIT="$2"
            shift 2
            ;;
        --id)
            if [[ -z "${2:-}" ]]; then
                echo "Error: Option $1 requires an argument." >&2; exit 1
            fi
            USE_ID="$2"
            shift 2
            ;;
        -h|--help)
            echo "Usage: $0 [OPTIONS] <CONTAINER_IP> [COMMAND...]"
            echo "Options:"
            echo "  -m, --memory <limit>  Memory limit (e.g., 100M)"
            echo "  -c, --cpu <percent>   CPU limit percent (e.g., 20)"
            echo "  --id <id>             Resume existing container ID"
            exit 0
            ;;
        -*)
            echo "Error: Unknown option $1" >&2
            exit 1
            ;;
        *)
            # 遇到不以 - 开头的参数，假设是 IP，停止解析选项
            break
            ;;
    esac
done

# ---------- 3. 提取必要参数 ----------

if [[ $# -lt 1 ]]; then
    echo "Error: Missing CONTAINER_IP"
    echo "Usage: $0 [OPTIONS] <CONTAINER_IP> [COMMAND...]"
    exit 1
fi

CONT_IP="$1"
shift
CMD=${*:-"/bin/sh"} # 剩余的所有参数作为命令

HOST_IP="10.200.1.1/24"

export WAIT_STEP=0.05
export WAIT_MAX_ITERS=200

# ---------- helpers ----------
die() { echo "ERROR: $*" >&2; exit 1; }

need_root() {
  if [[ ${EUID:-$(id -u)} -ne 0 ]]; then
    exec sudo -E BASE_IMAGE="$BASE_IMAGE" "$0" "$CONT_IP" "$CMD"
  fi
}

# ---------- child logic ----------
child_logic() {
  local rootfs="$1"
  local pidfile="$2"
  local gofile="$3"
  local cont_ip="$4"
  local host_gw="$5"
  local cmd="$6"
  local cont_id="$7"

  # 1. 握手
  echo "$$" > "$pidfile"
  for ((i=0; i<WAIT_MAX_ITERS; i++)); do
    [[ -e "$gofile" ]] && break
    sleep "$WAIT_STEP"
  done
  [[ -e "$gofile" ]] || { echo "Child: timeout waiting for network"; exit 1; }

  # 2. 挂载
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

  # 4. 配置网络
  local veth_found=""
  for ((j=0; j<50; j++)); do
      veth_found=$(ip -o link show | awk -F': ' '{print $2}' | cut -d'@' -f1 | grep '^vethc-' | head -n1) || true
      if [[ -n "$veth_found" ]]; then break; fi
      sleep 0.1
  done

  if [[ -z "$veth_found" ]]; then
    echo "ERROR: Child timeout finding veth!" >&2; exit 1
  fi

  ip link set "$veth_found" name eth0
  ip link set eth0 up
  
  # [修复点] 自动补全子网掩码
  if [[ "$cont_ip" != *"/"* ]]; then
      cont_ip="${cont_ip}/24"
  fi

  ip addr add "$cont_ip" dev eth0
  ip route replace default via "$host_gw" dev eth0
  ip addr add 127.0.0.1/8 dev lo || true
  ip link set lo up

  hostname "container-$cont_id"
  
  # 5. 进入隔离环境
  exec unshare --pid --fork bash -ceu "
    rootfs=\"\$1\"
    cmd=\"\$2\"
    
    if mountpoint -q \"\$rootfs/proc\"; then umount -l \"\$rootfs/proc\"; fi
    mount -t proc proc \"\$rootfs/proc\"
    if ! mountpoint -q \"\$rootfs/sys\"; then mount -t sysfs sysfs \"\$rootfs/sys\"; fi

    exec chroot \"\$rootfs\" /bin/sh -c \"
      export PS1='\\u@\\h:/# '
      echo '--- Container Ready ($cont_id) ---'
      exec \$cmd
    \"
  " -- "$rootfs" "$cmd"
}
export -f child_logic

# ---------- host worker (network + cgroups) ----------
setup_host_worker() {
  local pidfile="$1"
  local gofile="$2"
  local cont_id="$3"
  local mem_limit="$4"  # [NEW] 接收内存参数
  local cpu_limit="$5"  # [NEW] 接收CPU参数 (百分比整数, e.g. 20 代表 20%)

  # 等待 PID
  for ((i=0; i<WAIT_MAX_ITERS; i++)); do
    [[ -s "$pidfile" ]] && break
    sleep "$WAIT_STEP"
  done
  
  if [[ ! -s "$pidfile" ]]; then echo "Worker: No PID found"; return 1; fi
  local target_pid
  target_pid="$(cat "$pidfile")"

  # Cgroups v2 初始化
  local cg_base="/sys/fs/cgroup/mydocker"
  local cg_dir="$cg_base/$cont_id"
  
  # 开启父目录控制权
  mkdir -p "$cg_base"
  echo "+cpu +memory" > /sys/fs/cgroup/cgroup.subtree_control 2>/dev/null || true
  echo "+cpu +memory" > "$cg_base/cgroup.subtree_control" 2>/dev/null || true

  mkdir -p "$cg_dir"
  
  # [NEW] 动态限制逻辑
  if [[ -n "$mem_limit" ]]; then
      echo "Worker: Limiting Memory to $mem_limit (No Swap)"
      echo "$mem_limit" > "$cg_dir/memory.max"
      echo "0" > "$cg_dir/memory.swap.max" 2>/dev/null || true
  else
      echo "Worker: Memory Unlimited"
  fi

  if [[ -n "$cpu_limit" ]]; then
      echo "Worker: Limiting CPU to $cpu_limit%"
      # 简单的算法：周期 100000us (100ms)
      # 配额 = limit * 1000
      # 例如: 20% -> 20 * 1000 = 20000us
      local quota=$((cpu_limit * 1000))
      echo "$quota 100000" > "$cg_dir/cpu.max"
  else
      echo "Worker: CPU Unlimited"
  fi
  
  # 加入进程
  echo "$target_pid" > "$cg_dir/cgroup.procs"

  # ... (网络配置部分保持完全不变，此处省略以节省空间) ...
  # 请保留原有的 wait netns 和 ip link/addr 逻辑
  
  # 网络逻辑开始 --->
  for ((i=0; i<WAIT_MAX_ITERS; i++)); do
    [[ -e "/proc/$target_pid/ns/net" ]] && break
    sleep "$WAIT_STEP"
  done
  [[ -e "/proc/$target_pid/ns/net" ]] || return 1

  ip link show "$BR_IF" >/dev/null 2>&1 || ip link add "$BR_IF" type bridge
  ip link set "$BR_IF" up
  ip addr add "$HOST_IP" dev "$BR_IF" 2>/dev/null || true

  local veth_host="vethh-$cont_id"
  local veth_cont="vethc-$cont_id"

  ip link del "$veth_host" 2>/dev/null || true
  ip link add "$veth_host" type veth peer name "$veth_cont"
  ip link set "$veth_host" master "$BR_IF"
  ip link set "$veth_host" up
  ip link set "$veth_cont" netns "$target_pid"
  
  : > "$gofile"
  # <--- 网络逻辑结束
}

# ---------- main ----------
[[ -d "$BASE_IMAGE" ]] || die "Base image missing at $BASE_IMAGE"

need_root

# [修改 1] ID 处理逻辑：支持复用旧 ID
if [[ -n "${USE_ID:-}" ]]; then
    CONT_ID="$USE_ID"
    echo "=== Resuming Existing Container ID: $CONT_ID ==="
else
    CONT_ID="$(date +%s%N | sha256sum | head -c 8)"
    echo "=== Allocating New Container ID: $CONT_ID ==="
fi

# 定义目录
CON_DIR="$CONTAINERS_DIR/$CONT_ID"
UPPER_DIR="$CON_DIR/upper"
WORK_DIR="$CON_DIR/work"
MERGED_DIR="$CON_DIR/merged"

# 确保目录存在 (mkdir -p 是幂等的，目录已存在也不会报错)
mkdir -p "$UPPER_DIR" "$WORK_DIR" "$MERGED_DIR"

# [修改 2] 智能挂载：只有未挂载时才执行 mount
# 这样即使你手动挂载过，或者脚本上次非正常退出没卸载，这里也不会报错
if mountpoint -q "$MERGED_DIR"; then
    echo "OverlayFS is already mounted. Skipping."
else
    echo "Mounting OverlayFS..."
    mount -t overlay overlay -o lowerdir="$BASE_IMAGE",upperdir="$UPPER_DIR",workdir="$WORK_DIR" "$MERGED_DIR"
fi

PIDFILE="$CON_DIR/minict.pid"
GOFILE="$CON_DIR/minict.go"
# 清理旧的 PID 文件，防止残留导致误判
rm -f "$PIDFILE" "$GOFILE"

# 启动网络工 (传入 CONT_ID)
setup_host_worker "$PIDFILE" "$GOFILE" "$CONT_ID" "$MEM_LIMIT" "$CPU_LIMIT" &
WORKER_PID=$!

cleanup() {
  echo ""
  echo "Stopping container ${CONT_ID:-unknown}..."
  kill "$WORKER_PID" 2>/dev/null || true
  
  if [[ -n "${CONT_ID:-}" ]]; then
      ip link del "vethh-$CONT_ID" 2>/dev/null || true
  fi

  sleep 0.5
  
  # ... (原本的 mountpoint 清理逻辑不变) ...
  
  if mountpoint -q "$MERGED_DIR"; then
      echo "Unmounting OverlayFS..."
      umount "$MERGED_DIR"
  fi
  
  # [NEW] 清理 Cgroups 目录
  # rmdir 只能删除空目录，如果容器进程已死，这里应该成功
  if [[ -n "${CONT_ID:-}" && -d "/sys/fs/cgroup/mydocker/$CONT_ID" ]]; then
      echo "Removing Cgroup..."
      rmdir "/sys/fs/cgroup/mydocker/$CONT_ID" 2>/dev/null || true
  fi

  echo "Container data kept in: ${CON_DIR:-unknown}"
}
trap cleanup EXIT

echo "Starting container process in $MERGED_DIR..."

HOST_GW="${HOST_IP%%/*}"

# 启动容器
unshare --mount --uts --ipc --net --fork \
  bash -c 'child_logic "$@"' -- \
  "$MERGED_DIR" "$PIDFILE" "$GOFILE" "$CONT_IP" "$HOST_GW" "$CMD" "$CONT_ID"