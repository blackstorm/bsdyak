# Server Setup (FreeBSD 14.x)

Yak targets **FreeBSD 14.x with ZFS**. `yak server init` is strictly scoped to Yak-owned paths and **never edits `/etc/pf.conf`, `/etc/rc.conf`, or `/boot/loader.conf`**.

## 1. Run `yak server init`

```sh
sudo install -m 0755 yak-freebsd-amd64 /usr/local/bin/yak
sudo yak server init --zfs-root zroot/yak --egress vtnet0
```

This idempotently creates:
- System group `yak` (grants `/var/run/yakd.sock` access without root SSH login)
- Directories `/var/db/yak` (`0700`) and `/var/run/yak` (`0755`)
- ZFS datasets `zroot/yak/{bases,layers,cache,projects}`
- Config file `/usr/local/etc/yak.conf`
- Service script `/usr/local/etc/rc.d/yakd`

## 2. Check & Enable Host Prerequisites

Run the read-only host inspector:

```sh
sudo yak server check
```

If any host-level item is missing, `yak server check` prints the exact snippet for you to confirm:

### Enable IP Forwarding & PF

```sh
sudo sysrc gateway_enable="YES"
sudo sysctl net.inet.ip.forwarding=1
sudo sysrc pf_enable="YES"
```

In `/etc/pf.conf`, add the Yak anchors in the translation/filter sections:

```pf
# Translation rules section:
nat-anchor "yak/*"

# Filter rules section:
anchor "yak/*"
```

Reload PF:

```sh
sudo service pf reload
```

### Optional: Enable `rctl` Resource Limits

If your `Yakfile` uses `resources { memory = "1G" cpu = "100%" }`, add `kern.racct.enable="1"` to `/boot/loader.conf` and reboot.

## 3. Start `yakd`

```sh
sudo sysrc yakd_enable="YES"
sudo service yakd start
```

## 4. Non-Root SSH & Scoped CI Keys

Instead of logging in as `root`, add your deployment user to the `yak` group:

```sh
sudo pw useradd -n deploy -m -G yak -s /bin/sh
```

For CI/CD keys in `~deploy/.ssh/authorized_keys`, pin the key so it can only bridge to a single project and never open an interactive shell:

```text
command="/usr/local/bin/yak bridge --allow-project=shop",restrict ssh-ed25519 AAAA...
```
