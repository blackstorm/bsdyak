# ZFS & Host Layout

## ZFS Dataset Tree (`zroot/yak`)

```text
zroot/yak/
├── bases/
│   └── 14.2-RELEASE@base           # Official FreeBSD base.txz extracted once
├── layers/
│   └── <sha256-16>@ready           # Content-addressed pkg + setup cache layer
├── cache/
│   └── <project>/                  # Persistent builder caches (/go/pkg/mod, etc.)
└── projects/
    └── <project>/
        ├── volumes/
        │   └── <storage>/          # Persistent ZFS dataset mounted via nullfs
        ├── releases/
        │   └── <svc>-r<N>@immutable # Sealed release snapshot (for rollback)
        └── jails/
            └── yk_<proj>_<svc>_r<N> # Active ephemeral jail clone
```

## Host Files & Permissions

| Path | Mode / Owner | Purpose |
| :--- | :--- | :--- |
| `/usr/local/bin/yak` | `0755 root:wheel` | Static single binary (CLI + `yakd` + `bridge`) |
| `/usr/local/etc/yak.conf` | `0644 root:wheel` | ZFS root, egress interface, state paths |
| `/usr/local/etc/rc.d/yakd` | `0555 root:wheel` | FreeBSD `rc.d` service definition |
| `/var/db/yak/state.db` | `0600 root:wheel` | `bbolt` state, subnets, secrets, GC queue |
| `/var/run/yakd.sock` | `0660 root:yak` | Local Unix socket bridged by SSH |
| `/var/run/yak/projects/<p>/endpoints.json` | `0644 root:wheel` | Atomic JSON service discovery file |

## Safety Guardrails

1. **85% ZFS Pool Guard**: Deploys abort before creating new snapshots or clones when pool capacity reaches `85%`.
2. **`tmpfs` Secret Isolation**: Secrets live strictly inside `<jail>/.yak-run/env` on RAM `tmpfs` (`0400`) and never appear on ZFS datasets or `zfs send` streams.
3. **Async `dying` Jail Reaper**: Stopped VNET jails stuck in `TIME_WAIT` `dying` state are cleaned asynchronously by `yakd`'s background GC worker without blocking deployments.
