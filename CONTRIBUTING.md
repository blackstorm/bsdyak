# Contributing to Yak

Thank you for your interest in contributing to **Yak** (`https://bsdyak.com`)!

## Design Principles

Before submitting a feature or pull request, keep Yak's core boundaries in mind:

1. **Zero Magic**: No hidden software presets or opaque runtime rewrites. Express behavior through transparent HCLv2 primitives (`image`, `stage`, `runtime`, `service`, `storage`, `expose`).
2. **Zero New Public Ports**: All control-plane traffic flows over OpenSSH (`:22`) into `yak bridge` and `/var/run/yakd.sock`.
3. **Host Respect**: Never overwrite `/etc/pf.conf`, `/etc/rc.conf`, or `/boot/loader.conf`. Only manage `pf` anchors (`yak/*`) and Yak-owned ZFS datasets.
4. **Pure Go (`CGO_ENABLED=0`)**: All dependencies must compile cleanly with `CGO_ENABLED=0` for `freebsd/amd64`, `freebsd/arm64`, `linux/amd64`, and `darwin/arm64`.
5. **Composable CLI**: Keep user commands minimal and pipe-friendly (`deploy`, `down`, `ps`, `logs`, `exec`, `secret`).

## Development Workflow

Yak uses a single shared pipeline script (`scripts/ci.sh`) across both local `Makefile` targets and `.github/workflows/ci.yml`:

```bash
# Run the full local pipeline (lint + unit + integration + build-matrix + docs)
make ci

# Or run individual stages:
make lint
make test-unit
make test-integration
make docs-build

# Preview documentation site locally
pnpm --dir website install
make docs-dev
```

## Testing Tiers

- **Tier 1 (`make ci` — Linux / macOS / FreeBSD)**: Runs `gofmt`, `go vet`, unit tests (`./internal/...` with `-race`), in-process integration tests (`./tests/integration/...` wiring CLI, HCLv2, `yamux` bridge, `bbolt`, embedded DNS, L4 proxy draining, and `FakeDriver`), static cross-builds, and VitePress site build.
- **Tier 2 (`make test-e2e-vm` — Local & GitHub CI FreeBSD VM via QEMU/KVM)**: `./scripts/ci.sh e2e-freebsd` boots a FreeBSD 14.5 ZFS cloud image under KVM and runs `go test -tags freebsd_e2e -v ./tests/e2e`.

## Pull Requests

- Keep commits focused and descriptive.
- Add tests covering new HCL fields, validation rules, or state machine transitions.
- Update relevant documentation in `website/` when changing CLI flags or `Yakfile` syntax.
