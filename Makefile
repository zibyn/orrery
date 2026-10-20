GO ?= go

.PHONY: build test vet e2e e2e-console sqlc proto web web-check console

build:
	$(GO) build ./...

web:
	cd web && pnpm install --frozen-lockfile && pnpm build

web-check:
	cd web && pnpm install --frozen-lockfile && pnpm typecheck && pnpm test

# Plain `go build` serves the committed placeholder; this embeds the real
# console (api.md §8).
console: web
	$(GO) build -tags embed ./...

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

e2e:
	./scripts/e2e.sh

# Playwright against the same Compose deployment as `make e2e`; the script
# enrolls a local Node Agent so a Sandbox can reach ready.
e2e-console:
	ORRERY_E2E_CONSOLE=1 ./scripts/e2e.sh

sqlc:
	sqlc generate

proto:
	buf generate
