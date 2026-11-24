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

  resources {
    memory = "1G"
    cpu    = "100%"
  }
}
