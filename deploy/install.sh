#!/usr/bin/env bash
# Installs or upgrades a Stars Sandbox host from a GitHub release (ADR-0023).
# Fetch it from the release tag and run it as root; docs/deploy.md has the
# full walkthrough. Re-run with a newer --version to upgrade, Control Plane
# first, then Nodes (ADR-0014).
set -euo pipefail

REPO=zibyn/stars-sandbox

usage() {
  cat >&2 <<'EOF'
usage:
  install.sh server --version vX.Y.Z [--public-host HOST]   (required on first install) [--tls self-signed|proxy]
                    [--tls-cert FILE --tls-key FILE]
                    [--access-url URL --registry-addr HOST[:PORT] --s3-public-endpoint URL]
  install.sh node   --version vX.Y.Z [--server HOST:PORT --packet sse_...] [--ca FILE]
                    [--trust-registry HOST:PORT] [--runtime docker|podman|local] [--name NAME]
                    [-- extra `stars-sandbox-agent run` flags]
EOF
  exit 2
}

die() { echo "install.sh: $*" >&2; exit 1; }
need() { for c in "$@"; do command -v "$c" >/dev/null || die "$c is required but not installed"; done; }
rand() { openssl rand -hex "${1:-24}"; }

check_version() {
  [[ "$1" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] ||
    die "--version must be a release tag such as v1.2.3 (there is no latest)"
}

server() {
  local version="" host="" tls=self-signed cert="" key="" access_url="" registry_addr="" s3_url=""
  while [ $# -gt 0 ]; do
    case "$1" in
      --version) version="$2" ;;
      --public-host) host="$2" ;;
      --tls) tls="$2" ;;
      --tls-cert) cert="$2" ;;
      --tls-key) key="$2" ;;
      --access-url) access_url="$2" ;;
      --registry-addr) registry_addr="$2" ;;
      --s3-public-endpoint) s3_url="$2" ;;
      *) usage ;;
    esac
    shift 2
  done
  check_version "$version"
  [ "$(id -u)" = 0 ] || die "run as root"
  need docker openssl curl
  docker compose version >/dev/null 2>&1 || die "the docker compose plugin is required"

  local etc=/etc/stars-sandbox backups=/var/lib/stars-sandbox-server/backups previous=""
  mkdir -p "$etc" "$backups"
  chmod 700 "$etc" "$backups"
  cd "$etc"
  [ -f .env ] && previous="$(sed -n 's/^STARS_VERSION=//p' .env)"

  if [ -n "$previous" ] && [ "$previous" != "$version" ]; then
    # ponytail: sort -V orders v1.2.0 before v1.2.0-rc.1, so going from an rc
    # to its final release reads as a downgrade; compare semver if rcs ship.
    [ "$(printf '%s\n' "$previous" "$version" | sort -V | head -n1)" = "$previous" ] ||
      die "refusing to downgrade $previous to $version: migrations are forward-only; restore a backup from $backups instead"
  fi

  if [ ! -f .env ]; then
    first_install "$version" "$host" "$tls" "$cert" "$key" "$access_url" "$registry_addr" "$s3_url"
  elif [ -n "$host$cert$access_url$registry_addr$s3_url" ] || [ "$tls" != self-signed ]; then
    echo "install.sh: $etc/.env exists; addresses and TLS are kept, edit .env to change them" >&2
  fi

  curl -fsSL "https://raw.githubusercontent.com/$REPO/$version/deploy/release/compose.yaml" -o compose.yaml.new
  mv compose.yaml.new compose.yaml
  # Pull before stopping anything, so the outage is only the restart.
  STARS_VERSION="$version" docker compose pull --quiet

  # ADR-0014 upgrade: stop the Control Plane, back up PostgreSQL, start the
  # new version.
  if [ -n "$previous" ] && [ "$previous" != "$version" ]; then
    local dump="$backups/$(date -u +%Y%m%dT%H%M%SZ)-$previous.sql.gz"
    docker compose stop server
    docker compose exec -T postgres pg_dump -U stars stars | gzip > "$dump" ||
      die "backup failed; the old Control Plane is stopped, start it with: cd $etc && docker compose start server"
    echo "backed up PostgreSQL to $dump"
  fi
  sed -i "s/^STARS_VERSION=.*/STARS_VERSION=$version/" .env

  docker compose up -d --remove-orphans
  local code=""
  for _ in $(seq 1 120); do
    code="$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:18080/api/v1/users || true)"
    [ "$code" = 401 ] && break
    sleep 1
  done
  [ "$code" = 401 ] || die "the Control Plane did not come up; see: cd $etc && docker compose logs server"
  echo "Control Plane $version is running"
  [ -z "$previous" ] && next_steps "$version"
  return 0
}

# first_install writes TLS material, secrets, the registry config and .env.
# None of it is ever regenerated.
first_install() {
  local version="$1" host="$2" tls="$3" cert="$4" key="$5" access_url="$6" registry_addr="$7" s3_url="$8"
  local bind="" listen=":15000" registry_tls=""
  # Agents dial the `node` surface at this host, so it is the certificate's SAN.
  [ -n "$host" ] || die "--public-host is required on first install"
  case "$tls" in
    self-signed)
      access_url="${access_url:-http://$host:18080}"
      registry_addr="${registry_addr:-$host:15000}"
      s3_url="${s3_url:-http://$host:19000}"
      registry_tls=$'  tls:\n    certificate: /tls/server.crt\n    key: /tls/server.key'
      ;;
    proxy)
      [ -n "$access_url" ] && [ -n "$registry_addr" ] && [ -n "$s3_url" ] ||
        die "--tls proxy needs --access-url, --registry-addr and --s3-public-endpoint"
      bind="127.0.0.1:"
      listen="127.0.0.1:15000"
      ;;
    *) die "--tls is self-signed or proxy" ;;
  esac

  mkdir -p tls ca keys registry
  # The `node` surface is TLS in both modes (ADR-0014).
  if [ -n "$cert" ]; then
    [ -n "$key" ] || die "--tls-cert needs --tls-key"
    install -m 0644 "$cert" tls/server.crt
    install -m 0600 "$key" tls/server.key
  else
    local san="DNS:$host"
    [[ "$host" =~ ^[0-9.]+$ ]] && san="IP:$host"
    # ponytail: ten-year certificates and no rotation; re-issue by hand
    # (docs/deploy.md) if a deployment outlives them.
    openssl req -x509 -newkey rsa:3072 -nodes -days 3650 -subj "/CN=Stars Sandbox CA" \
      -keyout ca.key -out ca/ca.crt 2>/dev/null
    chmod 600 ca.key
    openssl req -newkey rsa:3072 -nodes -subj "/CN=$host" -keyout tls/server.key -out tls/server.csr 2>/dev/null
    openssl x509 -req -in tls/server.csr -CA ca/ca.crt -CAkey ca.key -set_serial "0x$(rand 16)" -days 3650 \
      -extfile <(printf 'subjectAltName=%s\nextendedKeyUsage=serverAuth\nbasicConstraints=CA:FALSE\n' "$san") \
      -out tls/server.crt 2>/dev/null
    rm tls/server.csr
  fi

  local pg s3_secret registry_password auth secure=false
  pg="$(rand)"
  s3_secret="$(rand)"
  registry_password="$(rand)"
  auth="$(printf 'stars:%s' "$registry_password" | base64 -w0)"
  printf 'k1 %s\n' "$(openssl rand -base64 32)" > keys/encryption
  docker run --rm httpd:2.4-alpine htpasswd -Bbn stars "$registry_password" > registry/htpasswd
  [[ "$s3_url" == https://* ]] && secure=true
  cat > registry/config.yml <<EOF
version: 0.1
log:
  level: info
storage:
  s3:
    accesskey: stars
    secretkey: $s3_secret
    region: us-east-1
    regionendpoint: $s3_url
    bucket: stars
    rootdirectory: /registry
    forcepathstyle: true
    secure: $secure
http:
  addr: $listen
  secret: $(rand)
$registry_tls
auth:
  htpasswd:
    realm: stars-sandbox
    path: /etc/distribution/htpasswd
EOF
  # The server container runs as uid 10001.
  chown 10001:10001 tls/server.key keys/encryption
  chmod 600 tls/server.key keys/encryption registry/config.yml registry/htpasswd

  cat > .env <<EOF
# Written once by install.sh (ADR-0023); edit to change settings, then run
# docker compose up -d. Every STARS_* setting is in docs/spec/defaults.md.
STARS_VERSION=$version
# STARS_PUBLIC_HOST and STARS_TLS are read by install.sh only.
STARS_PUBLIC_HOST=$host
STARS_TLS=$tls
STARS_BIND=$bind
STARS_POSTGRES_PASSWORD=$pg
STARS_DATABASE_URL=postgres://stars:$pg@postgres:5432/stars?sslmode=disable
STARS_ACCESS_URL=$access_url
STARS_S3_PUBLIC_ENDPOINT=$s3_url
STARS_S3_BUCKET=stars
STARS_S3_REGION=us-east-1
STARS_S3_ACCESS_KEY=stars
STARS_S3_SECRET_KEY=$s3_secret
STARS_REGISTRY_ADDR=$registry_addr
STARS_REGISTRY_CREDENTIAL='{"auths":{"$registry_addr":{"auth":"$auth"}}}'
STARS_PORT_TICKET_KEY=$(rand 32)
# A Sandbox needs a Platform Snapshot, referenced by digest:
# STARS_PLATFORM_SNAPSHOTS=default=docker.io/library/ubuntu:24.04@sha256:...
# STARS_DEFAULT_SNAPSHOT=default
EOF
  chmod 600 .env
}

next_steps() {
  local version="$1" etc=/etc/stars-sandbox node_host registry_addr ca_flags=""
  node_host="$(sed -n 's/^STARS_PUBLIC_HOST=//p' "$etc/.env")"
  registry_addr="$(sed -n 's/^STARS_REGISTRY_ADDR=//p' "$etc/.env")"
  if [ -f "$etc/ca/ca.crt" ]; then
    ca_flags="--ca ca.crt"
    grep -q '^STARS_TLS=self-signed' "$etc/.env" && ca_flags="$ca_flags --trust-registry $registry_addr"
  fi
  grep -q '^STARS_TLS=proxy' "$etc/.env" && cat <<EOF

Point the HTTPS proxy at: api -> 127.0.0.1:18080, registry -> 127.0.0.1:15000,
object store -> 127.0.0.1:19000. Port 18081 (node surface) is never proxied.
EOF
  cat <<EOF

Next steps:
  1. Create the first admin:
       cd $etc && docker compose exec server stars-sandbox-server bootstrap
  2. Issue an Enrollment Packet (valid for 1 hour):
       TOKEN=\$(curl -fsS -H 'Content-Type: application/json' \\
         -d '{"username":"admin","password":"...","token":{"name":"ops","scope":"admin"}}' \\
         http://127.0.0.1:18080/api/v1/sessions | sed -n 's/.*"token":"\\([^"]*\\)".*/\\1/p')
       curl -fsS -X POST -H "Authorization: Bearer \$TOKEN" http://127.0.0.1:18080/api/v1/enrollment-packets
  3. On each Node$( [ -n "$ca_flags" ] && echo ", after copying $etc/ca/ca.crt there as ca.crt"):
       curl -fsSL https://raw.githubusercontent.com/$REPO/$version/deploy/install.sh | sudo bash -s -- node \\
         --version $version --server $node_host:18081 --packet sse_... $ca_flags
EOF
}

node() {
  local version="" server="" packet="" ca="" trust_registry="" runtime="" name="" extra=() write_unit=0
  while [ $# -gt 0 ]; do
    case "$1" in
      --version) version="$2" ;;
      --server) server="$2" ;;
      --packet) packet="$2" ;;
      --ca) ca="$2" ;;
      --trust-registry) trust_registry="$2" ;;
      --runtime) runtime="$2"; write_unit=1 ;;
      --name) name="$2" ;;
      --) shift; extra=("$@"); write_unit=1; break ;;
      *) usage ;;
    esac
    shift 2
  done
  check_version "$version"
  [ "$(id -u)" = 0 ] || die "run as root"
  need curl tar sha256sum systemctl
  case "${runtime:-docker}" in
    docker) need docker ;;
    podman) need podman ;;
    local) ;;
    *) die "--runtime is docker, podman or local" ;;
  esac

  local arch bin=/opt/stars-sandbox/bin conf=/etc/stars-sandbox-agent
  local state=/var/lib/stars-sandbox-agent/identity.json unit=/etc/systemd/system/stars-sandbox-agent.service
  case "$(uname -m)" in
    x86_64) arch=amd64 ;;
    aarch64 | arm64) arch=arm64 ;;
    *) die "unsupported architecture $(uname -m)" ;;
  esac

  local tmp pkg="stars-sandbox-node_${version}_linux_${arch}.tar.gz"
  local base="https://github.com/$REPO/releases/download/$version"
  tmp="$(mktemp -d)"
  trap "rm -rf '$tmp'" EXIT
  curl -fsSL "$base/$pkg" -o "$tmp/$pkg"
  curl -fsSL "$base/SHA256SUMS" -o "$tmp/SHA256SUMS"
  (cd "$tmp" && grep "  $pkg\$" SHA256SUMS | sha256sum -c --quiet -) || die "$pkg does not match SHA256SUMS"
  tar -C "$tmp" -xzf "$tmp/$pkg"
  mkdir -p "$bin" "$conf"
  # The Agent finds the CLI beside its own executable (runtime.md).
  for b in stars-sandbox-agent stars-sandbox; do
    install -m 0755 "$tmp/$b" "$bin/.$b.new"
    mv -f "$bin/.$b.new" "$bin/$b"
  done

  if [ -n "$ca" ]; then
    install -m 0644 "$ca" "$conf/ca.crt"
  fi
  if [ -n "$trust_registry" ]; then
    [ -f "$conf/ca.crt" ] || die "--trust-registry needs --ca"
    # Docker and Podman read per-registry CAs without a daemon restart.
    for d in /etc/docker/certs.d /etc/containers/certs.d; do
      install -D -m 0644 "$conf/ca.crt" "$d/$trust_registry/ca.crt"
    done
  fi
  local ca_flag=()
  [ -f "$conf/ca.crt" ] && ca_flag=(--ca "$conf/ca.crt")

  if [ ! -f "$state" ]; then
    [ -n "$server" ] && [ -n "$packet" ] || die "first install needs --server and --packet"
    "$bin/stars-sandbox-agent" enroll --server "$server" --packet "$packet" "${ca_flag[@]}" ${name:+--name "$name"}
  fi

  # An upgrade without --runtime or extra flags keeps the existing unit.
  if [ "$write_unit" = 1 ] || [ ! -f "$unit" ]; then
    cat > "$unit" <<EOF
[Unit]
Description=Stars Sandbox Node Agent
After=network-online.target docker.service podman.socket
Wants=network-online.target

[Service]
ExecStart=$bin/stars-sandbox-agent run --runtime ${runtime:-docker} ${ca_flag[*]} ${extra[*]}
Restart=always
RestartSec=5
# Stop signals reach the Agent only, so Exec Supervisors keep running across
# a restart (ADR-0014); the first SIGTERM exits gracefully.
KillMode=process

[Install]
WantedBy=multi-user.target
EOF
    systemctl daemon-reload
  fi
  systemctl enable --quiet stars-sandbox-agent
  systemctl restart stars-sandbox-agent
  echo "Node Agent $version is running: systemctl status stars-sandbox-agent"
}

case "${1:-}" in
  server) shift; server "$@" ;;
  node) shift; node "$@" ;;
  *) usage ;;
esac
