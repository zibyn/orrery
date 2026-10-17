GO ?= go

.PHONY: build test vet e2e sqlc proto web web-check console

build:
	$(GO) build ./...

web:
	cd web && npm ci && npm run build

web-check:
	cd web && npm ci && npm run typecheck && npm test

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

sqlc:
	sqlc generate

proto:
	buf generate
