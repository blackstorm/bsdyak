# Networking & Ingress (`expose`)

Yak gives each project an isolated FreeBSD **VNET network stack** while keeping host HTTP/TLS ingress 100% under your control.

## Per-Project VNET Isolation

- Each project allocates a dedicated `/24` subnet from `10.88.1.0/24` – `10.88.254.0/24` and a bridge `ykb_<id>`.
- Each jail receives a short 11-character `epair` interface (`yk<hex8>a/b`, respecting FreeBSD's 15-char `IFNAMSIZ` cap) with a descriptive label (`shop:api:r42`).
- `yakd` runs an embedded DNS responder on `<subnet>.1:53`, resolving `service.db.host` (`db`) and `db.shop.yak` to the live jail IP.
- `pf` rules live strictly inside `anchor "yak/<project>"` and `nat-anchor "yak/<project>"`, blocking cross-project lateral traffic automatically.

## Exposing Services to Nginx / Caddy / HAProxy

Instead of modifying `/usr/local/etc/nginx/nginx.conf` or owning ports 80/443, a service declares an `expose` block:

```hcl
service "api" {
  expose {
    listen = "127.0.0.1:18080"
    # Or expose a Unix domain socket:
    # unix = "/var/run/yak/shop-api.sock"
    to = 8080
  }
}
```

Yak binds a lightweight protocol-agnostic L4 forwarder on `127.0.0.1:18080`. During a `blue-green` deployment:
1. New release `r42` starts, passes `migrate`, and passes `healthcheck`.
2. Yak atomically swaps the forwarder target from `r41` IP to `r42` IP.
3. Active connections to `r41` drain gracefully; new connections route to `r42`.

Your Nginx config never changes and **never even needs a reload**:

### Nginx Example

```nginx
upstream shop_api {
    server 127.0.0.1:18080;
    keepalive 32;
}

server {
    listen 443 ssl http2;
    server_name api.example.com;

    location / {
        proxy_pass http://shop_api;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection "upgrade";
        proxy_buffering off;
    }
}
```

### Caddy Example

```caddyfile
api.example.com {
    reverse_proxy 127.0.0.1:18080
}
```

## Direct Discovery (`endpoints.json` & `hook`)

For zero-hop direct proxying to jail IPs, read `/var/run/yak/projects/<project>/endpoints.json` or trigger a host reload hook after cutover:

```hcl
hook {
  on_switch = "/usr/local/bin/reload-nginx-upstream.sh"
}
```
