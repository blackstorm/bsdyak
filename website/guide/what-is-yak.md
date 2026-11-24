# What is Yak?

**Yak** is a declarative single-host deployment tool engineered specifically for **FreeBSD 14.x**, combining **VNET Jails**, **ZFS**, **`pf` anchors**, and **OpenSSH** into a calm, predictable workflow.

## Why Build Yak Instead of Running Containers?

FreeBSD already ships best-in-class isolation and storage primitives in the base system:

- **ZFS Copy-on-Write Layers**: Cloning an OS + package runtime snapshot takes milliseconds and consumes zero duplicate disk blocks. Rolling back is restoring a prior `@immutable` ZFS snapshot.
- **VNET Jails**: Every service gets its own isolated network stack (`epair` + per-project `bridge`) and can run native FreeBSD packages (`postgresql16-server`, `redis`, `ffmpeg`, `vips`) with zero Linux emulation overhead.
- **No Fat Daemon / No Exposed Control Plane**: Both client and server install the same static Go binary (`yak`). All remote commands ride inside OpenSSH `:22` using `yak bridge`.

## Five Core Principles

1. **Zero Hidden Presets**: Yak never embeds opaque `"postgres:16"` black boxes. You compose databases, workers, and web apps using generic HCLv2 primitives (`packages`, `jail`, `mount`, `init_cmd`, `cmd`, `healthcheck`) and share them via `include`.
2. **Host Respect**: `yak server init` never rewrites `/etc/pf.conf`, `/etc/rc.conf`, or `/boot/loader.conf`. Firewall rules live strictly in `anchor "yak/*"` and `nat-anchor "yak/*"`.
3. **Secrets Never Enter ZFS**: Secret values are stored on the host with `0600` permissions and injected into jails solely via a volatile memory `tmpfs` mount (`0400`). Your ZFS snapshots and `zfs send` backups remain clean.
4. **Ingress Agnostic**: Keep your existing Nginx, Caddy, HAProxy, or Cloudflare Tunnel. Services expose a loopback TCP port (`127.0.0.1:<port>`) or Unix socket; Yak performs atomic L4 blue/green backend switching underneath.
5. **Six Composable Commands**: Instead of inventing 30 subcommands, Yak provides `deploy`, `down`, `ps`, `logs`, `exec`, and `secret`—designed to pair naturally with `ssh -L`, `grep`, `jq`, and shell redirects.
