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

  resources {
    memory = "512M"
  }
}
