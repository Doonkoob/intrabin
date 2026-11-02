# Installation, backup and upgrade

## Requirements
Docker Engine 24+ with the Compose plugin. Linux amd64 or arm64. ~64 MB RAM idle.

## Install
### Ubuntu installer (recommended)
As root on Ubuntu 20.04/22.04/24.04:
```bash
bash <(curl -fsSL https://raw.githubusercontent.com/Doonkoob/intrabin/main/scripts/install.sh)
```
What it does, in order:
1. Checks the OS (`/etc/os-release`), CPU architecture, RAM and disk.
2. Installs `git curl openssl ca-certificates`, then Docker Engine, Compose and buildx from
   download.docker.com, falling back to Ubuntu's `docker.io` / `docker-compose-v2` packages when
   that host is unreachable. Existing Docker installations are left as they are.
3. Clones the repository to `/opt/intrabin` (or updates it, if it is already there).
4. Writes `/opt/intrabin/.env` (mode 600) with a random master key, a one-time setup token and,
   if enabled, a random Action Runner token. Back this file up: it holds the master key.
5. With a domain: enables the `proxy` Compose profile (Caddy), which obtains a Let's Encrypt
   certificate on first access and renews it automatically; Intrabin itself stays on
   `127.0.0.1:8080`. Without a domain: publishes port 8080 (all interfaces, or localhost only).
   Opens the needed ports in UFW when UFW is active.
6. Builds and starts the containers, waits for the health check, creates the owner account with a
   random username (`admin-xxxxxx`) and a 20-character random password through the setup API, and
   removes the setup token from `.env`. The password is printed once and stored nowhere.
7. Installs the `intrabin` management command (`/usr/local/bin/intrabin`).

Restricted networks: `--docker-mirror https://mirror.example` (Docker registry mirror, written to
`/etc/docker/daemon.json` only when that file is empty), `--goproxy` and `--npm-registry` (used
while building the image). Re-running the installer without `--domain`/`--no-domain` keeps the
existing configuration and updates the code. Updates replace local edits to tracked files; put
custom Compose settings in `/opt/intrabin/compose.override.yaml`, which is never overwritten.

### Management CLI
`intrabin` opens an interactive menu; `intrabin help` lists the direct commands: `status`,
`start|stop|restart`, `logs [intrabin|caddy|runner]`, `password [user]`, `rename [user] [new]`,
`users`, `domain`, `runner on|off`, `backup` (database + `.env` + key file into
`/opt/intrabin/backups/*.tar.gz`, mode 600), `update` (backs up, pulls, rebuilds, restarts),
`info`, `uninstall` (data volumes are deleted only after typing `DELETE`). Password and username
changes run the offline commands below inside the container and are refused on cluster members.

### Manual
```bash
git clone https://github.com/Doonkoob/intrabin.git && cd intrabin
cp .env.example .env            # review the settings
openssl rand -hex 32            # optional: paste into INTRABIN_MASTER_KEY
docker compose up -d --build
docker compose logs intrabin | grep setup_token
```
Open `http://127.0.0.1:8080`, paste the setup token, create the owner account.

### Ports
| Port | Purpose | Default exposure |
|---|---|---|
| 8080/tcp | Web UI + API (+ `/metrics` with auth) | `127.0.0.1` only |
| 8090/tcp | Action Runner API (separate `runner` container, optional; see [automation.md](automation.md)) | not published; reached over the Compose network |
| 7443/tcp | Cluster port: Raft + peer API with mutual TLS (only used after creating or joining a cluster; see [cluster.md](cluster.md)) | not published by the single-node Compose file |

To expose Intrabin, put a TLS reverse proxy in front (recommended) and set
`INTRABIN_TRUSTED_PROXIES` to the proxy's address so client IPs and `https` are detected, or set
`INTRABIN_TLS_CERT`/`INTRABIN_TLS_KEY` and change `INTRABIN_BIND`.

Example Caddy site:
```
intrabin.example.com {
  reverse_proxy 127.0.0.1:8080
}
```

## Backup
The data volume holds `intrabin.db` (SQLite, WAL) and `master.key` (unless you use
`INTRABIN_MASTER_KEY`). Take a consistent online backup:
```bash
docker compose exec intrabin intrabin backup -out /var/lib/intrabin/backup-$(date +%F).db
docker compose cp intrabin:/var/lib/intrabin/backup-$(date +%F).db ./
docker compose cp intrabin:/var/lib/intrabin/master.key ./   # store separately and securely
```
Without the master key, encrypted monitor credentials cannot be recovered.

## Restore
```bash
docker compose down
docker run --rm -v intrabin_intrabin-data:/data -v "$PWD":/backup alpine \
  sh -c 'cp /backup/backup-YYYY-MM-DD.db /data/intrabin.db && rm -f /data/intrabin.db-wal /data/intrabin.db-shm && cp /backup/master.key /data/ && chown 65532:65532 /data/*'
docker compose up -d
```

## Upgrade
```bash
docker compose exec intrabin intrabin backup -out /var/lib/intrabin/pre-upgrade.db
git pull && docker compose up -d --build
```
Schema migrations run automatically at start-up inside a transaction. Downgrades are not supported;
restore the pre-upgrade backup instead.

## Credential recovery
If every owner is locked out:
```bash
docker compose exec -e INTRABIN_NEW_PASSWORD='a-new-long-password' intrabin intrabin reset-password -user owner
```
This requires shell access to the host, re-enables the account, revokes its sessions and is audited.
With the installer, `intrabin password` does the same (random or typed password). Usernames can be
changed with `intrabin rename` (`intrabin rename-user -user old -to new` inside the container), and
`intrabin backup -out -` streams a consistent database copy to stdout.

## Monitoring Intrabin itself
* `GET /api/v1/health` — liveness (used by the container health check).
* `GET /api/v1/readiness` — database reachability.
* `GET /metrics` — Prometheus metrics; requires `Authorization: Bearer $INTRABIN_METRICS_TOKEN`
  or an authenticated admin session.
