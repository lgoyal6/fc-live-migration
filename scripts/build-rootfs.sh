#!/usr/bin/env bash
# Builds the minimal guest root filesystem: busybox userland + guestd (our
# static Go workload) + an init script that configures networking from the
# kernel command line. ~64 MiB ext4, boots in well under a second.
#
# Usage: build-rootfs.sh <build-dir>   (expects <build-dir>/guestd to exist)
set -euo pipefail

BUILD_DIR="${1:?usage: build-rootfs.sh <build-dir>}"
ROOTFS="$BUILD_DIR/rootfs.ext4"
GUESTD="$BUILD_DIR/guestd"
MNT="$(mktemp -d)"

[[ -f "$GUESTD" ]] || { echo "missing $GUESTD — run 'make binaries' first" >&2; exit 1; }
BUSYBOX="$(command -v busybox)"
[[ -n "$BUSYBOX" ]] || { echo "busybox-static not installed" >&2; exit 1; }

cleanup() { sudo umount "$MNT" 2>/dev/null || true; rmdir "$MNT"; }
trap cleanup EXIT

truncate -s 64M "$ROOTFS"
mkfs.ext4 -q -F "$ROOTFS"
sudo mount -o loop "$ROOTFS" "$MNT"

sudo mkdir -p "$MNT"/{bin,sbin,etc,proc,sys,dev,tmp,root,usr/bin}
sudo cp "$BUSYBOX" "$MNT/bin/busybox"
for applet in sh mount ip cat sleep ls ps dmesg reboot; do
  sudo ln -sf /bin/busybox "$MNT/bin/$applet"
done
sudo cp "$GUESTD" "$MNT/usr/bin/guestd"
sudo chmod 755 "$MNT/usr/bin/guestd"

# PID 1: mount pseudo-filesystems, bring up eth0 from guestd.* kernel args,
# then hand over to guestd. If guestd ever exits the kernel panics and
# Firecracker reboots the guest — fail loudly, not half-alive.
sudo tee "$MNT/sbin/init" >/dev/null <<'EOF'
#!/bin/busybox sh
/bin/busybox mount -t proc proc /proc
/bin/busybox mount -t sysfs sys /sys
/bin/busybox mount -t devtmpfs dev /dev 2>/dev/null

ip link set lo up

# Parse guestd.ip=<cidr> and guestd.gw=<ip> from the kernel command line.
GUEST_IP=""; GUEST_GW=""
for tok in $(cat /proc/cmdline); do
  case "$tok" in
    guestd.ip=*) GUEST_IP="${tok#guestd.ip=}" ;;
    guestd.gw=*) GUEST_GW="${tok#guestd.gw=}" ;;
  esac
done
if [ -n "$GUEST_IP" ]; then
  ip addr add "$GUEST_IP" dev eth0
  ip link set eth0 up
  [ -n "$GUEST_GW" ] && ip route add default via "$GUEST_GW"
fi

exec /usr/bin/guestd
EOF
sudo chmod 755 "$MNT/sbin/init"

sudo umount "$MNT"
trap - EXIT
rmdir "$MNT"
echo ">> rootfs: $ROOTFS"
