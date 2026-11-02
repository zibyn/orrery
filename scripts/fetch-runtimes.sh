#!/usr/bin/env bash
# fetch-runtimes.sh ARCH DEST [CLI_DEST] downloads the runtimes the node
# package bundles (ADR-0028) for linux/ARCH, at the versions and sha256 in
# ci.yml's workflow env, and puts them in DEST as install.sh copies them to
# /opt/orrery/bin/: flat, plus runsc's gvisor-bin/. CLI_DEST, when given,
# also gets ctr and buildctl from the same releases; the package has neither.
# The release job and scripts/ci-e2e-node.sh both use it.
set -euo pipefail

[ $# -ge 2 ] || { echo "usage: fetch-runtimes.sh amd64|arm64 DEST [CLI_DEST]" >&2; exit 2; }
arch="$1" dest="$2" cli_dest="${3:-}"
case "$arch" in
  amd64) gvisor_arch=x86_64 ;;
  arm64) gvisor_arch=aarch64 ;;
  *) echo "::error::fetch-runtimes.sh: unknown arch $arch" >&2; exit 2 ;;
esac
sum_suffix="SHA256_${arch^^}"
# RUNTIMES_CACHE, one directory per arch, reuses files downloaded before;
# they are checked like new ones.
dl="${RUNTIMES_CACHE:-}"
[ -n "$dl" ] || { dl="$(mktemp -d)"; trap 'rm -rf "$dl"' EXIT; }

# fetch URL NAME downloads URL, checks it against ${NAME}_SHA256_<ARCH> and
# prints its path. Call it in an assignment, so a mismatch stops the script.
fetch() {
  local sum_var="$2_$sum_suffix" out="$dl/${1##*/}"
  [ -f "$out" ] || curl -fsSL --retry 3 "$1" -o "$out"
  echo "${!sum_var}  $out" | sha256sum -c --quiet - >&2 ||
    { echo "::error::$1 does not match its pinned sha256" >&2; exit 1; }
  printf '%s' "$out"
}

mkdir -p "$dest"
containerd_tgz="$(fetch "https://github.com/containerd/containerd/releases/download/v$CONTAINERD_VERSION/containerd-$CONTAINERD_VERSION-linux-$arch.tar.gz" CONTAINERD)"
tar -C "$dest" --strip-components=1 -xzf "$containerd_tgz" bin/containerd bin/containerd-shim-runc-v2
runc_bin="$(fetch "https://github.com/opencontainers/runc/releases/download/v$RUNC_VERSION/runc.$arch" RUNC)"
install -m 0755 "$runc_bin" "$dest/runc"
buildkit_tgz="$(fetch "https://github.com/moby/buildkit/releases/download/$BUILDKIT_VERSION/buildkit-$BUILDKIT_VERSION.linux-$arch.tar.gz" BUILDKIT)"
tar -C "$dest" --strip-components=1 -xzf "$buildkit_tgz" bin/buildkitd
gvisor_tbz="$(fetch "https://storage.googleapis.com/gvisor/releases/release/${GVISOR_VERSION#release-}/$gvisor_arch/gvisor.tar.bz2" GVISOR)"
tar -C "$dest" -xjf "$gvisor_tbz"

if [ -n "$cli_dest" ]; then
  mkdir -p "$cli_dest"
  tar -C "$cli_dest" --strip-components=1 -xzf "$containerd_tgz" bin/ctr
  tar -C "$cli_dest" --strip-components=1 -xzf "$buildkit_tgz" bin/buildctl
fi
