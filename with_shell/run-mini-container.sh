#!/usr/bin/env bash
set -euo pipefail

BR_IF="${BR_IF:-br0}"
SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
BASE_IMAGE="${BASE_IMAGE:-$SCRIPT_DIR/rootfs}"
BASE_IMAGE="$(readlink -f "$BASE_IMAGE")"
CONTAINERS_DIR="$SCRIPT_DIR/containers"
mkdir -p "$CONTAINERS_DIR"


if [[ ${EUID:-$(id -u)} -ne 0 ]]; then
    exec sudo -E BASE_IMAGE="$BASE_IMAGE" "$0" "$@"
fi


MEM_LIMIT=""
CPU_LIMIT=""
USE_ID=""

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
            break
            ;;
    esac
done


if [[ $# -lt 1 ]]; then
    echo "Error: Missing CONTAINER_IP"
    echo "Usage: $0 [OPTIONS] <CONTAINER_IP> [COMMAND...]"
    exit 1
fi

CONT_IP="$1"
shift
CMD=${*:-"/bin/sh"}

HOST_IP="10.200.1.1/24"

export WAIT_STEP=0.05
export WAIT_MAX_ITERS=200

die() { echo "ERROR: $*" >&2; exit 1; }

need_root() {
  if [[ ${EUID:-$(id -u)} -ne 0 ]]; then
    exec sudo -E BASE_IMAGE="$BASE_IMAGE" "$0" "$CONT_IP" "$CMD"
  fi
}

child_logic() {
  local rootfs="$1"
  local pidfile="$2"
  local gofile="$3"
  local cont_ip="$4"
  local host_gw="$5"
  local cmd="$6"
  local cont_id="$7"

  echo "$$" > "$pidfile"
  for ((i=0; i<WAIT_MAX_ITERS; i++)); do
    [[ -e "$gofile" ]] && break
    sleep "$WAIT_STEP"
  done
  [[ -e "$gofile" ]] || { echo "Child: timeout waiting for network"; exit 1; }

  mount --make-rprivate /

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

  if [[ "$cont_ip" != *"/"* ]]; then
      cont_ip="${cont_ip}/24"
  fi

  ip addr add "$cont_ip" dev eth0
  ip route replace default via "$host_gw" dev eth0
  ip addr add 127.0.0.1/8 dev lo || true
  ip link set lo up

  hostname "container-$cont_id"
  
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

setup_host_worker() {
  local pidfile="$1"
  local gofile="$2"
  local cont_id="$3"
  local mem_limit="$4"
  local cpu_limit="$5"

  for ((i=0; i<WAIT_MAX_ITERS; i++)); do
    [[ -s "$pidfile" ]] && break
    sleep "$WAIT_STEP"
  done
  
  if [[ ! -s "$pidfile" ]]; then echo "Worker: No PID found"; return 1; fi
  local target_pid
  target_pid="$(cat "$pidfile")"

  local cg_base="/sys/fs/cgroup/mydocker"
  local cg_dir="$cg_base/$cont_id"
  
  mkdir -p "$cg_base"
  echo "+cpu +memory" > /sys/fs/cgroup/cgroup.subtree_control 2>/dev/null || true
  echo "+cpu +memory" > "$cg_base/cgroup.subtree_control" 2>/dev/null || true

  mkdir -p "$cg_dir"

  if [[ -n "$mem_limit" ]]; then
      echo "Worker: Limiting Memory to $mem_limit (No Swap)"
      echo "$mem_limit" > "$cg_dir/memory.max"
      echo "0" > "$cg_dir/memory.swap.max" 2>/dev/null || true
  else
      echo "Worker: Memory Unlimited"
  fi

  if [[ -n "$cpu_limit" ]]; then
      echo "Worker: Limiting CPU to $cpu_limit%"
      local quota=$((cpu_limit * 1000))
      echo "$quota 100000" > "$cg_dir/cpu.max"
  else
      echo "Worker: CPU Unlimited"
  fi
  
  echo "$target_pid" > "$cg_dir/cgroup.procs"

  
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
}

[[ -d "$BASE_IMAGE" ]] || die "Base image missing at $BASE_IMAGE"

need_root

if [[ -n "${USE_ID:-}" ]]; then
    CONT_ID="$USE_ID"
    echo "=== Resuming Existing Container ID: $CONT_ID ==="
else
    CONT_ID="$(date +%s%N | sha256sum | head -c 8)"
    echo "=== Allocating New Container ID: $CONT_ID ==="
fi

CON_DIR="$CONTAINERS_DIR/$CONT_ID"
UPPER_DIR="$CON_DIR/upper"
WORK_DIR="$CON_DIR/work"
MERGED_DIR="$CON_DIR/merged"

mkdir -p "$UPPER_DIR" "$WORK_DIR" "$MERGED_DIR"

if mountpoint -q "$MERGED_DIR"; then
    echo "OverlayFS is already mounted. Skipping."
else
    echo "Mounting OverlayFS..."
    mount -t overlay overlay -o lowerdir="$BASE_IMAGE",upperdir="$UPPER_DIR",workdir="$WORK_DIR" "$MERGED_DIR"
fi

PIDFILE="$CON_DIR/minict.pid"
GOFILE="$CON_DIR/minict.go"

rm -f "$PIDFILE" "$GOFILE"

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
  
  if mountpoint -q "$MERGED_DIR"; then
      echo "Unmounting OverlayFS..."
      umount "$MERGED_DIR"
  fi
  
  if [[ -n "${CONT_ID:-}" && -d "/sys/fs/cgroup/mydocker/$CONT_ID" ]]; then
      echo "Removing Cgroup..."
      rmdir "/sys/fs/cgroup/mydocker/$CONT_ID" 2>/dev/null || true
  fi

  echo "Container data kept in: ${CON_DIR:-unknown}"
}
trap cleanup EXIT

echo "Starting container process in $MERGED_DIR..."

HOST_GW="${HOST_IP%%/*}"

unshare --mount --uts --ipc --net --fork \
  bash -c 'child_logic "$@"' -- \
  "$MERGED_DIR" "$PIDFILE" "$GOFILE" "$CONT_IP" "$HOST_GW" "$CMD" "$CONT_ID"