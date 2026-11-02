# Implementation status

_Last updated: 2026-11-02. Keep this file accurate: a feature is "done" only when its backend works and it is tested._

## Summary
| Phase | State |
|---|---|
| 0 — Architecture & foundation | **Done** (architecture doc, 7 ADRs, threat model, build tooling, Dockerfile, Compose, CI workflow) |
| 1 — Functional standalone product | **Done** (see below; verified by Go tests, Playwright UI tests and a Compose smoke test) |
| 2 — Distributed nodes / Raft | Designed (docs/architecture §5, ADR-0003/0004), **not implemented** |
| 3 — Automation / workflows / runner | Not started (incident listener hook exists: `incidents.Manager.OnTransition`) |
| 4 — Cloudflare / ArvanCloud DNS | Not started |
| 5 — Disaster-recovery workflows | Not started (incident states RECOVERY_PENDING…FAILED_OVER exist in the state machine) |
| 6 — Hardening & release | Partial (CI workflow, docs, license) |

## Completed components (Phase 1)
* **Storage** — SQLite WAL via pure-Go driver, embedded migrations, idempotent result inserts (UUID), round verdicts, hourly rollups, retention pruning, `VACUUM INTO` backups.
* **Monitoring engine** — HTTP/HTTPS (methods, headers, body, expected status patterns, body match, redirects, TLS verify toggle, origin address + Host/SNI override, latency degradation, cert expiry), TCP, DNS (A/AAAA/CNAME/MX/TXT/NS, expected value, custom resolver), TLS certificate monitor. Precise error classification. In-probe retries. Interval-aligned scheduler with per-monitor jitter and bounded worker pool.
* **Evaluation** — round verdicts (`ok/partial/failed/inconclusive/maintenance`), confirmations + min locations + min valid probes, consecutive vs rolling-window modes, recovery threshold, UNKNOWN after 3 inconclusive rounds, config-version freshness, catch-up after downtime (bounded).
* **Incidents** — persistent state machine with validated transitions, events timeline, dedup (one open incident per monitor), cooldown reopen, ack/resolve/notes via API, listener hook for automation.
* **Auth & security** — setup token bootstrap, Argon2id, sessions (hashed tokens, idle+absolute expiry), CSRF + Origin checks, rate limiting, RBAC (owner/admin/operator/viewer), last-owner/self protections, AES-256-GCM secrets with AAD, sensitive header auto-encryption, redaction, CSP & security headers, optional Host allow-list, trusted-proxy handling, offline `reset-password`.
* **API** — `/api/v1` (auth, setup, users, monitors, monitor-groups, results, uptime, stats, incidents, maintenance-windows, overview, audit-logs, settings, node, cluster, health, readiness), uniform errors, OpenAPI 3.1 spec, `/metrics`.
* **UI** — React 19 + TS + Tailwind 4 SPA embedded in the binary: setup, login, overview dashboard, monitors list/filter, monitor create/edit with live test and policy summary, monitor details (uptime periods with coverage, 90-day bars, latency chart, rounds strip, status codes, error categories, per-node, recent checks, incidents, certificate), groups, incidents + timeline, maintenance, cluster/node page, audit log, users & roles, settings (profile, password, appearance, instance, retention). Persian RTL with Vazirmatn and Jalali dates via `Intl`, English LTR, light/dark/system themes, keyboard focus styles, responsive layout. Restricted follower view (activates when the node is not the controller).
* **Deployment** — multi-stage Dockerfile (distroless, non-root, 7.4 MB content), Compose with read-only FS, dropped caps, localhost bind, healthcheck, log rotation; `.env.example`; Makefile.

## Test results (2026-11-02)
* `go vet ./...` clean; `go test -race ./...`: **36 tests passed, 0 failed** — security (hashing, secret box, key file), RBAC & rate limiter, evaluator (independent confirmation, min locations, missing data, stale config, one result per node, rounds≠probes, partial reset, window mode, recovery, maintenance, slow), checkers against live local servers (status, 5xx vs timeout, refused, retries, origin+Host, TLS verify/expiry, DNS), engine+storage+incident lifecycle (suspect → confirm → recover → cooldown reopen, uptime/coverage math, duplicate result ignore), incident transitions, API integration (setup token, CSRF, login lockout, logout, RBAC, last owner, secret non-disclosure & encryption at rest, validation, pause, read-only follower, incident ack/resolve, security headers, metrics auth).
* Playwright (Chromium, `apps/web/e2e`): **9/9 passed** — setup, login/logout, EN↔FA with RTL sidebar position, theme switching + persistence, monitor creation with 30 s interval + live test + real results, inline validation, viewer restrictions, groups/maintenance/audit/cluster pages, phone layout without horizontal overflow.
* Compose smoke test (`tests/smoke/compose_smoke.sh`): setup, 30 s HTTP monitor producing real results, container recreation preserving node identity, data and sessions — **passed**.
* Live check: a target switched to HTTP 503 produced SUSPECTED → CONFIRMED incident with category `http_5xx`; a self-signed HTTPS target was classified `tls_error`.

## Known defects / limitations
* Clustering is not implemented: Cluster page shows standalone mode only; `node_ids` accepts only the local node.
* TOTP 2FA not implemented. Login throttling state is in-memory.
* `/monitors/test` uses only credentials typed into the form, not stored secrets of an existing monitor.
* Uptime 24 h per monitor in list/overview is computed with one query per monitor (fine for hundreds, should be batched for thousands).
* Charts keep a left-to-right time axis in RTL by design.
* No master-key rotation command yet.

## Next implementation steps (Phase 2)
1. `internal/consensus`: hashicorp/raft + raft-boltdb, FSM applying typed control-plane commands to SQLite; snapshots via `VACUUM INTO`.
2. Route all control-plane writes (users, monitors, groups, settings, incidents, monitor state) through a `controlplane.Apply` interface (direct in standalone, Raft in cluster).
3. Cluster CA + per-node certificates; enrollment tokens (single-use, short-lived, hashed, revocable); Join wizard with existing-data migration/export/discard step.
4. Follower result shipping with durable local buffer and UUID dedup; leader-only evaluation (`Engine.IsEvaluator`).
5. Strict mesh gate (fresh bidirectional reachability among voters) for campaigning and for mutations; NO_QUORUM handling.
6. Three-node Compose test environment + cluster tests (leader loss, return as follower, partitions).
