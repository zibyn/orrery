#!/usr/bin/env bash
# Turns a GitHub Actions Ubuntu runner into the e2e suite's containerd Node
# the way install.sh sets up a Node (ADR-0028): the node package's runtimes,
# fetched as the release job does (scripts/fetch-runtimes.sh), run as
# orrery-containerd and orrery-buildkitd with install.sh's own configs and
# units. The runner's containerd and Docker are left alone. The
# Agent needs no CNI plugins, nft or ip (#205). scripts/e2e.sh then runs as
# root, so every Agent it starts is a root containerd Agent on these daemons.
set -euo pipefail

[ "$(id -u)" = 0 ] || exec sudo -E "$0" "$@"
# shellcheck source=../deploy/install.sh
source "$(dirname "$0")/../deploy/install.sh"

dl="$(mktemp -d)"
trap 'rm -rf "$dl"' EXIT
# The suite's ctr and buildctl come from the same releases; the package has none.
"$(dirname "$0")/fetch-runtimes.sh" amd64 "$dl/bin" "$dl/cli"
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

ctr --address "$NODE_CTRD_SOCK" version ||
  { journalctl -u orrery-containerd --no-pager; exit 1; }
for _ in $(seq 1 60); do
  buildctl --addr "$NODE_BK_ADDR" debug workers >/dev/null 2>&1 && break
  sleep 1
done
buildctl --addr "$NODE_BK_ADDR" debug workers || { journalctl -u orrery-buildkitd --no-pager; exit 1; }

# Docker's FORWARD policy is drop. Let Sandbox traffic on the Agent's
# bridge (orrery0) through, so Sandboxes reach the internet as they did on
# the Docker Node; Egress Rules still filter it on the bridge itself.
iptables -I DOCKER-USER -i orrery0 -j ACCEPT
iptables -I DOCKER-USER -o orrery0 -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT

if [ -n "${GITHUB_ENV:-}" ]; then
  echo "ORRERY_E2E_BUILDKIT=$NODE_BK_ADDR" >> "$GITHUB_ENV"
fi
