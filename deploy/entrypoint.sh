#!/usr/bin/env bash
# Host-container entrypoint: put the microVM on the guest L2, then start the
# migration agent.
#
# Each host container has two interfaces:
#   - one on mignet  (172.30.0.0/24) — the guest's data-plane L2
#   - one on xfernet (172.31.0.0/24) — a dedicated migration network
#
# We build a bridge br0 for the guest L2: the mignet interface's identity
# moves onto br0, and tap0 (the guest's NIC backend) joins as a second port.
# The guest then holds its own IP directly on mignet — L2-adjacent to the
# client and the other host — so its MAC+IP (and live TCP connections) move
# between hosts with nothing but a gratuitous ARP.
#
# The xfernet interface is left alone. All bulk agent→agent migration traffic
# (the base image, pre-copy rounds) travels over it, so a multi-hundred-MB
# transfer never competes with the guest's own packets on the guest L2 — the
# same reason production clusters put live migration on a dedicated network.
set -euo pipefail

# Identify interfaces by subnet, not by kernel name (eth0/eth1 ordering across
# two docker networks is not guaranteed).
iface_for_subnet() {
  ip -4 -o addr show | awk -v pfx="$1" '$4 ~ ("^" pfx) {print $2; exit}'
}
GUEST_IF="$(iface_for_subnet 172.30.0.)"
: "${GUEST_IF:?no interface on the guest network 172.30.0.0/24}"

# `docker compose restart` re-runs this script in a network namespace that
# already has br0/tap0 configured. Detect that and skip setup so restart is
# idempotent (re-adding br0 would error out under `set -e`).
if ! ip link show br0 >/dev/null 2>&1; then
  CIDR="$(ip -4 -o addr show "$GUEST_IF" | awk '{print $4}')"
  GW="$(ip route | awk '/^default/ {print $3; exit}')"
  MAC="$(cat "/sys/class/net/$GUEST_IF/address")"

  ip link add br0 type bridge
  ip link set br0 address "$MAC" # keep the MAC docker allocated to this container
  ip tuntap add dev tap0 mode tap

  ip addr flush dev "$GUEST_IF"
  ip link set "$GUEST_IF" master br0
  ip link set tap0 master br0
  ip link set br0 up
  ip link set "$GUEST_IF" up
  ip link set tap0 up

  ip addr add "$CIDR" dev br0
  # Default route via the guest network's gateway (docker only runs a gateway
  # on the network that carried the original default route).
  ip route add default via "$GW" 2>/dev/null || true
fi

# GARP goes out the enslaved uplink directly, bypassing br0, so our own bridge
# never mislearns the guest MAC as local.
exec /opt/fcmig/migrated \
  -name "${AGENT_NAME:?AGENT_NAME must be set}" \
  -garp-iface "$GUEST_IF" \
  "$@"
