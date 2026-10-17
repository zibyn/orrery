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
COMPOSE_FILE="$PWD/deploy/compose.yaml"
COMPOSE_PROJECT="stars-sandbox-e2e"
COMPOSE=(docker compose -f "$COMPOSE_FILE" -p "$COMPOSE_PROJECT")

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

mkdir -p .e2e
"$GO" build -o .e2e/stars-sandbox ./cmd/stars-sandbox

STARS_E2E_API_URL="$API_URL" \
STARS_E2E_CLI="$PWD/.e2e/stars-sandbox" \
STARS_E2E_COMPOSE_FILE="$COMPOSE_FILE" \
STARS_E2E_COMPOSE_PROJECT="$COMPOSE_PROJECT" \
"$GO" test -tags e2e -count=1 ./e2e/...
