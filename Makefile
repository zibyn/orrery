GO ?= go

.PHONY: build test vet e2e sqlc proto

build:
	$(GO) build ./...

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
