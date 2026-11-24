# Yak

**Declarative, zero-magic FreeBSD Jail & ZFS deployment over SSH.**

[![CI](https://github.com/blackstorm/bsdyak/actions/workflows/ci.yml/badge.svg)](https://github.com/blackstorm/bsdyak/actions/workflows/ci.yml)
[![License: BSD-2-Clause](https://img.shields.io/badge/License-BSD--2--Clause-blue.svg)](LICENSE)
[![FreeBSD](https://img.shields.io/badge/FreeBSD-14.x-ab2b28.svg)](https://www.freebsd.org/)
[![Go](https://img.shields.io/badge/Go-1.24+-00add8.svg)](https://go.dev/)
[![Docs](https://img.shields.io/badge/docs-bsdyak.com-informational.svg)](https://bsdyak.com)

[Website & Docs (bsdyak.com)](https://bsdyak.com)

---

Yak packages a single static Go binary (`yak`) for both your workstation and your FreeBSD 14.x server. Describe your services, ZFS storage datasets, multi-stage builds, and local port exposures in an HCL `Yakfile`, then deploy an entire stack (such as Go + PostgreSQL + Redis) over standard OpenSSH port 22.

## Why Yak?

- **100% FreeBSD Native**: Built directly on FreeBSD Jails, VNET virtual network stacks, ZFS datasets/clones/snapshots, `pf` anchors, `rctl`, and official `pkg` repositories—no Linuxulator hacks, no Docker daemon.
- **Zero New Public Ports**: Yak never opens a management API port. Clients invoke `yak bridge` over standard SSH (`:22`), multiplexing requests via `yamux` into the host `/var/run/yakd.sock` (`0660 root:yak`).
- **Transparent HCLv2 `Yakfile` (Zero Black-Box Presets)**: No magic database templates hidden inside the binary. Compose services cleanly using pure HCL primitives and `include` files you can read and modify.
- **Local & Server Multi-Stage Builds**: Cross-compile pure Go locally (`on = "local"`) or run multi-stage CGO/frontend builds inside ephemeral server-side Builder Jails (`on = "server"`) backed by content-addressed ZFS cache layers.
- **Ingress-Agnostic Zero-Downtime Cutover**: Yak leaves ports 80/443 and TLS entirely to your existing Nginx, Caddy, HAProxy, or `cloudflared`. Services declare `expose` on `127.0.0.1:<port>` or a Unix socket; Yak swaps backends atomically with graceful connection draining.
- **Six Composable CLI Primitives**: `deploy`, `down`, `ps`, `logs`, `exec`, `secret`—designed for standard Unix shell pipelines.

## Architecture

```text
Workstation (macOS / Linux / FreeBSD)          FreeBSD 14.x Server
┌──────────────────────────────────┐           ┌──────────────────────────────────────┐
│ yak deploy / ps / logs / exec    │  SSH :22  │ yak bridge ──▶ /var/run/yakd.sock    │
│  • HCLv2 Yakfile + include       │ ────────▶ │ yakd Daemon (bbolt + reconciler + GC)│
│  • Optional local build stages   │   yamux   │  ├── ZFS datasets, layers, snapshots │
│  • Incremental zstd source sync  │           │  ├── VNET bridge + embedded DNS      │
└──────────────────────────────────┘           │  ├── pf anchors (anchor "yak/*")     │
                                               │  ├── Service Jails (api/worker/db)   │
  Your Nginx / Caddy / HAProxy ──▶ 127.0.0.1:18080 ──▶ L4 Blue/Green Switcher ────────┘
```

## Quickstart (60 Seconds)

### 1. Prepare your FreeBSD 14.x Host (once)

```sh
# Install the static binary on FreeBSD
sudo install -m 0755 yak-freebsd-amd64 /usr/local/bin/yak

# Initialize Yak directories, yak group, ZFS root (zroot/yak), and rc.d script
sudo yak server init --zfs-root zroot/yak --egress vtnet0

# Verify host readiness and inspect suggested pf.conf / rc.conf snippets
sudo yak server check

# Start yakd
sudo service yakd enable
sudo service yakd start
```

### 2. Define `Yakfile` in Your Project

```hcl
project "shop" {
  freebsd = "14.2-RELEASE"
}

include = [
  "services/postgres.hcl",
  "services/redis.hcl",
]

target "prod" {
  host    = "deploy@203.0.113.10"
  default = true
}

storage "uploads" {
  size                   = "20G"
  snapshot_before_deploy = true
}

image "backend" {
  stage "go-build" {
    on       = "server"
    packages = ["go123", "pkgconf"]
    cache    = ["/go/pkg/mod", "/root/.cache/go-build"]
    steps = [
      "go build -trimpath -o /out/api ./cmd/api",
      "go build -trimpath -o /out/worker ./cmd/worker",
    ]
  }

  runtime {
    packages = ["ca_root_nss", "tzdata"]
    setup    = ["pw useradd -n app -m -s /usr/sbin/nologin"]
    copy {
      from = stage.go-build
      src  = "/out/*"
      dst  = "/app/"
    }
    user    = "app"
    workdir = "/app"
  }
}

service "api" {
  image      = image.backend
  depends_on = [service.db, service.cache]
  strategy   = "blue-green"

  mount {
    source = storage.uploads
    target = "/app/uploads"
  }

  env = {
    DB_HOST    = service.db.host
    REDIS_HOST = service.cache.host
    DB_PASS    = secret.postgres_password
  }

  cmd  = "/app/api"
  port = 8080

  healthcheck {
    http     = "/healthz"
    interval = "2s"
    timeout  = "15s"
  }

  expose {
    listen = "127.0.0.1:18080"
    to     = 8080
  }
}
```

### 3. Deploy & Compose

```sh
# Store secrets on the host (injected only via RAM tmpfs, never ZFS snapshots)
echo "s3cr3t" | yak secret set postgres_password

# Preview planned changes
yak deploy --dry-run

# Deploy the stack
yak deploy

# Point your host Nginx/Caddy at 127.0.0.1:18080 once; blue/green deploys need zero Nginx reloads
```

## Six Orthogonal CLI Commands

| Command | Purpose | Unix Composition Examples |
| :--- | :--- | :--- |
| `yak deploy` | Reconcile services (`--dry-run`, `--release r<N>`) | `yak deploy api --release r41` (instant rollback) |
| `yak down` | Stop & remove service jails (keeps `storage` datasets) | `yak down api` / `yak down` |
| `yak ps` | Inspect status, active release, jail IP, and exposed socket | `ssh -L 5432:$(yak ps db --ip):5432 prod` |
| `yak logs` | Stream or tail service output (`-f`, `-n`) | `yak logs api -n 500 \| grep ERROR` |
| `yak exec` | Execute commands inside a service jail (`-it` supported) | `yak exec db -- pg_dump -U shop shop > backup.sql` |
| `yak secret` | Manage host-side project secrets (`set`, `unset`, `ls`) | `op read ... \| yak secret set jwt_secret` |

## Building from Source

```sh
make vet test
make build
make build-freebsd
```

## Documentation

Full guides, HCL schema reference, ZFS layout details, and production recipes are published at **[https://bsdyak.com](https://bsdyak.com)**.

- [Architecture Specification](docs/architecture.md)
- [Examples](examples/)
- [Contributing Guide](CONTRIBUTING.md)
- [Security Policy](SECURITY.md)

## License

Yak is released under the [BSD-2-Clause License](LICENSE).
