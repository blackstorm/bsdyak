# Local & Server Multi-Stage Builds

Yak supports both **local cross-compilation (`on = "local"`)** and **server-side multi-stage Builder Jails (`on = "server"`)** inside the same `image` syntax.

## Choosing `on = "local"` vs `on = "server"`

| Scenario | Recommended `on` | Why |
| :--- | :--- | :--- |
| Pure Go (`CGO_ENABLED=0`) | `"local"` | Compiles in seconds on your laptop; uploads only the final FreeBSD binary. |
| Go with CGO (`sqlite3`, `vips`), Node frontend + Go embed, Rust | `"server"` | Compiles directly against native FreeBSD 14 headers and `.so` libraries. |

## Hybrid Multi-Stage Example

```hcl
image "backend" {
  stage "web" {
    on       = "server"
    packages = ["node22", "npm"]
    cache    = ["/root/.npm"]
    workdir  = "/src/web"
    steps    = ["npm ci", "npm run build"]
  }

  stage "go-build" {
    on       = "server"
    packages = ["go123", "pkgconf", "vips"]
    cache    = ["/go/pkg/mod", "/root/.cache/go-build"]
    copy {
      from = stage.web
      src  = "/src/web/dist"
      dst  = "/src/internal/webui/dist"
    }
    env = {
      CGO_ENABLED = "1"
    }
    steps = [
      "go generate ./...",
      "go build -trimpath -o /out/api ./cmd/api",
    ]
  }

  runtime {
    packages = ["ca_root_nss", "vips", "tzdata"]
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
```

## How ZFS Layer Caching Works

Yak hashes `(freebsd_release, sorted(packages), setup)` into a 16-character layer key (`zroot/yak/layers/<hash>@ready`). Whenever your `packages` and `setup` remain unchanged:
- `pkg install` is **skipped completely**—even during network outages.
- The cached layer snapshot is cloned in milliseconds.

## Private Go Modules (`build_secrets`)

To pass credentials (such as a Git token or `.netrc` token) into a server build stage without storing them in any ZFS layer:

```hcl
stage "go-build" {
  on            = "server"
  build_secrets = ["git_token"]
  steps = [
    "git config --global url.\"https://x-access-token:${git_token}@github.com/\".insteadOf \"https://github.com/\"",
    "go build -o /out/api ./cmd/api",
  ]
}
```
