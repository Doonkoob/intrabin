# Installation, backup and upgrade

## Requirements
Docker Engine 24+ with the Compose plugin. Linux amd64 or arm64. ~64 MB RAM idle.

## Install
```bash
git clone https://github.com/<you>/intrabin.git && cd intrabin
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
| 7946/tcp | Peer/Raft transport (Phase 2) | not yet used |

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

## Monitoring Intrabin itself
* `GET /api/v1/health` — liveness (used by the container health check).
* `GET /api/v1/readiness` — database reachability.
* `GET /metrics` — Prometheus metrics; requires `Authorization: Bearer $INTRABIN_METRICS_TOKEN`
  or an authenticated admin session.
