BINARY := yak
PKG := ./cmd/yak
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "0.1.0-dev")
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w -X github.com/blackstorm/bsdyak/internal/version.Version=$(VERSION) \
           -X github.com/blackstorm/bsdyak/internal/version.Commit=$(COMMIT) \
           -X github.com/blackstorm/bsdyak/internal/version.Date=$(DATE)

.PHONY: all build build-freebsd vet test lint test-unit test-integration test-e2e-vm ci ci-full docs-dev docs-build clean

all: build

build:
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o dist/$(BINARY) $(PKG)

build-freebsd:
	@mkdir -p dist
	CGO_ENABLED=0 GOOS=freebsd GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o dist/$(BINARY)-freebsd-amd64 $(PKG)
	CGO_ENABLED=0 GOOS=freebsd GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o dist/$(BINARY)-freebsd-arm64 $(PKG)

vet:
	go vet ./...

test:
	go test -race -v ./internal/... ./tests/integration/...

lint:
	./scripts/ci.sh lint

test-unit:
	./scripts/ci.sh unit

test-integration:
	./scripts/ci.sh integration

test-e2e-vm:
	./scripts/ci.sh e2e-freebsd

ci:
	./scripts/ci.sh all

ci-full:
	./scripts/ci.sh full

docs-dev:
	pnpm --dir website dev

docs-build:
	./scripts/ci.sh docs

clean:
	rm -rf dist/ website/.vitepress/dist website/.vitepress/cache coverage.out
