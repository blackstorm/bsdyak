---
layout: home

hero:
  name: "Yak"
  text: "FreeBSD Jails & ZFS, Deployed Over SSH"
  tagline: "One static Go binary. Zero extra open ports. Zero hidden presets. Compose Go, PostgreSQL, Redis, and ZFS datasets from a declarative HCL Yakfile."
  image:
    src: /logo.svg
    alt: Yak Logo
  actions:
    - theme: brand
      text: Get Started
      link: /guide/quickstart
    - theme: alt
      text: HCL Reference
      link: /reference/hcl-schema
    - theme: alt
      text: GitHub
      link: https://github.com/blackstorm/bsdyak

features:
  - title: SSH Port 22 Only
    details: No control-plane API port exposed to the internet. Clients invoke `yak bridge` over OpenSSH, multiplexing into `/var/run/yakd.sock`.
  - title: Pure FreeBSD 14.x Primitives
    details: Real VNET project bridges, embedded per-project DNS, `pf` anchor isolation, ZFS clones/snapshots, and `daemon(8)` supervision.
  - title: Zero Black-Box Presets
    details: No magical hidden database drivers. Express PostgreSQL, Redis, and Go builds with transparent HCL primitives and `include` files.
  - title: Local & Server Multi-Stage Builds
    details: Cross-compile pure Go in seconds (`on = "local"`) or run CGO/frontend builds in server Builder Jails (`on = "server"`) with cached ZFS layers.
  - title: Ingress Agnostic (`expose`)
    details: Keep using your Nginx, Caddy, or HAProxy. Point it at `127.0.0.1:18080` or a Unix socket while Yak handles L4 blue/green cutover.
  - title: 6 Composable CLI Commands
    details: "`deploy`, `down`, `ps`, `logs`, `exec`, and `secret`—small, scriptable tools built for standard Unix pipes and `ssh -L`."
---

## One `Yakfile`, Zero Magic

```hcl
project "shop" {
  freebsd = "14.2-RELEASE"
}

include = ["services/postgres.hcl", "services/redis.hcl"]

target "prod" {
  host    = "deploy@203.0.113.10"
  default = true
}

service "api" {
  image      = image.backend
  depends_on = [service.db, service.cache]
  strategy   = "blue-green"

  env = {
    DB_HOST    = service.db.host
    REDIS_HOST = service.cache.host
    DB_PASS    = secret.postgres_password
  }

  cmd  = "/app/api"
  port = 8080

  expose {
    listen = "127.0.0.1:18080"
    to     = 8080
  }
}
```

## Built for Unix Composition

```sh
# Preview exact plan without mutating the host
yak deploy --dry-run

# Tunnel straight to internal PostgreSQL via SSH without exposing port 5432
ssh -L 5432:$(yak ps db --ip):5432 deploy@203.0.113.10

# Stream logical backup directly to your laptop
yak exec db -- pg_dump -U shop shop > backup.sql

# Roll back instantly to a previous immutable ZFS snapshot
yak deploy api --release r41
```
