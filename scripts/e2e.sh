#!/usr/bin/env bash
# One command brings up PostgreSQL and the Control Plane, runs the black-box
# e2e scenarios, and tears everything down (including the volume).
set -euo pipefail
cd "$(dirname "$0")/.."

GO="${GO:-}"
if [ -z "$GO" ]; then
  GO="$(command -v go || true)"
fi
if [ -z "$GO" ]; then
  for candidate in "$HOME/.local/go/bin/go" /usr/local/go/bin/go; do
    if [ -x "$candidate" ]; then GO="$candidate"; break; fi
  done
fi
if [ -z "$GO" ]; then
  echo "go not found; install Go 1.25 or set GO=/path/to/go" >&2
  exit 1
fi
export PATH="$(dirname "$GO"):$PATH"
# A clean machine has a writable GOPATH; this keeps the script working on hosts
# where ~/go is not writable.
if [ -z "${GOPATH:-}" ] && ! [ -w "$("$GO" env GOPATH)" ]; then
  export GOPATH="$HOME/.local/gopath"
fi

API_URL="http://127.0.0.1:18080"
NODE_URL="127.0.0.1:18081"
COMPOSE_FILE="$PWD/deploy/compose.yaml"
COMPOSE_PROJECT="stars-sandbox-e2e"
COMPOSE=(docker compose --profile oidc --profile e2e -f "$COMPOSE_FILE" -p "$COMPOSE_PROJECT")

# wait_for_http polls a URL until it answers the expected status, then leaves a
# last-status diagnostic on failure.
wait_for_http() {
  local url="$1" want="$2" name="$3" code=""
  for _ in $(seq 1 120); do
    code="$(curl -s -o /dev/null -w '%{http_code}' "$url" || true)"
    [ "$code" = "$want" ] && return 0
    sleep 1
  done
  echo "$name did not become ready (last status: ${code:-none})" >&2
  return 1
}

# The e2e Agents copy their binaries into every Sandbox data root. On hosts
# where /tmp is a small quota'd tmpfs that exhausts quickly, keep the test
# directories on the regular filesystem instead.
export TMPDIR="${STARS_E2E_TMPDIR:-/var/tmp/${COMPOSE_PROJECT}-$$}"
mkdir -p "$TMPDIR"

# Object store and registry (defaults.md, ADR-0014). The Control Plane reaches
# RustFS as http://rustfs:9000 inside Compose; presigned URLs and the tests use
# the host-mapped http://127.0.0.1:19000. The registry listens on host port
# 15000 because it signs redirects for the Node-reachable address.
export STARS_S3_ENDPOINT="${STARS_S3_ENDPOINT:-http://rustfs:9000}"
export STARS_S3_PUBLIC_ENDPOINT="${STARS_S3_PUBLIC_ENDPOINT:-http://127.0.0.1:19000}"
export STARS_S3_BUCKET="${STARS_S3_BUCKET:-stars}"
export STARS_S3_REGION="${STARS_S3_REGION:-us-east-1}"
export STARS_S3_ACCESS_KEY="${STARS_S3_ACCESS_KEY:-stars}"
export STARS_S3_SECRET_KEY="${STARS_S3_SECRET_KEY:-stars-secret}"
export STARS_REGISTRY_ADDR="${STARS_REGISTRY_ADDR:-127.0.0.1:15000}"

# The node listener always uses TLS (ADR-0014); a throwaway self-signed cert is
# enough for the black-box tests. Backdate it: a remote Node whose clock is a
# few seconds behind would otherwise reject it as "not yet valid".
mkdir -p .e2e/tls
openssl req -x509 -newkey rsa:2048 -nodes -days 1 \
  -not_before 20200101000000Z \
  -subj "/CN=localhost" \
  -addext "subjectAltName=DNS:localhost,IP:127.0.0.1" \
  -keyout .e2e/tls/server.key -out .e2e/tls/server.crt >/dev/null 2>&1
# The server container runs as an unprivileged user; this is a throwaway cert.
chmod 644 .e2e/tls/server.key .e2e/tls/server.crt
export STARS_NODE_TLS_DIR="$PWD/.e2e/tls"

# Secrets are encrypted at rest (ADR-0013); the e2e deployment gets its own
# throwaway keyring file.
mkdir -p .e2e/keys
printf 'e2e %s\n' "$(head -c 32 /dev/urandom | base64)" > .e2e/keys/encryption
chmod 644 .e2e/keys/encryption
export STARS_ENCRYPTION_KEYS_DIR="$PWD/.e2e/keys"
export STARS_ENCRYPTION_KEYS_FILE=/keys/encryption

# The probe container runs the Agent binary as the Supervisor, so it has to be
# static. The external CLI is copied into Sandboxes, so it has to be static too
# (alpine and distroless images do not have this host's libc).
mkdir -p .e2e
CGO_ENABLED=0 "$GO" build -o .e2e/stars-sandbox-agent ./cmd/stars-sandbox-agent
CGO_ENABLED=0 "$GO" build -o .e2e/stars-sandbox ./cmd/stars-sandbox

# A small Exec log cap keeps the truncation scenario fast, and a short PTY
# idle window keeps the pty-idle scenario fast. A short admission deadline
# keeps the queue-and-deadline scenario fast.
export STARS_EXEC_LOG_CAP="${STARS_EXEC_LOG_CAP:-1048576}"
export STARS_PTY_IDLE_SECONDS="${STARS_PTY_IDLE_SECONDS:-10}"
export STARS_ADMISSION_TIMEOUT="${STARS_ADMISSION_TIMEOUT:-10s}"
# The retention scenario (#90) observes a real sweep without waiting for the
# documented 10-minute cadence and 30-day TTL. Production defaults are
# untouched; these are e2e-only overrides.
export STARS_RESOURCE_VERSION_TTL="${STARS_RESOURCE_VERSION_TTL:-1s}"
# Test hook only: production uses the 10-minute constant (defaults.md).
export STARS_TEST_PURGE_INTERVAL="${STARS_TEST_PURGE_INTERVAL:-1s}"
# The Gateway tests reach port subdomains with a Host header, so no DNS entry
# is needed; only the domain the server signs into tickets matters.
export STARS_APPS_DOMAIN="${STARS_APPS_DOMAIN:-apps.test}"
# GPU scenarios are skipped unless a GPU host is reachable over SSH:
#   STARS_E2E_GPU_SSH=user@gpu-host ./scripts/e2e.sh -run GPU
# The test deploys the Agent there and tunnels back to this machine's node
# listener. STARS_E2E_GPU_REMOTE_PORT overrides the remote tunnel port
# (default 18081).

# STARS_URL inside a Sandbox must reach the Control Plane. Published ports bind
# on the host, so the Docker bridge gateway is the address a Sandbox can use.
HOST_GATEWAY="$(docker network inspect bridge --format '{{(index .IPAM.Config 0).Gateway}}' 2>/dev/null || true)"
HOST_GATEWAY="${HOST_GATEWAY:-172.17.0.1}"
if [ -z "${STARS_ACCESS_URL:-}" ]; then
  export STARS_ACCESS_URL="http://${HOST_GATEWAY}:18080"
fi

# The Control Plane is a container: it cannot push to the registry through
# 127.0.0.1, so imports use the host gateway. Nodes still pull from
# 127.0.0.1:15000. The one credential document lists both hosts
# (deploy/registry/htpasswd holds the same user and password).
export STARS_REGISTRY_PUSH_ADDR="${STARS_REGISTRY_PUSH_ADDR:-${HOST_GATEWAY}:15000}"
# Blob redirects must be reachable from both the host (Agents) and the
# Control Plane container, so sign them for the bridge gateway.
export STARS_REGISTRY_S3_REGIONENDPOINT="${STARS_REGISTRY_S3_REGIONENDPOINT:-http://${HOST_GATEWAY}:19000}"
REGISTRY_AUTH="c3RhcnM6c3RhcnMtcmVnaXN0cnktcGFzc3dvcmQ="
export STARS_REGISTRY_CREDENTIAL="${STARS_REGISTRY_CREDENTIAL:-{\"auths\":{\"127.0.0.1:15000\":{\"auth\":\"${REGISTRY_AUTH}\"},\"${HOST_GATEWAY}:15000\":{\"auth\":\"${REGISTRY_AUTH}\"}}}}"

# OIDC e2e: Dex is the deployment's one external issuer (ADR-0020). Its issuer
# is the Compose service name, which the Control Plane resolves; the redirect
# URIs use the host gateway because STARS_ACCESS_URL also does (defaults.md).
export STARS_OIDC_ISSUER_URL="${STARS_OIDC_ISSUER_URL:-http://dex:5556/dex}"
export STARS_OIDC_CLIENT_ID="${STARS_OIDC_CLIENT_ID:-stars-sandbox}"
export STARS_OIDC_ALLOW_SIGNUPS="${STARS_OIDC_ALLOW_SIGNUPS:-false}"
export STARS_OIDC_ACCESS_URL="${STARS_OIDC_ACCESS_URL:-http://${HOST_GATEWAY}:18082}"
mkdir -p .e2e/oidc .e2e/dex
printf 'stars-sandbox-oidc-secret' > .e2e/oidc/client
chmod 644 .e2e/oidc/client
export STARS_OIDC_SECRET_DIR="$PWD/.e2e/oidc"
export STARS_OIDC_CLIENT_SECRET_FILE=/oidc/client
sed -e "s|__MAIN_CALLBACK__|${STARS_ACCESS_URL%/}/api/v1/oidc/callback|" \
    -e "s|__OIDC_CALLBACK__|${STARS_OIDC_ACCESS_URL%/}/api/v1/oidc/callback|" \
    deploy/dex.yaml > .e2e/dex/dex.yaml
export STARS_DEX_DIR="$PWD/.e2e/dex"

# Every Sandbox without an explicit snapshot runs the deployment default
# platform Snapshot (api.md §3, defaults.md). The Control Plane imports it into
# the platform registry at startup. The source is the local unauthenticated
# registry-source service, so the run never depends on Docker Hub availability
# or anonymous rate limits; the image is pushed there below, before the server
# starts.
if ! docker image inspect alpine:3.22 >/dev/null 2>&1; then
  docker pull alpine:3.22 >/dev/null
fi
export STARS_DEFAULT_SNAPSHOT="${STARS_DEFAULT_SNAPSHOT:-default}"

cleanup() {
  "${COMPOSE[@]}" down -v --remove-orphans >/dev/null 2>&1 || true
  rm -rf "$TMPDIR" >/dev/null 2>&1 || true
}
trap cleanup EXIT

# A killed previous run leaves a volume whose postgres-init.sql never ran, so
# stars_oidc would be missing. Start from a clean volume every time.
"${COMPOSE[@]}" down -v --remove-orphans >/dev/null 2>&1 || true

# Gitea is the e2e Git backend. It needs its administrator, its OAuth2
# application and a user for the browser half of the link flow before the
# Control Plane starts, because the provider file names the application's
# secret (ADR-0013).
export STARS_GITEA_URL="http://${HOST_GATEWAY}:13000"
GITEA_ADMIN="gitea-admin"
GITEA_ADMIN_PASSWORD="gitea-admin-password-123"
GITEA_USER="gitea-user"
GITEA_USER_PASSWORD="gitea-user-password-123"
mkdir -p .e2e/git
"${COMPOSE[@]}" up -d postgres gitea
code=""
for _ in $(seq 1 120); do
  code="$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:13000/api/healthz" || true)"
  [ "$code" = "200" ] && break
  sleep 1
done
if [ "$code" != "200" ]; then
  echo "gitea did not become ready (last status: ${code:-none})" >&2
  "${COMPOSE[@]}" logs --no-color gitea >&2 || true
  exit 1
fi
"${COMPOSE[@]}" exec -T -u git gitea gitea admin user create \
  --username "$GITEA_ADMIN" --password "$GITEA_ADMIN_PASSWORD" \
  --email gitea-admin@example.com --admin --must-change-password=false >/dev/null
"${COMPOSE[@]}" exec -T -u git gitea gitea admin user create \
  --username "$GITEA_USER" --password "$GITEA_USER_PASSWORD" \
  --email gitea-user@example.com --must-change-password=false >/dev/null
GITEA_TOKEN="$("${COMPOSE[@]}" exec -T -u git gitea gitea admin user generate-access-token \
  --username "$GITEA_ADMIN" --token-name e2e --scopes all --raw | tail -n1 | tr -d '\r')"
if [ -z "$GITEA_TOKEN" ]; then
  echo "could not create a Gitea API token" >&2
  exit 1
fi
GITEA_APP="$(curl -fsS -H "Authorization: token $GITEA_TOKEN" -H 'Content-Type: application/json' \
  -d "{\"name\":\"stars-sandbox\",\"redirect_uris\":[\"${STARS_ACCESS_URL%/}/api/v1/external-auth/gitea/callback\"],\"confidential_client\":true,\"skip_secondary_authorization\":true}" \
  "http://127.0.0.1:13000/api/v1/user/applications/oauth2")"
read -r GITEA_CLIENT_ID GITEA_CLIENT_SECRET < <(printf '%s' "$GITEA_APP" |
  python3 -c 'import json, sys; app = json.load(sys.stdin); print(app["client_id"], app["client_secret"])')
if [ -z "$GITEA_CLIENT_ID" ] || [ -z "$GITEA_CLIENT_SECRET" ]; then
  echo "could not create the Gitea OAuth2 application: $GITEA_APP" >&2
  exit 1
fi
printf '%s' "$GITEA_CLIENT_SECRET" > .e2e/git/gitea-secret
# The server container runs as an unprivileged user; this is a throwaway
# client secret for the e2e deployment.
chmod 644 .e2e/git/gitea-secret
python3 - "$GITEA_CLIENT_ID" "$STARS_GITEA_URL" > .e2e/git/providers.json <<'PY'
import json, sys
client_id, base_url = sys.argv[1], sys.argv[2]
print(json.dumps([{
    "id": "gitea",
    "type": "gitea",
    "client_id": client_id,
    "client_secret_file": "/git/gitea-secret",
    "base_url": base_url,
    "scopes": ["read:user", "read:repository", "write:repository"],
}], indent=2))
PY
export STARS_GIT_PROVIDERS_DIR="$PWD/.e2e/git"
export STARS_GIT_PROVIDERS_FILE=/git/providers.json

# RustFS and the platform registry are behind the object-store and image-pull
# scenarios, and the Control Plane imports STARS_PLATFORM_SNAPSHOTS at
# startup, so both must be serving first. The registry answers 401 without
# credentials once it is serving.
"${COMPOSE[@]}" up -d rustfs rustfs-init registry registry-source
if ! wait_for_http "http://127.0.0.1:19000/health" 200 rustfs; then
  "${COMPOSE[@]}" logs --no-color rustfs rustfs-init >&2 || true
  exit 1
fi
if ! wait_for_http "http://127.0.0.1:15000/v2/" 401 registry; then
  "${COMPOSE[@]}" logs --no-color registry >&2 || true
  exit 1
fi
if ! wait_for_http "http://127.0.0.1:15001/v2/" 200 registry-source; then
  "${COMPOSE[@]}" logs --no-color registry-source >&2 || true
  exit 1
fi

# Push the default base image into the local source registry and pin its
# digest. The Control Plane reaches the source by its Compose-network IP: the
# registry client treats RFC1918 addresses as plain HTTP, while a bare service
# name would be tried over HTTPS. Docker Hub is never contacted.
SOURCE_REGISTRY="127.0.0.1:15001"
docker tag alpine:3.22 "${SOURCE_REGISTRY}/alpine:3.22"
docker push "${SOURCE_REGISTRY}/alpine:3.22" >/dev/null
SOURCE_HOST="$(docker inspect --format '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' \
  "${COMPOSE_PROJECT}-registry-source-1" 2>/dev/null || true)"
if [ -z "$SOURCE_HOST" ]; then
  echo "could not find the registry-source container address" >&2
  exit 1
fi
# The pushed single-platform manifest's digest is not the local RepoDigest (a
# multi-arch pull records the index digest), so ask the registry itself.
SOURCE_ACCEPT="application/vnd.oci.image.index.v1+json,application/vnd.oci.image.manifest.v1+json,application/vnd.docker.distribution.manifest.list.v2+json,application/vnd.docker.distribution.manifest.v2+json"
SOURCE_DIGEST="$(curl -fsSI -H "Accept: ${SOURCE_ACCEPT}" \
  "http://${SOURCE_HOST}:5000/v2/alpine/manifests/3.22" 2>/dev/null | tr -d '\r' |
  awk -F': ' 'tolower($1) == "docker-content-digest" { print $2 }')"
if [ -z "$SOURCE_DIGEST" ]; then
  echo "the local source registry did not return a digest for alpine:3.22" >&2
  exit 1
fi
STARS_E2E_ALPINE_REF="${SOURCE_HOST}:5000/alpine@${SOURCE_DIGEST}"
export STARS_E2E_ALPINE_REF
export STARS_PLATFORM_SNAPSHOTS="${STARS_PLATFORM_SNAPSHOTS:-default=${STARS_E2E_ALPINE_REF}}"
echo "platform Snapshot: ${STARS_DEFAULT_SNAPSHOT}=${STARS_E2E_ALPINE_REF}"

"${COMPOSE[@]}" up -d --build

code=""
for _ in $(seq 1 120); do
  code="$(curl -s -o /dev/null -w '%{http_code}' "$API_URL/api/v1/users" || true)"
  if [ "$code" = "401" ]; then break; fi
  sleep 1
done
if [ "$code" != "401" ]; then
  echo "server did not become ready (last status: ${code:-none})" >&2
  "${COMPOSE[@]}" logs --no-color >&2 || true
  exit 1
fi

# The default platform Snapshot is imported in the background at startup; wait
# until it is ready so every Sandbox that omits `snapshot` can be created.
ready=""
for _ in $(seq 1 180); do
  ready="$("${COMPOSE[@]}" exec -T postgres psql -U stars -d stars -tAc \
    "SELECT count(*) FROM snapshot_versions sv JOIN snapshots s ON s.id = sv.snapshot_id
     WHERE s.owner_id IS NULL AND s.name = '${STARS_DEFAULT_SNAPSHOT}' AND sv.state = 'ready'" \
    2>/dev/null | tr -d '[:space:]')"
  [ "$ready" = "1" ] && break
  sleep 1
done
if [ "$ready" != "1" ]; then
  echo "platform Snapshot ${STARS_DEFAULT_SNAPSHOT} did not become ready" >&2
  "${COMPOSE[@]}" logs --no-color server registry >&2 || true
  exit 1
fi

# The OIDC-first-User scenario has its own Control Plane and database.
for _ in $(seq 1 120); do
  code="$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:18082/api/v1/users" || true)"
  [ "$code" = "401" ] && break
  sleep 1
done
if [ "$code" != "401" ]; then
  echo "server-oidc did not become ready (last status: ${code:-none})" >&2
  "${COMPOSE[@]}" logs --no-color >&2 || true
  exit 1
fi
for _ in $(seq 1 60); do
  code="$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:15556/dex/.well-known/openid-configuration" || true)"
  [ "$code" = "200" ] && break
  sleep 1
done
if [ "$code" != "200" ]; then
  echo "dex did not become ready (last status: ${code:-none})" >&2
  "${COMPOSE[@]}" logs --no-color >&2 || true
  exit 1
fi

DOCKER_SOCKET=""
case "${DOCKER_HOST:-}" in
  unix://*) DOCKER_SOCKET="${DOCKER_HOST#unix://}" ;;
esac

STARS_E2E_API_URL="$API_URL" \
STARS_E2E_ACCESS_URL="$STARS_ACCESS_URL" \
STARS_E2E_OIDC_ACCESS_URL="$STARS_OIDC_ACCESS_URL" \
STARS_E2E_OIDC_ISSUER_URL="$STARS_OIDC_ISSUER_URL" \
STARS_E2E_DEX_URL="http://127.0.0.1:15556/dex" \
STARS_E2E_GITEA_URL="$STARS_GITEA_URL" \
STARS_E2E_GITEA_USER="$GITEA_USER" \
STARS_E2E_GITEA_PASSWORD="$GITEA_USER_PASSWORD" \
STARS_E2E_GITEA_TOKEN="$GITEA_TOKEN" \
STARS_E2E_GIT_PROVIDER="gitea" \
STARS_E2E_CLI="$PWD/.e2e/stars-sandbox" \
STARS_E2E_AGENT="$PWD/.e2e/stars-sandbox-agent" \
STARS_E2E_NODE_URL="$NODE_URL" \
STARS_E2E_NODE_CA="$PWD/.e2e/tls/server.crt" \
STARS_E2E_DOCKER_SOCKET="$DOCKER_SOCKET" \
STARS_E2E_COMPOSE_FILE="$COMPOSE_FILE" \
STARS_E2E_COMPOSE_PROJECT="$COMPOSE_PROJECT" \
STARS_E2E_EXEC_LOG_CAP="$STARS_EXEC_LOG_CAP" \
STARS_E2E_PTY_IDLE_SECONDS="$STARS_PTY_IDLE_SECONDS" \
STARS_E2E_S3_ENDPOINT="${STARS_E2E_S3_ENDPOINT:-http://127.0.0.1:19000}" \
STARS_E2E_S3_BUCKET="$STARS_S3_BUCKET" \
STARS_E2E_S3_ACCESS_KEY="$STARS_S3_ACCESS_KEY" \
STARS_E2E_S3_SECRET_KEY="$STARS_S3_SECRET_KEY" \
STARS_E2E_REGISTRY_ADDR="$STARS_REGISTRY_ADDR" \
STARS_E2E_REGISTRY_CREDENTIAL="$STARS_REGISTRY_CREDENTIAL" \
STARS_E2E_ALPINE_REF="$STARS_E2E_ALPINE_REF" \
STARS_E2E_PLATFORM_SNAPSHOT="$STARS_DEFAULT_SNAPSHOT" \
"$GO" test -tags e2e -count=1 "$@" ./e2e/...
