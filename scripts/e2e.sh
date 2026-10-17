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
COMPOSE=(docker compose -f "$COMPOSE_FILE" -p "$COMPOSE_PROJECT")

# The node listener always uses TLS (ADR-0014); a throwaway self-signed cert is
# enough for the black-box tests.
mkdir -p .e2e/tls
openssl req -x509 -newkey rsa:2048 -nodes -days 1 \
  -subj "/CN=localhost" \
  -addext "subjectAltName=DNS:localhost,IP:127.0.0.1" \
  -keyout .e2e/tls/server.key -out .e2e/tls/server.crt >/dev/null 2>&1
# The server container runs as an unprivileged user; this is a throwaway cert.
chmod 644 .e2e/tls/server.key .e2e/tls/server.crt
export STARS_NODE_TLS_DIR="$PWD/.e2e/tls"

# The probe container runs the Agent binary as the Supervisor, so it has to be
# static.
mkdir -p .e2e
CGO_ENABLED=0 "$GO" build -o .e2e/stars-sandbox-agent ./cmd/stars-sandbox-agent
"$GO" build -o .e2e/stars-sandbox ./cmd/stars-sandbox

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

DOCKER_SOCKET=""
case "${DOCKER_HOST:-}" in
  unix://*) DOCKER_SOCKET="${DOCKER_HOST#unix://}" ;;
esac

STARS_E2E_API_URL="$API_URL" \
STARS_E2E_CLI="$PWD/.e2e/stars-sandbox" \
STARS_E2E_AGENT="$PWD/.e2e/stars-sandbox-agent" \
STARS_E2E_NODE_URL="$NODE_URL" \
STARS_E2E_NODE_CA="$PWD/.e2e/tls/server.crt" \
STARS_E2E_DOCKER_SOCKET="$DOCKER_SOCKET" \
STARS_E2E_COMPOSE_FILE="$COMPOSE_FILE" \
STARS_E2E_COMPOSE_PROJECT="$COMPOSE_PROJECT" \
"$GO" test -tags e2e -count=1 ./e2e/...
