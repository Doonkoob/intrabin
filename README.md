# Intrabin

Self-hosted, open-source **uptime monitoring, incident detection and recovery automation**.
One Go binary with an embedded React UI, embedded SQLite storage, Persian/English (RTL/LTR) and
light/dark themes. Every installation runs the same software; nodes can form a Raft cluster for
multi-location monitoring with automatic leader failover.

> Status: **Phases 1 (standalone), 2 (clustering) and 3 (automation) are functional.** DNS providers
> and disaster-recovery workflows are not implemented yet — see
> [docs/IMPLEMENTATION_STATUS.md](docs/IMPLEMENTATION_STATUS.md).

## Features available now
* HTTP/HTTPS, TCP, DNS and TLS-certificate monitors (intervals from 10 s; 30 s works reliably).
* Precise error categories (HTTP 5xx vs timeout vs DNS vs TLS vs refused vs unreachable — a timeout is never reported as 5xx).
* Origin checks: connect to an origin IP while keeping Host header / SNI, to tell CDN from origin failures.
* Round-based detection policy: confirmations, minimum independent locations, consecutive or rolling-window escalation, recovery threshold, cooldown. Missing data is never treated as downtime.
* Persistent incident state machine with deduplication, acknowledgement, manual resolution, notes and a full timeline.
* Uptime for 24 h / 7 d / 30 d / 90 d with separate **availability** and **coverage**, 90-day history bars, response-time charts, status-code and error breakdowns, per-node results, certificate expiry.
* Maintenance windows, monitor groups and tags.
* Users with Owner/Admin/Operator/Viewer roles, Argon2id passwords, CSRF protection, login rate limiting, audit log.
* Secrets encrypted at rest (AES-256-GCM) and never returned by the API.
* **Clustering**: create a cluster from any node, add nodes with single-use join tokens, promote them
  to voters; Raft (hashicorp/raft) replicates the control plane, peers use mutual TLS, followers ship
  probe results to the leader, a mesh safety gate and NO_QUORUM mode prevent split-brain actions.
  See [docs/operations/cluster.md](docs/operations/cluster.md).
* **Automation**: workflows triggered by incident status changes, schedules or by hand, with Bash/Python
  script steps in an isolated Action Runner container, HTTP/webhook steps with SSRF protection, durable
  waits and incident status updates. Every external action is fenced by the leader term; an action whose
  outcome is uncertain after a failover is never repeated but marked UNKNOWN_OUTCOME for reconciliation.
  Workflows are disabled until enabled. See [docs/operations/automation.md](docs/operations/automation.md).
* Prometheus metrics, structured JSON logs, health/readiness endpoints, OpenAPI spec at `/api/v1/openapi.yaml`.

## Quick start (Docker Compose)
```bash
git clone <this repository> intrabin && cd intrabin
cp .env.example .env
docker compose up -d --build
docker compose logs intrabin | grep setup_token     # one-time setup token
```
Open <http://127.0.0.1:8080>, enter the setup token and create the owner account. The UI is bound
to localhost by default; see [docs/operations/installation.md](docs/operations/installation.md) for
TLS/reverse-proxy setup, backup/restore, upgrades and credential recovery.

## Development
```bash
make build        # builds apps/web and bin/intrabin
make run          # runs with ./data
make test         # go vet + go test -race
make test-e2e     # Playwright UI tests against a fresh binary
make test-cluster # 3 local processes: join, promote, leader failover, quorum loss
tests/cluster/run_compose.sh   # same test against a 3-node Docker Compose cluster
cd apps/web && npm run dev   # Vite dev server proxying /api to :8080
```

## Repository layout
```
cmd/intrabin/          main binary (serve, healthcheck, backup, reset-password, version)
internal/api/          HTTP API, middleware, OpenAPI spec
internal/auth/         RBAC, login throttling
internal/cluster/      Raft, mTLS transport, CA/enrollment, mesh gate, result shipping
internal/controlplane/ replicated write path (commands executed locally or via Raft)
internal/audit/        audit logger
internal/config/       environment configuration
internal/incidents/    incident state machine and manager
internal/metrics/      Prometheus metrics
internal/model/        domain types
internal/monitoring/   probes, round evaluator, scheduler engine
internal/node/         node identity and role
internal/security/     Argon2id, AES-GCM secret box, tokens, redaction
internal/storage/      SQLite persistence and migrations
internal/web/          embedded UI assets
apps/web/              React + TypeScript + Tailwind UI, Playwright tests
docs/                  architecture, ADRs, security, operations
tests/smoke/           Docker Compose smoke test
tests/cluster/         cluster e2e test (local processes or 3-node Compose)
```

## License
Apache-2.0 — see [LICENSE](LICENSE).
