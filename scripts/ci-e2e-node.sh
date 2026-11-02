#!/usr/bin/env bash
# Turns a GitHub Actions Ubuntu runner into the e2e suite's containerd Node
# the way install.sh sets up a Node (ADR-0028): the node package's containerd,
# runc and buildkitd, at the versions and sha256 the release job pins (ci.yml's
# env), run as orrery-containerd and orrery-buildkitd with install.sh's own
# configs and units. The runner's containerd and Docker are left alone. The
# Agent needs no CNI plugins, nft or ip (#205). scripts/e2e.sh then runs as
# root, so every Agent it starts is a root containerd Agent on these daemons.
set -euo pipefail

[ "$(id -u)" = 0 ] || exec sudo -E "$0" "$@"
# shellcheck source=../deploy/install.sh
source "$(dirname "$0")/../deploy/install.sh"

dl="$(mktemp -d)"
trap 'rm -rf "$dl"' EXIT
fetch() { # url sha256 out-file
  curl -fsSL --retry 3 "$1" -o "$3"
  echo "$2  $3" | sha256sum -c --quiet -
}
mkdir "$dl/bin" "$dl/cli"
fetch "https://github.com/containerd/containerd/releases/download/v$CONTAINERD_VERSION/containerd-$CONTAINERD_VERSION-linux-amd64.tar.gz" "$CONTAINERD_SHA256_AMD64" "$dl/containerd.tar.gz"
tar -C "$dl/bin" --strip-components=1 -xzf "$dl/containerd.tar.gz" bin/containerd bin/containerd-shim-runc-v2
fetch "https://github.com/opencontainers/runc/releases/download/v$RUNC_VERSION/runc.amd64" "$RUNC_SHA256_AMD64" "$dl/runc"
install -m 0755 "$dl/runc" "$dl/bin/runc"
fetch "https://github.com/moby/buildkit/releases/download/$BUILDKIT_VERSION/buildkit-$BUILDKIT_VERSION.linux-amd64.tar.gz" "$BUILDKIT_SHA256_AMD64" "$dl/buildkit.tar.gz"
tar -C "$dl/bin" --strip-components=1 -xzf "$dl/buildkit.tar.gz" bin/buildkitd
# The suite's ctr and buildctl, from the same releases; the package has none.
tar -C "$dl/cli" --strip-components=1 -xzf "$dl/containerd.tar.gz" bin/ctr
tar -C "$dl/cli" --strip-components=1 -xzf "$dl/buildkit.tar.gz" bin/buildctl
install -m 0755 "$dl/cli/ctr" "$dl/cli/buildctl" /usr/local/bin/

mkdir -p "$NODE_BIN" "$NODE_CONF"
install_bins "$dl/bin" "$NODE_BIN" >/dev/null
containerd_toml > "$NODE_CONF/containerd.toml"
# The platform registry is plain HTTP on loopback (scripts/e2e.sh).
{ buildkitd_toml; printf '\n[registry."127.0.0.1:15000"]\n  http = true\n'; } > "$NODE_CONF/buildkitd.toml"
containerd_unit > /etc/systemd/system/orrery-containerd.service
buildkitd_unit > /etc/systemd/system/orrery-buildkitd.service
systemctl daemon-reload
systemctl start orrery-containerd orrery-buildkitd

ctr --address /run/orrery/containerd/containerd.sock version ||
  { journalctl -u orrery-containerd --no-pager; exit 1; }
BUILDKIT_ADDR=unix:///run/orrery/buildkit/buildkitd.sock
for _ in $(seq 1 60); do
  buildctl --addr "$BUILDKIT_ADDR" debug workers >/dev/null 2>&1 && break
  sleep 1
done
buildctl --addr "$BUILDKIT_ADDR" debug workers || { journalctl -u orrery-buildkitd --no-pager; exit 1; }

# Docker's FORWARD policy is drop. Let Sandbox traffic on the Agent's
# bridge (orrery0) through, so Sandboxes reach the internet as they did on
# the Docker Node; Egress Rules still filter it on the bridge itself.
iptables -I DOCKER-USER -i orrery0 -j ACCEPT
iptables -I DOCKER-USER -o orrery0 -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT

if [ -n "${GITHUB_ENV:-}" ]; then
  echo "ORRERY_E2E_BUILDKIT=$BUILDKIT_ADDR" >> "$GITHUB_ENV"
fi
