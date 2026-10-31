#!/usr/bin/env bash
# Turns a GitHub Actions Ubuntu runner into the e2e suite's containerd Node
# (#170): the runner's containerd (2.x), the CNI plugins in /opt/cni/bin, nft,
# and a standalone buildkitd. scripts/e2e.sh then runs as root, so every Agent
# it starts is a root containerd Agent. The versions match the hand-tested
# containerd host (docs/research/containerd-cdi-runsc-cni-spike.md).
set -euo pipefail

CNI_VERSION=v1.9.1
BUILDKIT_VERSION=v0.34.1
CONTAINERD_VERSION=2.2.1
BUILDKIT_DIR=/run/orrery-buildkit
BUILDKIT_ADDR="unix://$BUILDKIT_DIR/buildkitd.sock"

sudo mkdir -p /opt/cni/bin
curl -fsSL "https://github.com/containernetworking/plugins/releases/download/$CNI_VERSION/cni-plugins-linux-amd64-$CNI_VERSION.tgz" |
  sudo tar -xz -C /opt/cni/bin
command -v nft >/dev/null || sudo apt-get install -y --no-install-recommends nftables

# The Agent speaks the containerd 2.x API. The runner's containerd serves
# Docker too, so an older one is upgraded in place and both are restarted.
current="$(containerd --version | awk '{print $3}')"
echo "runner containerd: $current"
case "${current#v}" in
  1.*)
    curl -fsSL "https://github.com/containerd/containerd/releases/download/v$CONTAINERD_VERSION/containerd-$CONTAINERD_VERSION-linux-amd64.tar.gz" |
      sudo tar -xz -C /usr
    sudo systemctl restart containerd docker
    containerd --version
    ;;
esac
sudo ctr version

curl -fsSL "https://github.com/moby/buildkit/releases/download/$BUILDKIT_VERSION/buildkit-$BUILDKIT_VERSION.linux-amd64.tar.gz" |
  sudo tar -xz -C /usr/local
sudo mkdir -p "$BUILDKIT_DIR"
# The platform registry is plain HTTP on loopback (scripts/e2e.sh).
printf '[registry."127.0.0.1:15000"]\n  http = true\n' | sudo tee "$BUILDKIT_DIR/buildkitd.toml" >/dev/null
sudo sh -c "nohup buildkitd --config $BUILDKIT_DIR/buildkitd.toml --root /var/lib/orrery-buildkit \
  --addr $BUILDKIT_ADDR --oci-worker=true --containerd-worker=false --oci-worker-net host \
  >$BUILDKIT_DIR/buildkitd.log 2>&1 &"
for _ in $(seq 1 60); do
  sudo buildctl --addr "$BUILDKIT_ADDR" debug workers >/dev/null 2>&1 && break
  sleep 1
done
sudo buildctl --addr "$BUILDKIT_ADDR" debug workers || { sudo cat "$BUILDKIT_DIR/buildkitd.log"; exit 1; }

# Docker's FORWARD policy is drop. Let Sandbox traffic on the Agent's CNI
# bridge (orrery0) through, so Sandboxes reach the internet as they did on
# the Docker Node; Egress Rules still filter it on the bridge itself.
sudo iptables -I DOCKER-USER -i orrery0 -j ACCEPT
sudo iptables -I DOCKER-USER -o orrery0 -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT

if [ -n "${GITHUB_ENV:-}" ]; then
  echo "ORRERY_E2E_BUILDKIT=$BUILDKIT_ADDR" >> "$GITHUB_ENV"
fi
