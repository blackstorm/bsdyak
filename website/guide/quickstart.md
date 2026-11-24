# Quickstart

Deploy a multi-service stack (**Go API + Worker + PostgreSQL + Redis + ZFS Storage**) to a FreeBSD 14.x host in under two minutes.

## 1. Install `yak`

Download or build the static binary for your workstation and FreeBSD server:

```sh
# Build from source (Go 1.24+, zero CGO required)
make build
make build-freebsd
```

Install `yak` locally into `/usr/local/bin/yak`, and copy `yak-freebsd-amd64` (or `arm64`) to `/usr/local/bin/yak` on your FreeBSD host.

## 2. Initialize the FreeBSD Host

Run once on your FreeBSD 14.x server (see [Server Setup](/guide/server-setup) for non-root SSH setup):

```sh
sudo yak server init --zfs-root zroot/yak --egress vtnet0
sudo yak server check
sudo service yakd enable
sudo service yakd start
```

## 3. Create a `Yakfile`

Place a `Yakfile` at the root of your repository:

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
    owner  = "app:app"
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

## 4. Set Secrets and Deploy

```sh
# 1. Store secrets on the host (never stored in Git or ZFS snapshots)
echo "strong-db-password" | yak secret set postgres_password

# 2. Preview planned changes
yak deploy --dry-run

# 3. Deploy all services in topological order
yak deploy

# 4. Inspect running jails and internal IPs
yak ps
```

Your host Nginx/Caddy can now proxy `api.example.com` to `127.0.0.1:18080` permanently—subsequent `yak deploy` runs perform atomic blue/green swaps without ever touching Nginx.
