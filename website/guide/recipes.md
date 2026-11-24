# Real-World Recipes (Zero Black Boxes)

All recipes below use pure Yak HCL primitives—copy them into `services/*.hcl` and adapt every flag freely.

## PostgreSQL 16 (`services/postgres.hcl`)

Requires isolated SysV IPC (`sysvshm = "new"`, etc.), `8k` ZFS recordsize, and `postgres` UID `770`:

```hcl
storage "pg_data" {
  size                   = "20G"
  snapshot_before_deploy = true
  zfs = {
    recordsize  = "8k"
    compression = "lz4"
  }
}

image "postgres16" {
  runtime {
    packages = ["postgresql16-server"]
  }
}

service "db" {
  image    = image.postgres16
  strategy = "recreate"

  jail = {
    sysvshm = "new"
    sysvsem = "new"
    sysvmsg = "new"
  }

  mount {
    source = storage.pg_data
    target = "/var/db/postgres"
    owner  = "770:770"
  }

  env = {
    POSTGRES_PASSWORD = secret.postgres_password
  }

  init_cmd = "test -f /var/db/postgres/data16/PG_VERSION || su -m postgres -c '/usr/local/bin/initdb -D /var/db/postgres/data16 -E UTF8 --auth-host=scram-sha-256'"
  cmd      = "su -m postgres -c '/usr/local/bin/postgres -D /var/db/postgres/data16 -c listen_addresses=*'"
  port     = 5432

  healthcheck {
    cmd      = "su -m postgres -c '/usr/local/bin/pg_isready -q'"
    interval = "2s"
    timeout  = "20s"
  }
}
```

## Redis 7 (`services/redis.hcl`)

```hcl
storage "redis_data" {
  size = "2G"
}

image "redis7" {
  runtime {
    packages = ["redis"]
  }
}

service "cache" {
  image    = image.redis7
  strategy = "recreate"

  mount {
    source = storage.redis_data
    target = "/var/db/redis"
    owner  = "535:535"
  }

  cmd  = "su -m redis -c '/usr/local/bin/redis-server --bind 0.0.0.0 --protected-mode no --dir /var/db/redis --appendonly yes'"
  port = 6379

  healthcheck {
    cmd      = "/usr/local/bin/redis-cli ping"
    interval = "2s"
    timeout  = "10s"
  }
}
```

## Shared Image Across API + Background Worker

Build once in `image "backend"`, then reference `image = image.backend` from both `service "api"` (`strategy = "blue-green"`) and `service "worker"` (`strategy = "recreate"`, `stop_grace = "30s"`). See `examples/go-postgres-redis/Yakfile` in the repository for the complete runnable configuration.
