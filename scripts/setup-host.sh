#!/usr/bin/env bash
# Builds everything the demo hosts need, on the Linux machine that has KVM
# (the Lima VM on macOS, or a bare-metal box directly):
#   1. Firecracker v1.16.1 from source, with our live-migration patches
#   2. A guest kernel from Firecracker's CI artifacts (arch-matched)
#   3. A minimal busybox + guestd root filesystem
#   4. A shared "storage array" dir both host containers mount (/srv/fcmig)
#
# Run via `make setup`, or directly:  scripts/setup-host.sh
set -euo pipefail

REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BUILD_DIR="$REPO_DIR/build"
SRC_DIR="$HOME/.cache/fcmig/firecracker"
FC_TAG="v1.16.1"

ARCH="$(uname -m)"   # aarch64 or x86_64
case "$ARCH" in
  aarch64|arm64) ARCH=aarch64; RUST_TARGET=aarch64-unknown-linux-musl ;;
  x86_64|amd64)  ARCH=x86_64;  RUST_TARGET=x86_64-unknown-linux-musl ;;
  *) echo "unsupported arch: $ARCH" >&2; exit 1 ;;
esac
# Newest CI folder that publishes a 6.1 kernel for this arch (the CI bucket
# lags release tags; probe downward from the pinned line).
CI_KERNEL_PREFIX=""
for ver in v1.15 v1.14 v1.13; do
  pfx="firecracker-ci/$ver/$ARCH/vmlinux-6.1"
  if curl -fsS "http://spec.ccfc.min.s3.amazonaws.com/?prefix=$pfx&list-type=2" | grep -q "<Key>"; then
    CI_KERNEL_PREFIX="$pfx"; break
  fi
done
: "${CI_KERNEL_PREFIX:?no CI 6.1 kernel found for $ARCH}"

mkdir -p "$BUILD_DIR"
export PATH="$HOME/.cargo/bin:$PATH"

# --- 1. Firecracker from source --------------------------------------------
if [[ ! -d "$SRC_DIR/.git" ]]; then
  echo ">> Cloning firecracker @ $FC_TAG"
  git clone --depth 1 --branch "$FC_TAG" \
    https://github.com/firecracker-microvm/firecracker "$SRC_DIR"
fi

cd "$SRC_DIR"
# Apply our patches idempotently: reset to the pinned tag, then apply.
git checkout -q -f "$FC_TAG"
git clean -qfd src 2>/dev/null || true
shopt -s nullglob
patches=("$REPO_DIR"/patches/*.patch)
if ((${#patches[@]})); then
  echo ">> Applying ${#patches[@]} patch(es)"
  git -c user.name=build -c user.email=build@local am --3way "${patches[@]}"
fi

# musl, like upstream releases: the gnu target ships no default seccomp policy
# and would run the VMM unsandboxed. userfaultfd-sys compiles C shims, so its
# cc must be musl-gcc with kernel headers visible (-idirafter keeps musl's own
# headers ahead of the glibc ones).
echo ">> Building firecracker (release, $RUST_TARGET)"
rustup target add "$RUST_TARGET" >/dev/null
CC_VAR="CC_${RUST_TARGET//-/_}"
CFLAGS_VAR="CFLAGS_${RUST_TARGET//-/_}"
env "$CC_VAR=musl-gcc" \
    "$CFLAGS_VAR=-idirafter /usr/include -idirafter /usr/include/$(uname -m)-linux-gnu" \
    cargo build --release -p firecracker --target "$RUST_TARGET"
cp -f "build/cargo_target/$RUST_TARGET/release/firecracker" "$BUILD_DIR/firecracker"

# --- 2. Guest kernel from Firecracker CI artifacts -------------------------
if [[ ! -f "$BUILD_DIR/vmlinux" ]]; then
  echo ">> Fetching guest kernel ($CI_KERNEL_PREFIX*)"
  key="$(curl -fsS "http://spec.ccfc.min.s3.amazonaws.com/?prefix=$CI_KERNEL_PREFIX&list-type=2" |
    grep -oP '(?<=<Key>)[^<]+' | grep -v '\.config$' | sort -V | tail -1)"
  [[ -n "$key" ]] || { echo "no CI kernel for prefix $CI_KERNEL_PREFIX" >&2; exit 1; }
  curl -fsS -o "$BUILD_DIR/vmlinux" "https://s3.amazonaws.com/spec.ccfc.min/$key"
  echo ">> Kernel: $key"
fi

# --- 3. Guest rootfs --------------------------------------------------------
"$REPO_DIR/scripts/build-rootfs.sh" "$BUILD_DIR"

# --- 4. Shared storage ------------------------------------------------------
# /srv/fcmig plays the role of the shared storage array (the NFS/SAN of a real
# cluster). Both host containers bind-mount it; the guest rootfs lives here so
# migration never copies disk data.
sudo mkdir -p /srv/fcmig
sudo cp -f "$BUILD_DIR/vmlinux" "$BUILD_DIR/rootfs.ext4" /srv/fcmig/
sudo chmod 666 /srv/fcmig/rootfs.ext4
sudo chmod 644 /srv/fcmig/vmlinux

echo ">> setup-host.sh done ($ARCH):"
ls -lh "$BUILD_DIR/firecracker" /srv/fcmig/
