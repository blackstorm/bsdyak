# HCL Schema Reference

## Top-Level Blocks & Attributes

### `include = [string, ...]`
Optional list of relative HCL files merged into the project configuration.

### `project "<name>" { ... }` (Required, exactly 1)
- `freebsd` (`string`, required): Target FreeBSD release, e.g. `"14.2-RELEASE"`.

### `target "<name>" { ... }`
- `host` (`string`, required): SSH target (`user@host` or SSH config alias, optional `:port`).
- `default` (`bool`): Selects this target when `-T` is omitted.
- `vars` (`map(string)`): Key-value variables exposed as `var.<key>`.

### `storage "<name>" { ... }`
- `size` (`string`): ZFS `quota` (e.g. `"20G"`).
- `snapshot_before_deploy` (`bool`): Creates a pre-deploy ZFS snapshot before service updates.
- `zfs` (`map(string)`): ZFS dataset properties (e.g. `recordsize = "8k"`, `compression = "lz4"`).

### `image "<name>" { ... }`
Contains zero or more `stage "<name>"` blocks and exactly one `runtime` block.

#### `stage "<name>" { ... }`
- `on` (`string`): `"server"` (default) or `"local"`.
- `packages` (`list(string)`): FreeBSD packages for server stage Builder Jail.
- `cache` (`list(string)`): Persistent host-backed cache directories mounted inside server stage.
- `workdir` (`string`): Working directory inside build stage.
- `env` (`map(string)`): Environment variables during build steps.
- `build_secrets` (`list(string)`): Host secret keys injected in RAM for the stage only.
- `copy { from, src, dst }`: Copies files from an earlier stage (`stage.<name>`).
- `steps` (`list(string)`, required): Shell commands executed in sequence.

#### `runtime { ... }`
- `packages` (`list(string)`): FreeBSD packages baked into cached ZFS runtime layer.
- `setup` (`list(string)`): Shell commands executed during layer creation (cached in layer hash).
- `copy { from, src, dst }`: Copies artifacts from a build `stage.<name>` or project workspace.
- `env` (`map(string)`): Default runtime environment variables.
- `user` (`string`): Runtime execution user inside jail.
- `workdir` (`string`): Working directory inside jail.

### `service "<name>" { ... }`
- `image` (`reference`, required): `image.<name>`.
- `depends_on` (`list(reference)`): `[service.<name>, ...]`.
- `strategy` (`string`): `"blue-green"` (default if `expose` set) or `"recreate"`.
- `stop_grace` (`string`): Graceful shutdown/drain timeout (e.g. `"30s"`).
- `jail` (`map(string)`): Whitelisted jail parameters (`sysvshm`, `sysvsem`, `sysvmsg`, `allow.raw_sockets`, `allow.mlock`).
- `env` (`map(string)`): Runtime environment (supports `service.<name>.host`, `secret.<key>`, `var.<key>`).
- `init_cmd` (`string`): Synchronous shell hook run before service start.
- `migrate` (`string`): One-off migration command executed before `blue-green` cutover.
- `cmd` (`string`, required): Main process command supervised by `daemon(8)`.
- `port` (`number`): Service listening port inside jail.
- `mount { source = storage.<name>, target = "/path", owner = "uid:gid" }`: ZFS volume mount via `nullfs`.
- `healthcheck { http, cmd, interval, timeout }`: Readiness probe.
- `expose { listen, unix, to }`: Loopback TCP or Unix socket L4 forwarder.
- `resources { memory, cpu }`: `rctl` memory/CPU caps.
- `hook { on_switch }`: Host shell command run after blue/green traffic switch.
