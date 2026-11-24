# Changelog

All notable changes to Yak are documented in this file.

## [0.1.0] - Unreleased

### Added
- Single static Go binary (`yak`) providing both client CLI and FreeBSD server daemon (`yak server`).
- Zero-new-port control plane tunneling exclusively over OpenSSH port 22 via `yak bridge` and `yamux` multiplexing into `/var/run/yakd.sock`.
- Strict HCLv2 `Yakfile` parser with `include` composition, two-pass symbol resolution, DAG dependency validation, and static `secret.*` discovery.
- Multi-stage builds supporting both local cross-compilation (`on = "local"`) and server-side isolated Builder Jails (`on = "server"`) with content-addressed ZFS layer caching.
- FreeBSD 14.x native isolation: per-project VNET bridge + `epair` interfaces, embedded per-project DNS, `pf` anchor isolation (`anchor "yak/*"`), ZFS datasets/quotas/snapshots, `tmpfs`-only secret injection, and async `dying` jail garbage collection.
- Decoupled L4 `expose` listener (`127.0.0.1:<port>` or Unix socket) with atomic blue/green cutover and graceful connection draining for user-managed Nginx/Caddy/HAProxy.
- Six orthogonal CLI primitives: `deploy`, `down`, `ps`, `logs`, `exec`, `secret`.
- Bilingual VitePress documentation website (`website/` for `https://bsdyak.com`).
