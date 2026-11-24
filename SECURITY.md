# Security Policy

## Security Model

Yak is designed to minimize attack surface on production FreeBSD hosts:

1. **No Public Control-Plane Port**: `yakd` listens strictly on a local Unix domain socket (`/var/run/yakd.sock`, mode `0660`, owned by `root:yak`). Remote clients connect solely through OpenSSH (`:22`) executing `yak bridge`.
2. **Scoped CI Keys**: Deployment keys in `authorized_keys` can be pinned to a single project using OpenSSH forced commands:
   ```text
   command="/usr/local/bin/yak bridge --allow-project=shop",restrict ssh-ed25519 AAAA...
   ```
   `yak bridge` enforces the project path prefix before forwarding frames to `yakd.sock`.
3. **Secrets Never Enter ZFS Snapshots**: Secret values are stored on the host under `/var/db/yak/` (`0700` dir, `0600` file) and injected into jails exclusively via a volatile `tmpfs` mount (`0400`) or RAM-only builder context. ZFS dataset snapshots and `zfs send` streams never contain secret material.
4. **Network & Firewall Isolation**: Each project receives an isolated VNET bridge and subnet (`10.88.X.0/24`). Firewall rules live exclusively inside `yak/*` `pf` anchors.

## Reporting a Vulnerability

If you discover a security vulnerability in Yak, please **do not open a public GitHub issue**.

Instead, report it privately via GitHub Security Advisories or email `security@bsdyak.com` with:
- A clear description of the vulnerability and potential impact.
- Minimal reproduction steps or proof of concept.
- Affected Yak version and FreeBSD release.

We will acknowledge receipt within 48 hours and coordinate a fix before public disclosure.
