# Intrabin

**Find outages across regions. See which node failed. Act from one panel.**

Self-hosted uptime monitoring, incident tracking and recovery automation. Start with one server;
add monitoring nodes when you need a view from more locations. Your data stays on your infrastructure.

[نصب و معرفی فارسی](README.fa.md) · [Install](#install) · [Documentation](#documentation) · [Releases](CHANGELOG.md)

## What does it solve?

| When you need to… | Intrabin gives you… |
| --- | --- |
| Know whether a service fails in one region or everywhere | Per-node results, filters and latency charts |
| Tell a CDN problem from an origin outage | Public and direct-origin checks, with distinct DNS, TLS, HTTP and timeout errors |
| Reduce noise from transient failures | Multi-node confirmation, configurable failure thresholds and maintenance windows |
| Respond without repeating manual steps | Incident, schedule and condition workflows with scripts, webhooks and DNS actions |
| Move traffic to a backup site | DNS failover with health checks, cooldowns and an optional automatic trigger |
| Keep several monitoring servers up to date | Sequential cluster updates, progress bars and release notes in the panel |

## Built for day-to-day operations

- **Monitor:** HTTP/HTTPS, TCP, DNS and TLS certificates; uptime history, certificate expiry and response times.
- **Investigate:** incident timelines, per-node errors, acknowledgements, notes and an audit log. Missing observations are tracked separately from downtime.
- **Operate:** Cloudflare and ArvanCloud DNS, change previews, snapshots and rollback; Bash/Python workflows through an optional Action Runner.
- **Share:** user roles, encrypted stored credentials, Persian/English and light/dark themes.
- **Connect:** Prometheus metrics and an OpenAPI HTTP API.

A single Go binary serves the React panel and stores data in SQLite. Cluster nodes replicate
configuration with Raft and communicate over mutual TLS. Leader failover keeps control moving;
configuration changes and recovery actions pause without quorum.

DNS recovery moves traffic; it does not replicate application data. Returning to the primary site is manual.

## Install

On Ubuntu 20.04, 22.04 or 24.04 (amd64/arm64), run as root:

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/Doonkoob/intrabin/main/scripts/install.sh)
```

The installer sets up Docker/Compose, optionally configures a domain with HTTPS, starts services
one at a time, and prints the panel URL and generated login credentials. Without a domain, it uses
HTTP on port 8080. Use `--release vX.Y.Z` with a [published version](https://github.com/Doonkoob/intrabin/releases)
to pull prebuilt images instead of building on the server.

Then:

1. Add a monitor and test its endpoint.
2. Add nodes from **Cluster / Nodes** for checks from other servers.
3. Configure workflows or recovery plans when you are ready to automate actions.

Run `intrabin` on the server for the management menu. Install updates from **Settings → Software updates**;
starting an update on the leader updates compatible cluster members one at a time.

[Manual installation, networking, backups and upgrades →](docs/operations/installation.md)

## Documentation

[Clustering](docs/operations/cluster.md) · [Automation](docs/operations/automation.md) ·
[DNS](docs/operations/dns.md) · [Recovery plans](docs/operations/dr.md) ·
[Implementation status & limitations](docs/IMPLEMENTATION_STATUS.md)

## Development

```bash
make build        # build UI and Go binary
make run          # start with ./data
make test         # Go checks and race tests
make test-e2e     # browser regression tests
make test-cluster # local three-node cluster tests
```

Source: `cmd/intrabin/` (entry point), `internal/` (server), `apps/web/` (panel), `tests/` (integration tests).

MIT licensed. See [LICENSE](LICENSE).
