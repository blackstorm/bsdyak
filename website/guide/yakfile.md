# Writing a `Yakfile`

A `Yakfile` uses **HCLv2** syntax with static two-pass symbol validation. References such as `storage.pg_data`, `image.backend`, `service.db.host`, `stage.go-build`, and `secret.db_pass` are checked during parsing—before touching any server.

## Splitting Files with `include`

Keep infrastructure services modular without hiding any behavior behind magic presets:

```hcl
include = [
  "services/postgres.hcl",
  "services/redis.hcl",
]
```

Included paths must stay inside the project directory and cannot form include cycles. Diagnostic messages preserve the exact included filename and line number.

## Multi-Environment `target` Blocks

```hcl
target "staging" {
  host = "deploy@198.51.100.10"
  vars = {
    env_name = "staging"
  }
}

target "prod" {
  host    = "deploy@203.0.113.10"
  default = true
  vars = {
    env_name = "production"
  }
}
```

Deploy to the default target with `yak deploy`, or target staging explicitly with `yak deploy -T staging`.

## Storage Volumes (`storage`)

Each `storage` block provisions a dedicated ZFS dataset under `<zfs_root>/projects/<project>/volumes/<name>`:

```hcl
storage "pg_data" {
  size                   = "20G"    # ZFS quota
  snapshot_before_deploy = true     # Snapshot before each deploy
  zfs = {
    recordsize  = "8k"              # Tuned for PostgreSQL 8KB pages
    compression = "lz4"
  }
}
```

## Service Dependency Ordering

Services declare `depends_on = [service.db, service.cache]`. Yak performs a topological sort (rejecting cycles) and converges dependencies first. When `db` and `cache` configurations have not changed, they remain untouched while `api` rolls out.
