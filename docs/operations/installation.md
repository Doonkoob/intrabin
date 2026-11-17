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

Published releases: `--release v1.2.3` checks out that tag and pulls the images that the release
workflow (`.github/workflows/release.yml`, run when a `v*` tag is pushed) publishes to
`ghcr.io/<owner>/intrabin` and `ghcr.io/<owner>/intrabin-runner`, so nothing is built on the server.
Without it the images are built from source (about 1.5 GB RAM and a few minutes).

Restricted networks: `--docker-mirror https://mirror.example` (Docker registry mirror, written to
`/etc/docker/daemon.json` only when that file is empty), `--goproxy` and `--npm-registry` (used
while building the image; Go defaults to `https://goproxy.cn`, configurable with `GOPROXY_URL` in `.env`). Re-running the installer without `--domain`/`--no-domain` keeps the
existing configuration and updates the code. Updates replace local edits to tracked files; put
custom Compose settings in `/opt/intrabin/compose.override.yaml`, which is never overwritten.

### Management CLI
`intrabin` opens an interactive menu; `intrabin help` lists the direct commands: `status`,
`start|stop|restart`, `logs [intrabin|caddy|runner]`, `password [user]`, `rename [user] [new]`,
`users`, `domain`, `runner on|off`, `backup` (database + `.env` + key file into
`/opt/intrabin/backups/*.tar.gz`, mode 600), `restore [file]`, `update` (backs up, pulls, rebuilds, restarts),
`update --to vX.Y.Z` (installs a published release, see [Upgrade](#upgrade)), `info`, `uninstall` (data volumes are deleted only after typing `DELETE`). Password and username
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
With the installer: `intrabin restore` lists the backups in `/opt/intrabin/backups` (or pass a
`.tar.gz` file). It takes a safety backup of the current data, offers to switch to the backup's
master key if it differs, stops Intrabin, restores and starts it again.

Manually (standalone nodes only; the server must be stopped):
```bash
docker compose stop intrabin
docker compose run --rm --no-deps -v "$PWD/restore:/restore:ro" intrabin \
  restore -in /restore/backup.db [-key /restore/master.key]
docker compose up -d
```
`intrabin restore` checks the file (SQLite integrity check, Intrabin schema), refuses backups from
cluster members and cluster nodes, keeps the current database as `intrabin.db.pre-restore-<time>`,
applies newer migrations and writes an audit entry. The files in `restore/` must be readable by
UID 65532. Without the matching master key (`INTRABIN_MASTER_KEY` or `master.key`), stored
credentials (monitor secrets, DNS tokens) cannot be decrypted.

## Upgrade
### Restricted registries or an outdated updater

From v0.2.7, tagged GitHub updates default to release images even for existing source installs.
Older host scripts need the one-time bootstrap below to adopt this behavior.

A Docker Hub `403 Forbidden` while resolving Node, Go or Python prevents source builds. Existing
installations can switch to prebuilt images with `intrabin update --to v0.2.7 --prebuilt`.
The updater first tries GHCR, then the matching public GitHub release image archive. Images are
prepared one service at a time; cached versioned images are reused.

If the installed CLI predates this option (or still builds both services together), run once:

```bash
curl -fsSL https://raw.githubusercontent.com/Doonkoob/intrabin/v0.2.7/scripts/update.sh -o /tmp/intrabin-update.sh &&
sudo bash /tmp/intrabin-update.sh v0.2.7 /opt/intrabin
```

This loads the tagged updater before changing installed files, takes the normal backup, and keeps
prebuilt mode for subsequent panel updates. Run it on each outdated cluster host. An uncached
proxy image still needs its configured registry; set `CADDY_IMAGE` to an accessible equivalent
when necessary. Other installation settings and named data volumes are preserved.

### From the web UI (installer setups)
Every node checks GitHub once a day for a newer release (`INTRABIN_UPDATE_REPO`, default
`Doonkoob/intrabin`; turn it off with `INTRABIN_UPDATE_CHECK=false`). Owners and admins then see a
notice at the top of the page; closing it hides it for 30 days. **Settings → Updates** shows the
installed and latest version and has **Check now**.

**Update** asks for confirmation, then hands the update to the server's update agent: the container
writes a request into `/opt/intrabin/run/update` (mounted as `INTRABIN_UPDATE_DIR`), and the systemd
unit `intrabin-update.path` runs `intrabin apply-update` on the host, which

1. backs up the database and configuration (`/opt/intrabin/backups`),
2. checks out the release tag and pulls its images (including previous GitHub source installs),
3. recreates the containers and waits until the new version answers.

Data volumes and `.env` are kept; schema migrations run when the new version starts. If the pull or
build fails, the previous files are restored and the old containers keep running. Only a release
newer than the installed one can be requested from the UI. Progress and the log appear in the dialog
(and in `/opt/intrabin/run/update.log`). The container never gets access to Docker: the request file
only names a version, which the agent validates.

The agent is installed by `scripts/install.sh` (re-run it on existing installations). Without it,
the update dialog shows the command to run instead. From v0.2.0 onward, starting an update on
the leader also updates the other members sequentially; see the first-upgrade instructions below.

### From the command line
```bash
intrabin update --to vX.Y.Z          # backup, fetch, download release images, restart
intrabin update --to vX.Y.Z --build  # explicitly build this release on the server
```
Manual Compose installs:
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

## Data retention and database size
**Settings → Instance → Keep history for** chooses how long history is kept: 7 days, 15 days, 1, 3, 6
or 12 months. Older check results, rounds, hourly uptime and per-node latency aggregates, audit
entries and finished workflow runs are deleted automatically (housekeeping every 15 minutes on the active
controller; deletions are replicated). Individual check results are kept at most 90 days even with
longer presets; the hourly aggregates keep uptime and latency history for the full period. **Custom
(advanced)** restores the separate per-type fields. The **Storage** card shows the database size on
this node and the size and row count of each data type (`GET /api/v1/system/storage`). SQLite reuses
freed pages, so the file shrinks only after a `VACUUM`; the size stops growing once history reaches the
retention period.

## Monitoring Intrabin itself
* `GET /api/v1/health` — liveness (used by the container health check).
* `GET /api/v1/readiness` — database reachability.
* `GET /metrics` — Prometheus metrics; requires `Authorization: Bearer $INTRABIN_METRICS_TOKEN`
  or an authenticated admin session.

### Cluster rolling updates (v0.2.0+)
Start an update from Settings on the active leader. The plan is committed through Raft before
that server restarts. After it returns, the current leader updates remaining members one at a
time (non-voters, then voters). A leadership change resumes the same plan. Progress and errors
appear in Settings; failed updates stop until an administrator chooses **Retry remaining nodes**.
Every server needs the installer-managed host update agent and outbound GitHub/registry access.
The confirmed release is pinned for the whole rollout. New members added after confirmation
are not included; update them separately. Removed members stop the plan.

**First upgrade:** versions before v0.2.0 cannot accept peer update commands. Upgrade every
existing node once to v0.2.0 from its own Update button or with
`sudo intrabin update --to v0.2.0`. If the UI says the agent is missing, run the installer again.
Future updates started on the leader include all members automatically. A one- or two-voter
cluster temporarily loses management availability during a voter restart; all rollouts pause
while quorum or member reachability is missing. Startup failures require manual recovery;
backups are retained and cluster databases are never automatically rolled back.

### Publishing a release
Choose `vMAJOR.MINOR.PATCH`, update `VERSION` and `CHANGELOG.md`, and add
`docs/releases/vMAJOR.MINOR.PATCH.md`. Run the Go and UI checks, commit and push main, then
push the matching tag. The release workflow checks the version, publishes both architectures
of the app and runner images, then publishes the GitHub release with those notes. Settings
shows the current version, latest release and up to 20 recent stable releases with their changes.

### Sequential containers and update progress
The installer and management update commands prepare each enabled service image in its own
Compose invocation, then start containers one at a time. Compose engine concurrency is fixed
at one; a separate build invocation per service also supports older Buildx releases. Source
installs build each service and fetch image-only dependencies (such as Caddy); release installs
pull each image. Startup uses `--no-build --pull never` so it cannot trigger hidden builds.

The update dialog and Settings show deployment milestones: fetch, backup, image preparation,
container startup and health verification. Progress is based on completed stages/services, not
estimated elapsed time. The percentage stays at the last known milestone during a restart;
older agents without progress fields show an indeterminate bar. Cluster progress includes
completed nodes and the active node's progress. A failed update retains its stage and percentage;
100% is reported only after the host verifies the target version is healthy.
