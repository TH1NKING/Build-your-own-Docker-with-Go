#!/usr/bin/env bash
set -euo pipefail

SUBNET="${SUBNET:-10.200.1.0/24}"
WAN_IF="${WAN_IF:-$(ip route | awk '/default/ {print $5; exit}')}"

if [[ -z "$WAN_IF" ]]; then
  echo "ERROR: cannot determine WAN_IF from default route" >&2
  exit 1
fi

echo "Using SUBNET=$SUBNET WAN_IF=$WAN_IF"

# 1) enable forwarding (runtime)
sysctl -w net.ipv4.ip_forward=1 >/dev/null

# 2) iptables: add rules only if missing (idempotent)
iptables -t nat -C POSTROUTING -s "$SUBNET" -o "$WAN_IF" -j MASQUERADE 2>/dev/null \
  || iptables -t nat -A POSTROUTING -s "$SUBNET" -o "$WAN_IF" -j MASQUERADE

iptables -C FORWARD -s "$SUBNET" -o "$WAN_IF" -j ACCEPT 2>/dev/null \
  || iptables -A FORWARD -s "$SUBNET" -o "$WAN_IF" -j ACCEPT

iptables -C FORWARD -d "$SUBNET" -i "$WAN_IF" -m state --state ESTABLISHED,RELATED -j ACCEPT 2>/dev/null \
  || iptables -A FORWARD -d "$SUBNET" -i "$WAN_IF" -m state --state ESTABLISHED,RELATED -j ACCEPT

echo "OK: forwarding+NAT ready"
