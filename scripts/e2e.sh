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

# Warm the cache; the Agent pulls the image itself if this misses.
docker image inspect alpine:3.22 >/dev/null 2>&1 || docker pull alpine:3.22 >/dev/null

# Every Sandbox is created from the deployment default Snapshot digest
# (api.md §3). Prefer the pinned RepoDigest; a locally built image may not have
# one, in which case the tag is the honest fallback.
STARS_DEFAULT_SNAPSHOT_DIGEST="${STARS_DEFAULT_SNAPSHOT_DIGEST:-}"
if [ -z "$STARS_DEFAULT_SNAPSHOT_DIGEST" ]; then
  STARS_DEFAULT_SNAPSHOT_DIGEST="$(docker image inspect alpine:3.22 --format '{{index .RepoDigests 0}}' 2>/dev/null || true)"
fi
if [ -z "$STARS_DEFAULT_SNAPSHOT_DIGEST" ]; then
  STARS_DEFAULT_SNAPSHOT_DIGEST="alpine:3.22"
fi
export STARS_DEFAULT_SNAPSHOT_DIGEST
echo "default Snapshot: $STARS_DEFAULT_SNAPSHOT_DIGEST"

cleanup() {
  "${COMPOSE[@]}" down -v --remove-orphans >/dev/null 2>&1 || true
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
    "scopes": ["read:repository", "write:repository"],
}], indent=2))
PY
export STARS_GIT_PROVIDERS_DIR="$PWD/.e2e/git"
export STARS_GIT_PROVIDERS_FILE=/git/providers.json

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
"$GO" test -tags e2e -count=1 "$@" ./e2e/...
