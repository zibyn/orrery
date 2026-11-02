#!/usr/bin/env bash
# Installs or upgrades a Orrery host from a GitHub release (ADR-0023).
# Fetch it from the release tag (or from main for a bootstrap) and run it as
# root; --version defaults to the newest release. docs/deploy.md has the full
# walkthrough. Re-run with a newer --version to upgrade, Control Plane first,
# then Nodes (ADR-0014).
set -euo pipefail

REPO=zibyn/orrery

usage() {
  cat >&2 <<'EOF'
usage:
  install.sh server [--version vX.Y.Z] (--public-host HOST | --access-url https://HOST[:PORT])
                    [--public-port PORT] [--s3-public-endpoint https://S3HOST[:PORT]]
                    [--public-bind ADDR] [--tls self-signed|cert]
                    [--tls-cert FILE --tls-key FILE] [--ca FILE]
                    [--registry-addr HOST[:PORT]]
  install.sh node   [--version vX.Y.Z] [--server HOST:PORT --packet ore_...] [--ca FILE]
                    [--trust-registry HOST[:PORT]]... [--runtime containerd|local]
                    [--name NAME]
                    [-- extra `orrery-agent run` flags]

--version defaults to the newest published release. To upgrade, rerun
the same command.
EOF
  exit 2
}

die() { echo "install.sh: $*" >&2; exit 1; }
need() { for c in "$@"; do command -v "$c" >/dev/null || die "$c is required but not installed"; done; }
rand() { openssl rand -hex "${1:-24}"; }
env_get() { sed -n "s/^$1=//p" "$2"; }
port_suffix() { [ "$1" = 443 ] || printf ':%s' "$1"; }
san_of() {
  local h="${1#[}"; h="${h%]}"
  if [[ "$h" =~ ^[0-9.]+$ ]] || [[ "$h" == *:* ]]; then printf 'IP:%s' "$h"; else printf 'DNS:%s' "$h"; fi
}

# split_url prints "HOST PORT" for an https URL with no path.
split_url() {
  local url="$1" rest scheme authority host port=443
  scheme="${url%%://*}"
  [ "$scheme" = "https" ] || die "$url must use https"
  rest="${url#*://}"
  authority="${rest%%/*}"
  [ "$rest" = "$authority" ] || [ "${rest#"$authority"}" = "/" ] ||
    die "$url must not have a path"
  case "$authority" in
    \[*\]:*) host="${authority%:*}"; port="${authority##*:}" ;;
    \[*\]) host="$authority" ;;
    *:*) host="${authority%:*}"; port="${authority##*:}" ;;
    *) host="$authority" ;;
  esac
  [ -n "$host" ] || die "$url has no host"
  [[ "$port" =~ ^[0-9]+$ ]] && [ "$port" -ge 1 ] && [ "$port" -le 65535 ] ||
    die "$url has an invalid port"
  printf '%s %s' "$host" "$port"
}

check_version() {
  [[ "$1" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] ||
    die "--version must be a release tag such as v1.2.3"
}

# latest_version reads the tag from GitHub's releases/latest redirect. The
# script still pins that exact tag; it never follows a branch.
latest_version() {
  local url
  url="$(curl -fsSLI -o /dev/null -w '%{url_effective}' \
    "https://github.com/$REPO/releases/latest")" || die "could not resolve the newest release; pass --version"
  url="${url##*/}"
  [[ "$url" =~ ^v[0-9] ]] || die "could not resolve the newest release; pass --version"
  check_version "$url"
  printf '%s' "$url"
}

# is_newer CURRENT CANDIDATE returns success when CANDIDATE is a newer
# release. A final release supersedes its own rc; otherwise sort -V.
is_newer() {
  local cur="$1" new="$2"
  [ "$cur" = "$new" ] && return 1
  if [ "${cur%%-*}" = "${new%%-*}" ]; then
    [[ "$cur" == *-* ]] && [[ "$new" != *-* ]] && return 0
    return 1
  fi
  [ "$(printf '%s\n' "$cur" "$new" | sort -V | head -n1)" = "$cur" ]
}

# wait_rustfs blocks until the object store's healthcheck passes.
wait_rustfs() {
  local id status
  for _ in $(seq 1 120); do
    id="$(docker compose ps -q rustfs)"
    if [ -n "$id" ]; then
      status="$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "$id")"
      [ "$status" = "healthy" ] && return 0
      [ "$status" = "exited" ] &&
        die "rustfs exited; see: cd /etc/orrery && docker compose logs rustfs"
    fi
    sleep 1
  done
  die "rustfs did not become healthy"
}

# create_bucket replaces the old rustfs-init service. It runs before the
# registry and the Control Plane start; both write to the bucket.
create_bucket() {
  local bucket=orrery region=us-east-1 access secret network id
  access="$(env_get ORRERY_S3_ACCESS_KEY .env)"
  secret="$(env_get ORRERY_S3_SECRET_KEY .env)"
  id="$(docker compose ps -q rustfs)"
  [ -n "$id" ] || die "rustfs is not running"
  network="$(docker inspect "$id" --format '{{range $k, $v := .NetworkSettings.Networks}}{{$k}}{{end}}')"
  [ -n "$network" ] || die "could not find the rustfs network"
  local -a envs=(-e "AWS_ACCESS_KEY_ID=$access" -e "AWS_SECRET_ACCESS_KEY=$secret"
    -e "AWS_DEFAULT_REGION=$region" -e AWS_EC2_METADATA_DISABLED=true)
  # An existing bucket makes `mb` fail, which is the wanted state.
  docker run --rm --network "$network" "${envs[@]}" amazon/aws-cli:2.17.0 \
    --endpoint-url http://rustfs:9000 s3 mb "s3://$bucket" >/dev/null 2>&1 || true
  for _ in $(seq 1 60); do
    if docker run --rm --network "$network" "${envs[@]}" amazon/aws-cli:2.17.0 \
      --endpoint-url http://rustfs:9000 s3api head-bucket --bucket "$bucket" >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  die "could not create the object-store bucket $bucket"
}

server() {
  local version="" host="" port="" tls=self-signed cert="" key="" ca="" access_url="" registry_addr="" s3_url="" bind=""
  while [ $# -gt 0 ]; do
    case "$1" in
      --version) version="$2"; shift 2 ;;
      --public-host) host="$2"; shift 2 ;;
      --public-port) port="$2"; shift 2 ;;
      --tls) tls="$2"; shift 2 ;;
      --tls-cert) cert="$2"; shift 2 ;;
      --tls-key) key="$2"; shift 2 ;;
      --ca) ca="$2"; shift 2 ;;
      --access-url) access_url="$2"; shift 2 ;;
      --registry-addr) registry_addr="$2"; shift 2 ;;
      --s3-public-endpoint) s3_url="$2"; shift 2 ;;
      --public-bind) bind="$2"; shift 2 ;;
      --auto-update | --no-auto-update) die "auto-update was removed (ADR-0028); upgrade by rerunning install.sh" ;;
      *) usage ;;
    esac
  done
  [ "$(id -u)" = 0 ] || die "run as root"
  need docker openssl curl flock
  docker compose version >/dev/null 2>&1 || die "the docker compose plugin is required"
  case "$version" in "" | latest) version="$(latest_version)" ;; esac
  check_version "$version"
  mkdir -p /etc/orrery
  # Concurrent install.sh runs share one lock.
  exec 9>/etc/orrery/.install.lock
  flock -n 9 || die "another install.sh run is in progress"

  local etc=/etc/orrery backups=/var/lib/orrery-server/backups previous=""
  mkdir -p "$etc" "$backups"
  chmod 700 "$etc" "$backups"
  cd "$etc"
  [ -f .env ] && previous="$(sed -n 's/^ORRERY_VERSION=//p' .env)"

  if [ -n "$previous" ] && [ "$previous" != "$version" ]; then
    is_newer "$previous" "$version" ||
      die "refusing to downgrade $previous to $version: migrations are forward-only; restore a backup from $backups instead"
  fi

  if [ ! -f .env ]; then
    first_install "$version" "$host" "$port" "$tls" "$cert" "$key" "$ca" "$access_url" "$registry_addr" "$s3_url" "$bind"
  else
    [ -f nginx/orrery.conf ] ||
      die "nginx/orrery.conf is missing; recreate it from deploy/release/nginx.conf at this release tag"
    if [ -n "$host$port$cert$key$ca$access_url$registry_addr$s3_url$bind" ] || [ "$tls" != self-signed ]; then
      echo "install.sh: $etc/.env exists; addresses and TLS are kept, edit .env, config/config.yaml and nginx/orrery.conf to change them" >&2
    fi
  fi

  curl -fsSL "https://raw.githubusercontent.com/$REPO/$version/deploy/release/compose.yaml" -o compose.yaml.new
  mv compose.yaml.new compose.yaml
  # Pull before stopping anything, so the outage is only the restart.
  ORRERY_VERSION="$version" docker compose pull --quiet

  # ADR-0014 upgrade: stop the Control Plane, back up PostgreSQL, start the
  # new version.
  if [ -n "$previous" ] && [ "$previous" != "$version" ]; then
    local dump
    dump="$backups/$(date -u +%Y%m%dT%H%M%SZ)-$previous.sql.gz"
    docker compose stop server
    docker compose exec -T postgres pg_dump -U orrery orrery | gzip > "$dump" ||
      die "backup failed; the old Control Plane is stopped, start it with: cd $etc && docker compose start server"
    echo "backed up PostgreSQL to $dump"
  fi
  sed -i "s/^ORRERY_VERSION=.*/ORRERY_VERSION=$version/" .env

  # The registry and the Control Plane write to the object store at startup,
  # so its bucket must exist before either starts.
  docker compose up -d rustfs
  wait_rustfs
  create_bucket

  docker compose up -d --remove-orphans
  # nginx reads its config and resolves the loopback upstreams at start.
  docker compose restart nginx

  local host port check_addr
  host="$(env_get ORRERY_PUBLIC_HOST .env)"
  port="$(env_get ORRERY_PUBLIC_PORT .env)"
  check_addr="$(env_get ORRERY_PUBLIC_BIND .env)"
  check_addr="${check_addr%:}"
  { [ -z "$check_addr" ] || [ "$check_addr" = "0.0.0.0" ]; } && check_addr=127.0.0.1
  local code=""
  for _ in $(seq 1 120); do
    # The public entry is TLS even when the certificate is generated.
    code="$(curl -ks -o /dev/null -w '%{http_code}' \
      --connect-to "$host:$port:$check_addr:$port" \
      "https://$host:$port/api/v1/users" || true)"
    [ "$code" = 401 ] && break
    sleep 1
  done
  [ "$code" = 401 ] || die "the Control Plane did not come up; see: cd $etc && docker compose logs server nginx"
  echo "Control Plane $version is running at https://$host:$port"
  remove_updater server
  [ -z "$previous" ] && next_steps "$version"
  return 0
}

# first_install writes TLS material, the nginx and registry configs, secrets
# and .env. None of it is ever regenerated.
first_install() {
  local version="$1" host="$2" port="$3" tls="$4" cert="$5" key="$6" ca="$7" \
    access_url="$8" registry_addr="$9" s3_url="${10}" bind="${11}"
  local pair s3_host s3_port listen public_bind resolve

  # The public URL is the source of truth; --public-host/--public-port fill it.
  if [ -n "$access_url" ]; then
    pair="$(split_url "$access_url")"
    local url_host="${pair% *}" url_port="${pair##* }"
    [ -z "$host" ] || [ "$host" = "$url_host" ] || die "--public-host $host does not match --access-url"
    [ -z "$port" ] || [ "$port" = "$url_port" ] || die "--public-port $port does not match --access-url"
    host="$url_host"; port="$url_port"
  fi
  [ -n "$host" ] || die "--public-host (or --access-url) is required on first install"
  case "$host" in
    */*) die "--public-host is a hostname, not a URL" ;;
  esac
  if [[ "$host" != \[* ]] && [[ "$host" == *:* ]]; then
    die "--public-host is a hostname; put the port in --public-port or --access-url"
  fi
  port="${port:-443}"
  [[ "$port" =~ ^[0-9]+$ ]] && [ "$port" -ge 1 ] && [ "$port" -le 65535 ] ||
    die "--public-port must be 1-65535"
  access_url="https://$host$(port_suffix "$port")"

  if [ -z "$s3_url" ]; then
    if [[ "$host" =~ ^[0-9.]+$ ]] || [[ "$host" == *:* ]]; then
      die "--s3-public-endpoint is required when --public-host is an IP"
    fi
    s3_url="https://s3.$host$(port_suffix "$port")"
  fi
  pair="$(split_url "$s3_url")"
  s3_host="${pair% *}"; s3_port="${pair##* }"
  [ "$s3_port" = "$port" ] ||
    die "the object-store endpoint must use the public port $port; the registry signs URLs with it"
  [ "$s3_host" != "$host" ] ||
    die "the object-store host must differ from the public host: SigV4 signs Host and path, so it cannot share one"
  s3_url="https://$s3_host$(port_suffix "$port")"

  registry_addr="${registry_addr:-$host$(port_suffix "$port")}"

  [ "$tls" = "self-signed" ] || [ "$tls" = "cert" ] || die "--tls is self-signed or cert"
  [ -n "$cert" ] && tls=cert
  if [ "$tls" = "cert" ]; then
    [ -n "$cert" ] && [ -n "$key" ] || die "--tls cert needs --tls-cert and --tls-key"
  elif [ -n "$ca" ]; then
    die "--ca is only used with --tls cert"
  fi

  bind="${bind%:}"
  [ "$bind" = "0.0.0.0" ] && bind=""
  listen="$port"
  [ -n "$bind" ] && listen="$bind:$port"
  public_bind=""
  [ -n "$bind" ] && public_bind="$bind:"
  # Where the host-network registry finds nginx's object-store vhost.
  resolve=127.0.0.1
  [ -n "$bind" ] && resolve="$bind"

  mkdir -p config tls ca registry nginx
  # One certificate covers the API and the object store; Agents and containers
  # see the same public entry (ADR-0023).
  if [ "$tls" = "cert" ]; then
    install -m 0644 "$cert" tls/server.crt
    install -m 0600 "$key" tls/server.key
    if [ -n "$ca" ]; then
      install -m 0644 "$ca" ca/ca.crt
    else
      rm -f ca/ca.crt
    fi
  else
    local sans
    sans="$(san_of "$host"),$(san_of "$s3_host")"
    # ponytail: ten-year certificates and no rotation; re-issue by hand
    # (docs/deploy.md) if a deployment outlives them.
    openssl req -x509 -newkey rsa:3072 -nodes -days 3650 -subj "/CN=Orrery CA" \
      -keyout ca.key -out ca/ca.crt 2>/dev/null
    chmod 600 ca.key
    chmod 0644 ca/ca.crt
    openssl req -newkey rsa:3072 -nodes -subj "/CN=$host" -keyout tls/server.key -out tls/server.csr 2>/dev/null
    openssl x509 -req -in tls/server.csr -CA ca/ca.crt -CAkey ca.key -set_serial "0x$(rand 16)" -days 3650 \
      -extfile <(printf 'subjectAltName=%s\nextendedKeyUsage=serverAuth\nbasicConstraints=CA:FALSE\n' "$sans") \
      -out tls/server.crt 2>/dev/null
    rm tls/server.csr
  fi
  # A supplied certificate that misses a hostname would only fail at TLS time.
  local check
  for check in "$host" "$s3_host"; do
    check="${check#[}"; check="${check%]}"
    if [[ "$check" =~ ^[0-9.]+$ ]] || [[ "$check" == *:* ]]; then
      openssl x509 -in tls/server.crt -noout -checkip "$check" >/dev/null 2>&1 ||
        echo "install.sh: warning: tls/server.crt does not cover $check" >&2
    else
      openssl x509 -in tls/server.crt -noout -checkhost "$check" >/dev/null 2>&1 ||
        echo "install.sh: warning: tls/server.crt does not cover $check" >&2
    fi
  done

  local pg s3_secret registry_password
  pg="$(rand)"
  s3_secret="$(rand)"
  registry_password="$(rand)"
  docker run --rm httpd:2.4-alpine htpasswd -Bbn orrery "$registry_password" > registry/htpasswd
  cat > registry/config.yml <<EOF
version: 0.1
log:
  level: info
storage:
  s3:
    accesskey: orrery
    secretkey: $s3_secret
    region: us-east-1
    regionendpoint: $s3_url
    bucket: orrery
    rootdirectory: /registry
    forcepathstyle: true
    secure: true
  # purge deletes the manifests of deleted SnapshotVersions (#151).
  delete:
    enabled: true
http:
  # Plain HTTP on loopback; nginx is the public TLS entry (ADR-0023).
  addr: 127.0.0.1:15000
  secret: $(rand)
auth:
  htpasswd:
    realm: orrery
    path: /etc/distribution/htpasswd
EOF

  local tmp
  tmp="$(mktemp)"
  curl -fsSL "https://raw.githubusercontent.com/$REPO/$version/deploy/release/nginx.conf" -o "$tmp" ||
    die "download deploy/release/nginx.conf"
  sed -e "s|__LISTEN__|$listen|g" \
      -e "s|__PUBLIC_HOST__|$host|g" \
      -e "s|__S3_HOST__|$s3_host|g" "$tmp" > nginx/orrery.conf
  rm -f "$tmp"
  chmod 0644 nginx/orrery.conf

  # The Control Plane application settings live here (ADR-0024). The server
  # container runs as uid 10001 and reads the directory read-only.
  cat > config/config.yaml <<EOF
# Deployment configuration (docs/spec/defaults.md). Edit this file and run
# docker compose up -d; upgrades never overwrite it. Secrets are inline: the
# file is 0600 and must stay out of backups you do not protect.
database:
  host: postgres
  port: 5432
  user: orrery
  password: $pg
  name: orrery

access:
  # OAuth/OIDC callbacks and the Sandbox's ORRERY_URL.
  url: $access_url
  # Wildcard domain for browser port forwarding; empty leaves it off.
  apps_domain: ""
  ticket_key: $(rand 32)

s3:
  # The Control Plane reaches the bundled object store on the Compose network;
  # presigned URLs use the public endpoint. Clear endpoint to disable it.
  public_endpoint: $s3_url
  secret_key: $s3_secret

registry:
  addr: $registry_addr
  username: orrery
  password: $registry_password

encryption_keys:
  - {id: k1, key: $(openssl rand -base64 32)}

# Git OAuth applications (defaults.md):
# git_providers:
#   - {id: gitea, type: gitea, client_id: ..., client_secret: ...,
#      base_url: https://git.example.com, scopes: [read:user]}

# oidc:
#   issuer_url: https://idp.example.com
#   client_id: ...
#   client_secret: ...
#   email_domains: []
#   allow_signups: false

# A Sandbox needs a Platform Snapshot, referenced by digest:
# platform_snapshots:
#   - {name: default, ref: docker.io/library/ubuntu:24.04@sha256:..., default: true}
EOF
  chown 10001:10001 tls/server.key config config/config.yaml
  chmod 700 config
  chmod 600 tls/server.key config/config.yaml registry/config.yml registry/htpasswd

  # Compose interpolation only; application settings are in config/config.yaml.
  cat > .env <<EOF
# Written once by install.sh (ADR-0023, ADR-0024); these values are read by
# Compose and install.sh before the server starts. Application settings live
# in config/config.yaml.
ORRERY_VERSION=$version
# The server reads its application settings from this file (ADR-0024).
ORRERY_CONFIG_FILE=/config/config.yaml
ORRERY_PUBLIC_HOST=$host
ORRERY_PUBLIC_PORT=$port
ORRERY_PUBLIC_BIND=$public_bind
ORRERY_PUBLIC_RESOLVE=$resolve
ORRERY_S3_PUBLIC_HOST=$s3_host
# The server, registry and object store stay on loopback behind nginx.
ORRERY_BIND=127.0.0.1:
ORRERY_POSTGRES_PASSWORD=$pg
ORRERY_S3_ACCESS_KEY=orrery
ORRERY_S3_SECRET_KEY=$s3_secret
EOF
  chmod 600 .env
}

# remove_updater removes the auto-updater an older install.sh --auto-update
# left behind (ADR-0023, removed by ADR-0028). It runs last: an old updater
# running this script stops itself only after the upgrade succeeded.
remove_updater() {
  case "$1" in
    server)
      [ -f /etc/orrery/updater.yaml ] || [ -f /etc/orrery/updater.sh ] || return 0
      echo "removing the old auto-updater (orrery-updater)"
      docker compose -p orrery-updater -f /etc/orrery/updater.yaml \
        down --remove-orphans 2>/dev/null || true
      rm -f /etc/orrery/updater.yaml /etc/orrery/updater.sh
      ;;
    node)
      [ -f /etc/systemd/system/orrery-update.service ] || [ -f /etc/orrery-agent/updater ] || return 0
      echo "removing the old auto-updater (orrery-update.service)"
      systemctl disable --now orrery-update.service 2>/dev/null || true
      rm -f /etc/systemd/system/orrery-update.service /etc/orrery-agent/updater
      systemctl daemon-reload 2>/dev/null || true
      ;;
    *) die "remove_updater: unknown role $1" ;;
  esac
}

next_steps() {
  local version="$1" etc=/etc/orrery
  local host port bind check_addr registry_addr s3_url s3_host node_host ca_flags="" trust_flags=""
  host="$(env_get ORRERY_PUBLIC_HOST "$etc/.env")"
  port="$(env_get ORRERY_PUBLIC_PORT "$etc/.env")"
  bind="$(env_get ORRERY_PUBLIC_BIND "$etc/.env")"
  bind="${bind%:}"
  { [ -z "$bind" ] || [ "$bind" = "0.0.0.0" ]; } && check_addr=127.0.0.1 || check_addr="$bind"
  # config.yaml is the source of truth for the Node-reachable registry address.
  registry_addr="$(awk '/^registry:/{r=1;next} /^[^ #]/{r=0} r && /^  addr:/{print $2; exit}' \
    "$etc/config/config.yaml")"
  s3_host="$(env_get ORRERY_S3_PUBLIC_HOST "$etc/.env")"
  s3_url="https://$s3_host$(port_suffix "$port")"
  node_host="$host:$port"
  if [ -f "$etc/ca/ca.crt" ]; then
    ca_flags="--ca ca.crt"
    # The Agent fetches blobs from the object store after the redirect, so
    # that host needs the CA as well.
    trust_flags="--trust-registry $registry_addr --trust-registry $s3_host"
  fi
  if [ -n "$bind" ]; then
    cat <<EOF

An external proxy fronts this host. Forward https://$host:$port to
$check_addr:$port and keep Host, SNI and X-Forwarded-Proto.
EOF
  fi
  cat <<EOF

The public entry is https://$host:$port: /v2/ is the registry,
/orrery.node.v1.Node/ is the Node surface, and everything else is the API.
The object store is https://$s3_host.

Next steps:
  1. Create the first admin:
       cd $etc && docker compose exec server orrery-server bootstrap
  2. Issue an Enrollment Packet (valid for 1 hour):
       TOKEN=\$(curl -ks --connect-to "$host:$port:$check_addr:$port" -H 'Content-Type: application/json' \\
         -d '{"username":"admin","password":"...","token":{"name":"ops","scope":"admin"}}' \\
         "https://$host:$port/api/v1/sessions" | sed -n 's/.*"token":"\\([^"]*\\)".*/\\1/p')
       curl -ks --connect-to "$host:$port:$check_addr:$port" -X POST -H "Authorization: Bearer \$TOKEN" \\
         "https://$host:$port/api/v1/enrollment-packets"
  3. On each Node$( [ -n "$ca_flags" ] && echo ", after copying $etc/ca/ca.crt there as ca.crt"):
       curl -fsSL https://raw.githubusercontent.com/$REPO/$version/deploy/install.sh | sudo bash -s -- node \\
         --version $version --server $node_host --packet ore_... $ca_flags $trust_flags
EOF
}

# The Node's runtime bundle (ADR-0028). The functions from here to node()
# only print or copy files, so a script can source install.sh and reuse them
# without installing a Node (scripts/ci-e2e-node.sh).
NODE_BIN=/opt/orrery/bin
NODE_CONF=/etc/orrery-agent
NODE_STATE=/var/lib/orrery-agent
# The bundle comes first: containerd finds shims, and the shims find runc and
# runsc, on PATH before its own directory (spike §1.4).
NODE_PATH="$NODE_BIN:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

# containerd_toml prints the Agent's containerd config. imports = [] keeps
# the host's /etc/containerd/conf.d out, and NRI would remove and rebind the
# host containerd's /var/run/nri/nri.sock (spike §1.2). There are no runtime
# entries: the Agent names runsc and its options on each container.
containerd_toml() {
  cat <<EOF
# Written by install.sh on every run; edits are lost (ADR-0028).
version = 3
root = "$NODE_STATE/containerd"
state = "/run/orrery/containerd/state"
imports = []
disabled_plugins = ["io.containerd.grpc.v1.cri", "io.containerd.cri.v1.runtime", "io.containerd.cri.v1.images", "io.containerd.podsandbox.controller.v1.podsandbox", "io.containerd.nri.v1.nri"]

[grpc]
  address = "/run/orrery/containerd/containerd.sock"

[ttrpc]
  address = "/run/orrery/containerd/containerd.sock.ttrpc"

[debug]
  address = "/run/orrery/containerd/debug.sock"

[plugins."io.containerd.internal.v1.opt"]
  path = "$NODE_STATE/containerd/opt"
EOF
}

# buildkitd_toml HOST... prints buildkitd's config, trusting the Node's CA
# for each registry host (the registry and the object store it redirects to).
buildkitd_toml() {
  echo "# Written by install.sh on every run; edits are lost (ADR-0028)."
  local h
  for h in "$@"; do
    printf '\n[registry."%s"]\n  ca = ["%s/ca.crt"]\n' "$h" "$NODE_CONF"
  done
}

# buildkitd_hosts FILE prints the hosts an earlier buildkitd_toml wrote.
buildkitd_hosts() {
  [ ! -f "$1" ] || sed -n 's/^\[registry\."\(.*\)"\]$/\1/p' "$1"
}

containerd_unit() {
  cat <<EOF
[Unit]
Description=Orrery Agent containerd
After=network.target local-fs.target

[Service]
Environment=PATH=$NODE_PATH
ExecStart=$NODE_BIN/containerd --config $NODE_CONF/containerd.toml
Type=notify
# Shims, and the Sandboxes under them, outlive a containerd restart.
Delegate=yes
KillMode=process
Restart=always
RestartSec=5
LimitNPROC=infinity
LimitCORE=infinity
TasksMax=infinity
OOMScoreAdjust=-999

[Install]
WantedBy=multi-user.target
EOF
}

# Builds do not depend on the Runtime, so a Local Node runs buildkitd too
# (runtime.md §12); its OCI worker does not use containerd.
buildkitd_unit() {
  cat <<EOF
[Unit]
Description=Orrery Agent buildkitd
After=orrery-containerd.service

[Service]
Environment=PATH=$NODE_PATH
RuntimeDirectory=orrery/buildkit
ExecStart=$NODE_BIN/buildkitd --config $NODE_CONF/buildkitd.toml --root $NODE_STATE/buildkit --addr unix:///run/orrery/buildkit/buildkitd.sock --oci-worker=true --containerd-worker=false --oci-worker-net host
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF
}

# agent_unit RUNTIME EXEC_START prints orrery-agent.service.
agent_unit() {
  local deps=network-online.target
  [ "$1" != containerd ] || deps="$deps orrery-containerd.service"
  cat <<EOF
[Unit]
Description=Orrery Node Agent
# Wants=, not Requires=: the Agent reconnects across a containerd restart,
# and Requires= would restart it with containerd and leave it stopped after
# containerd stops and starts (spike §4.3).
Wants=$deps
After=$deps

[Service]
ExecStart=$2
Restart=always
RestartSec=5
# Stop signals reach the Agent only, so Exec Supervisors keep running across
# a restart (ADR-0014); the first SIGTERM exits gracefully.
KillMode=process

[Install]
WantedBy=multi-user.target
EOF
}

# drop_host_daemons WORD... prints the words, space-joined, without
# --containerd and --buildkit and their values, and says on stderr what it
# dropped: install.sh has no mode on the host daemons (ADR-0028), but earlier
# docs had operators pass their sockets. --buildkit= stays: it turns Snapshot
# builds off.
drop_host_daemons() {
  local kept=() dropped=()
  while [ $# -gt 0 ]; do
    case "$1" in
      --buildkit=) kept+=("$1") ;;
      --containerd=* | --buildkit=*) dropped+=("$1") ;;
      --containerd | --buildkit) dropped+=("$1 ${2-}"); [ $# -eq 1 ] || shift ;;
      *) kept+=("$1") ;;
    esac
    shift
  done
  [ ${#dropped[@]} -eq 0 ] ||
    echo "install.sh: dropped ${dropped[*]} from orrery-agent run; the Agent uses its own containerd and buildkitd (ADR-0028)" >&2
  printf '%s' "${kept[*]}"
}

# install_bins SRC DEST copies every file under SRC to DEST with its mode
# and prints the path, relative to DEST, of each one that changed. A changed
# file is replaced by rename, so a running binary is never written in place.
install_bins() {
  local src="$1" dst="$2" rel new
  while IFS= read -r rel; do
    rel="${rel#./}"
    cmp -s "$src/$rel" "$dst/$rel" && continue
    mkdir -p "$(dirname "$dst/$rel")"
    new="$(dirname "$dst/$rel")/.$(basename "$rel").new"
    install -m "$(stat -c %a "$src/$rel")" "$src/$rel" "$new"
    mv -f "$new" "$dst/$rel"
    printf '%s\n' "$rel"
  done < <(cd "$src" && find . -type f)
}

# write_if_changed DEST writes stdin to DEST, mode 0644, and succeeds only
# when that changed DEST.
write_if_changed() {
  local t
  t="$(mktemp "$1.XXXXXX")" || die "cannot write $1"
  cat > "$t" || die "cannot write $1"
  if cmp -s "$t" "$1"; then rm -f "$t"; return 1; fi
  { chmod 0644 "$t" && mv -f "$t" "$1"; } || die "cannot write $1"
}

# start_unit NAME RESTART enables NAME and restarts it when RESTART is 1,
# otherwise only makes sure it runs.
start_unit() {
  systemctl enable --quiet "$1"
  if [ "$2" = 1 ]; then systemctl restart "$1"; else systemctl start "$1"; fi
}

# An Agent before ADR-0028 kept its objects in the host containerd's orrery
# namespace. install.sh never removes it: that containerd may serve Docker.
migration_notice() {
  cat <<'EOF'

The host containerd still has the orrery namespace of an earlier Agent, which
this Agent no longer uses (ADR-0028). install.sh leaves it alone, since that
containerd may serve Docker. Once the Node is undrained and ready, remove it:
  c="ctr -a /run/containerd/containerd.sock -n orrery"
  for t in $($c tasks ls -q); do $c tasks delete -f "$t"; done
  for x in $($c containers ls -q); do $c containers delete "$x"; done
  $c images ls -q | xargs -r $c images rm
  $c leases ls -q | xargs -r $c leases rm
  ctr -a /run/containerd/containerd.sock namespaces remove orrery
EOF
}

probe_image_notice() {
  cat <<'EOF'

The Agent's containerd starts with an empty image store, so its self-check
pulls docker.io/library/alpine:3.22. A Node that cannot reach Docker Hub stays
unavailable until the image is imported, for example from Docker with a
containerd 2.x ctr:
  docker save docker.io/library/alpine:3.22 |
    ctr -a /run/orrery/containerd/containerd.sock -n orrery images import -
EOF
}

node() {
  local version="" server="" packet="" ca="" trust_registry=() runtime="" name="" extra=() write_unit=0
  while [ $# -gt 0 ]; do
    case "$1" in
      --version) version="$2"; shift 2 ;;
      --server) server="$2"; shift 2 ;;
      --packet) packet="$2"; shift 2 ;;
      --ca) ca="$2"; shift 2 ;;
      --trust-registry) trust_registry+=("$2"); shift 2 ;;
      --runtime) runtime="$2"; write_unit=1; shift 2 ;;
      --name) name="$2"; shift 2 ;;
      --auto-update | --no-auto-update) die "auto-update was removed (ADR-0028); upgrade by rerunning install.sh" ;;
      --) shift; extra=("$@"); write_unit=1; break ;;
      *) usage ;;
    esac
  done
  [ "$(id -u)" = 0 ] || die "run as root"
  need curl tar sha256sum systemctl flock cmp
  case "$version" in "" | latest) version="$(latest_version)" ;; esac
  check_version "$version"
  local bin="$NODE_BIN" conf="$NODE_CONF" sd=/etc/systemd/system
  local state="$NODE_STATE/identity.json" unit=/etc/systemd/system/orrery-agent.service exec_start=""
  mkdir -p "$conf"
  # Concurrent install.sh runs share one lock.
  exec 9>"$conf/.install.lock"
  flock -n 9 || die "another install.sh run is in progress"

  # An upgrade without --runtime or extra flags keeps the Agent's flags.
  if [ "$write_unit" = 0 ] && [ -f "$unit" ]; then
    # shellcheck disable=SC2046 # ExecStart is space-joined words
    exec_start="$(drop_host_daemons $(sed -n 's/^ExecStart=//p' "$unit"))"
    runtime="$(sed -n 's/.* --runtime \([^ ]*\).*/\1/p' <<<"$exec_start")"
  fi
  runtime="${runtime:-containerd}"
  case "$runtime" in
    containerd)
      # The Mesh is optional, so a missing module only warns; modprobe also succeeds for a built-in one.
      [ -d /sys/module/wireguard ] || modprobe -q wireguard 2>/dev/null ||
        echo "install.sh: warning: no kernel WireGuard; this Node will not join the Node Mesh" >&2
      ;;
    local) ;;
    *) die "--runtime is containerd or local" ;;
  esac

  local arch
  case "$(uname -m)" in
    x86_64) arch=amd64 ;;
    aarch64 | arm64) arch=arm64 ;;
    *) die "unsupported architecture $(uname -m)" ;;
  esac

  local tmp changed pkg="orrery-node_${version}_linux_${arch}.tar.gz"
  local base="https://github.com/$REPO/releases/download/$version"
  tmp="$(mktemp -d)"
  trap "rm -rf '$tmp'" EXIT
  curl -fsSL "$base/$pkg" -o "$tmp/$pkg"
  curl -fsSL "$base/SHA256SUMS" -o "$tmp/SHA256SUMS"
  (cd "$tmp" && grep "  $pkg\$" SHA256SUMS | sha256sum -c --quiet -) || die "$pkg does not match SHA256SUMS"
  tar -C "$tmp" -xzf "$tmp/$pkg"
  mkdir -p "$bin"
  # The whole bundle, runtimes included; the Agent finds the CLI beside its
  # own executable (runtime.md).
  changed="$(install_bins "$tmp/bin" "$bin")"

  if [ -n "$ca" ]; then
    install -m 0644 "$ca" "$conf/ca.crt"
  fi
  if [ ${#trust_registry[@]} -gt 0 ]; then
    [ -f "$conf/ca.crt" ] || die "--trust-registry needs --ca"
    # The Agent pulls with the host's trust store, which it reads at start;
    # the object store is a redirect target, so it needs the same CA. One CA
    # covers every --trust-registry host.
    if command -v update-ca-certificates >/dev/null; then
      install -D -m 0644 "$conf/ca.crt" /usr/local/share/ca-certificates/orrery-ca.crt
      update-ca-certificates >/dev/null
    elif command -v update-ca-trust >/dev/null; then
      install -D -m 0644 "$conf/ca.crt" /etc/pki/ca-trust/source/anchors/orrery-ca.crt
      update-ca-trust extract
    else
      die "--trust-registry needs update-ca-certificates or update-ca-trust"
    fi
  else
    # A rerun without --trust-registry keeps the hosts buildkitd trusts.
    mapfile -t trust_registry < <(buildkitd_hosts "$conf/buildkitd.toml")
  fi
  local ca_flag=()
  [ -f "$conf/ca.crt" ] && ca_flag=(--ca "$conf/ca.crt")

  if [ ! -f "$state" ]; then
    [ -n "$server" ] && [ -n "$packet" ] || die "first install needs --server and --packet"
    "$bin/orrery-agent" enroll --server "$server" --packet "$packet" "${ca_flag[@]}" ${name:+--name "$name"}
  fi

  # A runtime unit restarts only when its binary, config or unit changed;
  # shims, runc and runsc are found per container, so they need no restart.
  local reload=0 restart_ctrd=0 restart_bk=0 new_store=0
  if [ "$runtime" = containerd ]; then
    [ -d "$NODE_STATE/containerd" ] || new_store=1
    if containerd_toml | write_if_changed "$conf/containerd.toml"; then restart_ctrd=1; fi
    if containerd_unit | write_if_changed "$sd/orrery-containerd.service"; then restart_ctrd=1; reload=1; fi
    if grep -qx containerd <<<"$changed"; then restart_ctrd=1; fi
  fi
  if buildkitd_toml "${trust_registry[@]}" | write_if_changed "$conf/buildkitd.toml"; then restart_bk=1; fi
  if buildkitd_unit | write_if_changed "$sd/orrery-buildkitd.service"; then restart_bk=1; reload=1; fi
  if grep -qx buildkitd <<<"$changed"; then restart_bk=1; fi
  [ -n "$exec_start" ] || exec_start="$(drop_host_daemons "$bin/orrery-agent" run --runtime "$runtime" "${ca_flag[@]}" "${extra[@]}")"
  if agent_unit "$runtime" "$exec_start" | write_if_changed "$unit"; then reload=1; fi
  [ "$reload" = 0 ] || systemctl daemon-reload

  [ "$runtime" != containerd ] || start_unit orrery-containerd "$restart_ctrd"
  start_unit orrery-buildkitd "$restart_bk"
  systemctl enable --quiet orrery-agent
  systemctl restart orrery-agent
  # Records the installed version after a successful restart.
  printf '%s\n' "$version" > "$conf/version"
  chmod 0644 "$conf/version"
  echo "Node Agent $version is running: systemctl status orrery-agent"
  remove_updater node
  if [ "$runtime" = containerd ]; then
    [ "$new_store" = 0 ] || probe_image_notice
    if [ -S /run/containerd/containerd.sock ] && command -v ctr >/dev/null &&
      ctr -a /run/containerd/containerd.sock namespaces ls -q 2>/dev/null | grep -qx orrery; then
      migration_notice
    fi
  fi
  return 0
}

# Sourced, install.sh only defines its functions.
(return 0 2>/dev/null) && return 0

case "${1:-}" in
  server) shift; server "$@" ;;
  node) shift; node "$@" ;;
  *) usage ;;
esac
