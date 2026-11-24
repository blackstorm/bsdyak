#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT_DIR}"

stage="${1:-all}"

banner() {
  echo ""
  echo "=== [ci] stage: $1 ==="
}

run_lint() {
  banner "lint"
  unformatted="$(gofmt -l cmd internal tests)"
  if [[ -n "${unformatted}" ]]; then
    echo "gofmt check failed on:" >&2
    echo "${unformatted}" >&2
    exit 1
  fi
  go vet ./...
  bash -n scripts/*.sh
  go test -tags freebsd_e2e -c -o /tmp/yak-e2e-compile.test ./tests/e2e
  rm -f /tmp/yak-e2e-compile.test
}

run_unit() {
  banner "unit"
  go test -race -count=1 -coverprofile=coverage.out ./internal/...
}

run_integration() {
  banner "integration"
  go test -race -count=1 -v ./tests/integration/...
}

run_build_matrix() {
  banner "build-matrix"
  mkdir -p dist
  VERSION="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo '0.1.0-dev')}"
  COMMIT="${COMMIT:-$(git rev-parse --short HEAD 2>/dev/null || echo 'unknown')}"
  DATE="${DATE:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}"
  LDFLAGS="-s -w -X github.com/blackstorm/bsdyak/internal/version.Version=${VERSION} -X github.com/blackstorm/bsdyak/internal/version.Commit=${COMMIT} -X github.com/blackstorm/bsdyak/internal/version.Date=${DATE}"

  CGO_ENABLED=0 GOOS=freebsd GOARCH=amd64 go build -ldflags "${LDFLAGS}" -o dist/yak-freebsd-amd64 ./cmd/yak
  CGO_ENABLED=0 GOOS=freebsd GOARCH=arm64 go build -ldflags "${LDFLAGS}" -o dist/yak-freebsd-arm64 ./cmd/yak
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "${LDFLAGS}" -o dist/yak-linux-amd64 ./cmd/yak
  CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -ldflags "${LDFLAGS}" -o dist/yak-darwin-arm64 ./cmd/yak
  CGO_ENABLED=0 go build -ldflags "${LDFLAGS}" -o dist/yak ./cmd/yak

  ./dist/yak version
  ./dist/yak --help >/dev/null
  ./dist/yak deploy --dry-run --local-validate -F examples/go-postgres-redis/Yakfile
  ./dist/yak deploy --dry-run --local-validate -F examples/go-local-build/Yakfile
}

run_docs() {
  banner "docs"
  pnpm --dir website install
  pnpm --dir website build
}

run_e2e_freebsd() {
  banner "e2e-freebsd"
  ./scripts/e2e-freebsd-vm.sh
}

case "${stage}" in
  lint)
    run_lint
    ;;
  unit)
    run_unit
    ;;
  integration)
    run_integration
    ;;
  build-matrix)
    run_build_matrix
    ;;
  docs)
    run_docs
    ;;
  e2e-freebsd)
    run_e2e_freebsd
    ;;
  all)
    run_lint
    run_unit
    run_integration
    run_build_matrix
    run_docs
    ;;
  full)
    run_lint
    run_unit
    run_integration
    run_build_matrix
    run_docs
    run_e2e_freebsd
    ;;
  *)
    echo "usage: $0 {lint|unit|integration|build-matrix|docs|e2e-freebsd|all|full}" >&2
    exit 2
    ;;
esac
