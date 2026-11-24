# Yak

**通过 SSH 实现声明式、零黑盒的 FreeBSD Jail + ZFS 部署工具。**

[![CI](https://github.com/blackstorm/bsdyak/actions/workflows/ci.yml/badge.svg)](https://github.com/blackstorm/bsdyak/actions/workflows/ci.yml)
[![License: BSD-2-Clause](https://img.shields.io/badge/License-BSD--2--Clause-blue.svg)](LICENSE)
[![FreeBSD](https://img.shields.io/badge/FreeBSD-14.x-ab2b28.svg)](https://www.freebsd.org/)
[![Go](https://img.shields.io/badge/Go-1.24+-00add8.svg)](https://go.dev/)
[![Docs](https://img.shields.io/badge/docs-bsdyak.com-informational.svg)](https://bsdyak.com)

[English](README.md) | 简体中文 | [官方网站与文档 (bsdyak.com)](https://bsdyak.com)

---

Yak 将客户端与服务端能力整合进**单个静态 Go 二进制文件（`yak`）**。在项目中编写 HCL 风格的 `Yakfile` 描述多阶段构建、ZFS 持久化存储、多服务依赖拓扑与本机端口暴露，即可通过标准 OpenSSH 22 端口一键把 Golang、PostgreSQL、Redis 等完整技术栈部署到 FreeBSD 14.x 宿主机。

## 核心特性

- **100% FreeBSD 原生技术栈**：直接基于 FreeBSD Jail、VNET 独立网络栈、ZFS 数据集/克隆/快照、`pf` 锚点、`rctl` 与官方 `pkg` 生态运行，无任何 Linux 兼容层包袱。
- **零新增公网端口**：控制面不暴露管理端口。客户端通过系统 `ssh` 执行远端 `yak bridge`，经 `yamux` 单连接多路复用接入宿主机 `/var/run/yakd.sock`（`0660 root:yak`）。
- **零黑盒 HCLv2 `Yakfile`**：不内置隐藏的中间件 Preset，所有软件安装、初始化与运行参数均由透明 HCL 原语与 `include` 片段组合而成，行为 100% 可见、可修改。
- **同时支持本地与服务端多阶段构建**：纯 Go 项目可在客户端 `on = "local"` 极速交叉编译；涉及 CGO 或前端多步构建时可设为 `on = "server"` 在服务端隔离 Builder Jail 中编译，并利用 ZFS 内容寻址快照缓存加速二次构建。
- **解耦 Ingress 反代**：不干涉宿主机的 Nginx、Caddy、HAProxy 或 `cloudflared`。通过 `expose` 暴露稳定本机端点（`127.0.0.1:<port>` 或 Unix Socket），Yak 内置极薄 L4 蓝绿切换器，你的 Nginx 无需 reload 即可实现零停机发版。
- **6 个正交 CLI 原语**：`deploy`、`down`、`ps`、`logs`、`exec`、`secret`，全面拥抱 Unix 管道组合。

## 架构概览

```text
开发者机器 (macOS / Linux / FreeBSD)             FreeBSD 14.x 宿主机
┌──────────────────────────────────┐           ┌──────────────────────────────────────┐
│ yak deploy / ps / logs / exec    │  SSH :22  │ yak bridge ──▶ /var/run/yakd.sock    │
│  • HCLv2 Yakfile + include       │ ────────▶ │ yakd 守护进程 (bbolt + 协调器 + GC)  │
│  • 可选本地构建阶段 (on="local") │   yamux   │  ├── ZFS 数据集 / 分层缓存 / 快照    │
│  • 增量 zstd 源码同步            │           │  ├── VNET 项目网桥 + 内置 DNS        │
└──────────────────────────────────┘           │  ├── pf 锚点 (anchor "yak/*")        │
                                               │  ├── 服务 Jail (api / worker / db)   │
  用户 Nginx / Caddy / HAProxy ──▶ 127.0.0.1:18080 ──▶ L4 蓝绿切流器 ─────────────────┘
```

## 60 秒快速上手

### 1. 初始化 FreeBSD 14.x 宿主机（一次性）

```sh
sudo install -m 0755 yak-freebsd-amd64 /usr/local/bin/yak
sudo yak server init --zfs-root zroot/yak --egress vtnet0
sudo yak server check
sudo service yakd enable && sudo service yakd start
```

### 2. 在项目根目录创建 `Yakfile`

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

### 3. 注入密钥并一行部署

```sh
echo "s3cr3t" | yak secret set postgres_password
yak deploy --dry-run
yak deploy
```

## 6 个可组合命令行原语

| 命令 | 职责 | Shell 组合示例 |
| :--- | :--- | :--- |
| `yak deploy` | 声明式收敛服务端状态 | `yak deploy api --release r41`（秒级回滚） |
| `yak down` | 停止并移除服务 Jail（保留 ZFS 存储卷） | `yak down api` / `yak down` |
| `yak ps` | 输出服务状态、版本号、内网 IP 与暴露端点 | `ssh -L 5432:$(yak ps db --ip):5432 prod` |
| `yak logs` | 流式或尾部读取服务日志（`-f`, `-n`） | `yak logs api -n 500 \| grep ERROR` |
| `yak exec` | 在目标 Jail 内执行命令（支持 `-it` 交互） | `yak exec db -- pg_dump -U shop shop > backup.sql` |
| `yak secret` | 管理服务端密钥（`set` / `unset` / `ls`） | `op read ... \| yak secret set jwt_secret` |

## 从源码构建

```sh
make ci
make build
make build-freebsd
```

## 完整文档

完整教程、HCL Schema 手册、ZFS 数据集规范与实战范例请访问官网 **[https://bsdyak.com](https://bsdyak.com)**。

- [架构设计规范](docs/architecture.md)
- [示例项目](examples/)
- [贡献指南](CONTRIBUTING.md)
- [安全模型与漏洞报告](SECURITY.md)

## 开源协议

本项目基于 [BSD-2-Clause](LICENSE) 协议开源。
