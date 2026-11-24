# Yak Architecture Specification

This document describes the internal design, FreeBSD system invariants, and state transitions of **Yak** (`https://bsdyak.com`).

## 1. Control Plane Over SSH (`:22` Only)

Yak opens **zero additional public TCP or UDP ports** on the FreeBSD host.

```text
Workstation                                  FreeBSD 14.x Host
-----------                                  -----------------
yak deploy                                   sshd (:22)
   │                                            │
   ├── system ssh -T host "yak bridge" ────────▶├── yak bridge [--allow-project=X]
   │                                            │     │
   └── yamux.Client (single SSH stdio pipe) ◀═══╪════▶│ yamux.Server
          ├── Stream 1: POST /sync (zstd tar)   │     │   │
          ├── Stream 2: POST /deploy (SSE)      │     └──▶├── HTTP proxy -> /var/run/yakd.sock
          └── Stream N: GET  /logs?follow=true  │             (0660 root:yak)
                                                │                 │
                                                │             yakd daemon
```

### Why `ssh` + `yak bridge` + `yamux`?
- Inherits user `~/.ssh/config`, `ProxyJump` bastions, custom SSH ports, `ssh-agent`, and hardware keys.
- Requires no `AllowTcpForwarding` or `StreamLocalBindUnlink` sshd relaxation.
- Avoids SSH `MaxStartups` throttling because all concurrent transfers, API requests, log streams, and `exec` sessions share one `yamux` pipe.
- Enables single-project CI key restriction in `~/.ssh/authorized_keys`:
  ```text
  command="/usr/local/bin/yak bridge --allow-project=shop",restrict ssh-ed25519 AAAA...
  ```
  `yak bridge` parses each forwarded HTTP request path `/v1/projects/<project>/...` and rejects any cross-project request with `403 Forbidden`.

## 2. ZFS Dataset Hierarchy

All server state lives under a single configurable parent dataset (`zroot/yak` by default):

```text
zroot/yak/
├── bases/
│   └── 14.2-RELEASE/                 # Extracted official base.txz + snapshot @base
├── layers/
│   └── <sha256-16>/                  # Content-addressed runtime pkg + setup layer (@ready)
├── cache/
│   └── <project>/                    # Persistent builder caches (/go/pkg/mod, /root/.npm, etc.)
└── projects/
    └── <project>/
        ├── source/                   # Incrementally synced workspace
        ├── volumes/
        │   ├── pg_data/              # Storage ZFS dataset (recordsize=8k, quota=20G)
        │   └── uploads/              # Mounted into jail via nullfs
        └── releases/
            └── <service>-r<N>/       # Cloned from layer + built artifacts -> @immutable
                └── root/             # Active jail root cloned from @immutable
```

### Guardrails
- **85% Pool Capacity Guard**: Before cloning or creating datasets during `deploy`, `yakd` checks `zpool list -Hpo capacity`. Deployments abort when pool utilization is `>= 85%` to protect ZFS copy-on-write performance.
- **Secrets Never Touch ZFS**: Host secrets live under `/var/db/yak/secrets/` (`0700`/`0600`) and are mounted into jails strictly via `tmpfs` (`0400`) or RAM-only builder environments. ZFS snapshots and `zfs send` backups never capture secret values.

## 3. VNET Networking & `pf` Anchors

Each project receives an isolated `/24` RFC1918 subnet from `10.88.1.0/24` through `10.88.254.0/24` persisted in `bbolt`:

- **Project Bridge**: `ykb_<hash6>` (<= 10 chars) assigned gateway `<subnet>.1/24`.
- **Service `epair`**: Short interface name `yk<hex8>a` / `yk<hex8>b` (11 chars, safely under FreeBSD's 15-character `IFNAMSIZ` limit). Human-readable metadata is attached via `ifconfig <if> description "<project>:<service>:r<N>"`.
- **Embedded Project DNS**: `yakd` runs a lightweight DNS server on `<subnet>.1:53` resolving `<service>` and `<service>.<project>.yak.` to the currently active jail IP (`<subnet>.<host>`), forwarding external lookups upstream.
- **`pf` Anchor Isolation**: `yakd` loads NAT and inter-project isolation rules strictly via `pfctl -a yak/<project> -f -`. Host `/etc/pf.conf` is never rewritten by Yak; `yak server check` validates that `nat-anchor "yak/*"` and `anchor "yak/*"` are present.

## 4. Multi-Stage Builds (`on = "local"` vs `on = "server"`)

An `image` block contains zero or more `stage` blocks plus one `runtime` block:

1. **Local Stages (`on = "local"`)**: Executed sequentially on the developer machine (ideal for pure-Go `CGO_ENABLED=0 GOOS=freebsd` cross-compilation). Declared artifacts are packed with `zstd` and uploaded alongside the workspace sync.
2. **Server Stages (`on = "server"`)**: Executed inside an ephemeral FreeBSD Builder Jail:
   - Base layer cloned from ZFS cache matching `Hash(freebsd + sorted(packages))`.
   - Persistent `cache` paths mounted via `nullfs`.
   - Ephemeral `build_secrets` injected via `tmpfs` and destroyed immediately after stage completion.
   - Artifacts copied between stages via `copy { from = stage.<name> ... }`.
3. **Runtime Layer**: Cloned from `Hash(freebsd + sorted(runtime.packages) + runtime.setup)`. If the layer snapshot already exists locally, `pkg install` is skipped completely (0ms ZFS clone). Built artifacts are copied in and sealed with `@immutable`.

## 5. Reconciliation State Machine & `dying` Jail GC

Deployments hold an exclusive per-project lock in `yakd`:

### Strategy: `blue-green` (default when `expose` is configured)
1. Clone release `r<N>` jail (`yk_<project>_<service>_r<N>` — unique name per release).
2. Run optional `migrate` command inside a one-off jail against the new artifact. Abort on non-zero exit.
3. Start new release jail wrapped in FreeBSD `daemon(8)` (`-r`) with `tmpfs` secrets.
4. Poll `healthcheck` (`http` or `cmd`) until healthy or `timeout`. On failure, stop & queue new jail for GC; **previous release stays live untouched**.
5. Atomically swap L4 `expose` forwarder (`127.0.0.1:<port>` or Unix socket) to the new jail IP, update embedded DNS A record, refresh `endpoints.json`, and run optional `hook.on_switch`.
6. Gracefully drain old connections, stop old jail, and push it to the async GC queue.

### Strategy: `recreate` (default for stateful / singleton workers & databases)
1. Gracefully stop old jail (`SIGTERM`, wait `stop_grace`).
2. Clone release `r<N>`, mount persistent storage (`owner` UID:GID enforced), run optional `init_cmd`.
3. Start new jail and wait for `healthcheck`.
4. Update embedded DNS and `endpoints.json`.

### Async `dying` Jail Reaper
FreeBSD VNET jails with sockets in `TIME_WAIT` remain in `dying` state for 30–60s after `jail -r`, causing immediate `umount`/`zfs destroy` to fail with `EBUSY`. Because every release uses a unique jail name (`yk_<proj>_<svc>_r<N>`), Yak never blocks a deploy waiting on `TIME_WAIT`. Stopped jails enter the persisted `bbolt` GC queue and are cleaned asynchronously with exponential retry once `jls` confirms they are gone.
