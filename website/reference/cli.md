# CLI Reference

Global flags available on all client commands:
- `-F, --file <path>`: Path to `Yakfile` (default `./Yakfile`)
- `-T, --target <name>`: Named `target` block from `Yakfile`

## Client Commands

### `yak deploy [service...]`
Reconciles declared storage, network, builds, and services on the target FreeBSD host.
- `--dry-run`: Display execution plan without changing host state
- `--local-validate`: Validate `Yakfile` syntax, includes, and dependency graph offline
- `--release <rev>`: Roll back named service(s) to an existing release snapshot (e.g. `r41`)

### `yak down [service...]`
Stops and removes active service jails (preserving all `storage` ZFS volumes). When no service names are given, tears down all active services and the project VNET bridge.

### `yak ps [service]`
Prints running services, release revisions, internal VNET IPs, exposed loopback/Unix endpoints, and jail names.
- `--json`: Output structured JSON
- `--ip`: Print only the IP address of the named single service

### `yak logs <service>`
Streams or tails service logs to stdout.
- `-f, --follow`: Stream live log lines
- `-n, --lines <int>`: Number of trailing lines to output (default `100`)

### `yak exec <service> -- <cmd...>`
Executes a command inside the active jail for `<service>`.
- `-i, --stdin`: Forward stdin
- `-t, --tty`: Request interactive terminal handling

### `yak secret <subcommand>`
- `yak secret set <KEY> [VALUE]`: Set a secret (reads from `stdin` if `VALUE` is omitted)
- `yak secret unset <KEY>`: Delete a secret key
- `yak secret ls`: List configured secret keys

## Server Commands

### `yak server init`
Idempotently provisions ZFS datasets, `yak` group, directories, `/usr/local/etc/yak.conf`, and `/usr/local/etc/rc.d/yakd`.
- `--zfs-root <dataset>` (default `zroot/yak`)
- `--egress <iface>` (default `vtnet0`)

### `yak server check`
Verifies kernel version, ZFS pool usage (`< 85%`), `yak` group, IP forwarding, `/etc/pf.conf` anchors, and `rctl` status.

### `yak server run`
Runs the `yakd` daemon listening on `/var/run/yakd.sock` (`0660 root:yak`).

### `yak bridge` (Internal)
Executed automatically over SSH (`yak bridge [--allow-project <name>]`) to multiplex client requests to `yakd.sock`.
