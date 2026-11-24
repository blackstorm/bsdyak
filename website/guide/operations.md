# Operations & Unix Composition

Yak ships **six orthogonal commands** designed to compose with OpenSSH and standard shell pipelines rather than bundling a monolithic PaaS wrapper.

## Daily Workflow

```sh
# 1. Inspect planned changes
yak deploy --dry-run

# 2. Deploy all services (or a single service)
yak deploy
yak deploy api

# 3. Check live release, jail names, and internal IPs
yak ps
```

## Instant Rollbacks

Every successful build creates an immutable ZFS snapshot (`@immutable`). Rolling back clones that exact rootfs snapshot without rebuilding:

```sh
yak deploy api --release r41
```

## Connect Directly to PostgreSQL / Redis (Without Opening Ports)

Never expose `5432` or `6379` on the public interface. Compose `yak ps --ip` with standard SSH local forwarding:

```sh
ssh -N -L 5432:$(yak ps db --ip):5432 deploy@203.0.113.10
# Now connect TablePlus / psql / pgAdmin to localhost:5432
```

## Stream Logs & Pipe Output

```sh
yak logs api -f
yak logs api -n 500 | grep ERROR
yak logs api -f | jq -r '.msg'
```

## Remote Execution & Backups

```sh
# Interactive shell or database console
yak exec -it db -- su -m postgres -c psql
yak exec -it cache -- redis-cli

# Stream logical database dump straight to workstation
yak exec db -- su -m postgres -c 'pg_dump -Fc shop' > shop-$(date +%F).dump
```

## Secrets via Pipes

```sh
# Inject from password manager without shell history leaks
op read "op://prod/shop/pg_pass" | yak secret set postgres_password
yak secret ls
```
