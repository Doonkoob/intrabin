# Implementation status

_Last updated: 2026-11-02. Keep this file accurate: a feature is "done" only when its backend works and it is tested._

## Summary
| Phase | State |
|---|---|
| 0 — Architecture & foundation | **Done** (architecture doc, 9 ADRs, threat model, build tooling, Dockerfile, Compose, CI workflow) |
| 1 — Functional standalone product | **Done** (see below; verified by Go tests, Playwright UI tests and a Compose smoke test) |
| 2 — Distributed nodes / Raft | **Done** (see below; verified by in-process 3-node tests, a 3-process e2e test and a 3-node Docker Compose failover test) |
| 3 — Automation / workflows / runner | **Done** (see below; verified by Go unit/integration tests, an in-process failover test, Playwright and the local + Compose 3-node e2e with a leader killed mid-script) |
| 4 — Cloudflare / ArvanCloud DNS | Not started |
| 5 — Disaster-recovery workflows | Not started (incident states RECOVERY_PENDING…FAILED_OVER exist in the state machine) |
| 6 — Hardening & release | Partial (CI workflow, docs, license) |

## Completed components (Phase 3: automation)
* **Data model** (migration 0003) — `workflows` (disabled by default, trigger, steps, encrypted secret env, `operator_can_run`, schedule `next_fire_at`), `workflow_runs` (step snapshot, trigger ref with a unique dedup index, context, status, current step, durable timer `next_at`), `workflow_actions` (one record per dispatch: status, fence term + leader, exit code, redacted output, reconciled_by). All replicated through control-plane commands (`workflow.create/update/delete/fire`, `run.create/progress`, `runs.prune`).
* **Engine** (`internal/automation`) — runs only on the active controller; manual, incident (status + monitor/group/tag filters) and schedule triggers; steps: script, HTTP, durable wait, incident status update; per-step failure policy and retries after definite failures only; cancel; context and `{{placeholders}}`; secrets sealed with the cluster data key and redacted from output.
* **Fencing** — `cluster.Manager.Fence` (Raft leader + `VerifyLeader` + mesh gate + term); dispatch record committed through Raft before dispatch, fence re-checked after it; outcome and next run state committed together; graceful shutdown leaves in-flight records for the next leader.
* **Reconciliation** — records left `dispatching` are resolved by the next leader from the runner's per-action record (`reconciled_*` by `runner`), or marked UNKNOWN_OUTCOME (HTTP steps, runner has no record, runner interrupted). Never replayed. Operators reconcile with a required note (audited).
* **Action Runner** (`intrabin runner`, `Dockerfile.runner`) — separate process/container with Bash + Python, bearer token, per-action job directory, minimal environment, rlimits via a re-exec wrapper (`runner-exec`), process-group kill on timeout, output cap, bounded concurrency, durable results + `started` markers keyed by action ID (dedup, `interrupted` instead of re-run).
* **HTTP steps** — SSRF guard at connect time (private ranges opt-in per step, metadata always blocked), no proxy, no redirects, expected status patterns; connect failures are definite failures, failures after connecting are unknown outcomes.
* **API** — workflows CRUD, manual run (operators only where allowed), runs list/detail with actions, cancel, reconcile, `/automation/status`; OpenAPI updated (50 paths). All writes audited.
* **UI** — Automation page (runner status, active/unreconciled counts, workflows and runs tabs), workflow editor (trigger, steps with reordering, env/header editors, write-only secrets, opt-in warning), workflow detail with Run now, run detail with step timeline, fence info, output, cancel and reconcile dialog. EN + FA, light/dark.
* **Deployment** — runner service in `compose.yaml` (profile `automation`) and in the 3-node test Compose (one runner shared by all nodes).

## Completed components (Phase 2: clustering)
* **Control-plane write path** (`internal/controlplane`) — every replicated mutation is a typed command with its own timestamp, executed locally (standalone) or through Raft (cluster). Node-local settings (`node.*`, `local.*`) are never replicated. Replay test shows replicas converge.
* **Consensus** (`internal/cluster`) — hashicorp/raft v1.8 with pre-vote, raft-boltdb, file snapshots (gzip+gob dump of replicated tables; recent round history only), seeding of pre-existing data at cluster creation, applied-index persistence for safe restarts.
* **Security** — cluster CA (ECDSA P-256), per-node certificates (CN = node ID), TLS 1.3 mutual auth on one cluster port multiplexed by ALPN (Raft / peer API), membership check on peer API calls, single-use expiring revocable join tokens (hash stored, replicated deterministic consumption, rate-limited join), CA fingerprint pinning during join, cluster data key transferred with X25519 key wrapping and sealed at rest with the node master key; monitor secrets re-encrypted on cluster creation.
* **Membership** — join as non-voter (UI on a node with data with export/discard choice, first-run join on fresh nodes, or env automation), promote (requires two-way reachability and catch-up), demote, remove; never automatic.
* **Roles & safety** — STANDALONE / LEADER / FOLLOWER / NO_QUORUM; strict mesh gate (fresh two-way reachability with a majority of voters) gates writes, evaluation and membership changes; leader-only evaluation; followers read-only with leader link; session/audit writes forwarded from followers (allow-list, read-your-writes).
* **Result shipping** — followers keep their raw results locally plus a durable outbox; batches shipped to the leader immediately/every second, deduplicated by ID, node ID checked against the client certificate.
* **Monitors** — `node_ids` selects which cluster nodes probe a monitor; node location labels count as independent locations for `min_locations`.
* **UI** — Cluster page: create cluster, join wizard with data choice, summary (leader, term, applied index), mesh gate status, members with reachability/last proof/applied index/buffered results, reachability matrix, promote/demote/remove with confirmations, join tokens (create shown once, list, revoke), warnings for 1/2/even voters and closed gate. First-run "join a cluster" path. Follower/No-quorum status pages with cluster view and leader link. No-quorum banner and live role badge. Monitor form "Probe from" node selector. EN + FA.
* **Test environments** — `tests/cluster/run_local.sh` (3 processes) and `tests/cluster/run_compose.sh` + `tests/cluster/compose.yaml` (3 containers + target).

## Completed components (Phase 1)
* **Storage** — SQLite WAL via pure-Go driver, embedded migrations, idempotent result inserts (UUID), round verdicts, hourly rollups, retention pruning, `VACUUM INTO` backups.
* **Monitoring engine** — HTTP/HTTPS (methods, headers, body, expected status patterns, body match, redirects, TLS verify toggle, origin address + Host/SNI override, latency degradation, cert expiry), TCP, DNS (A/AAAA/CNAME/MX/TXT/NS, expected value, custom resolver), TLS certificate monitor. Precise error classification. In-probe retries. Interval-aligned scheduler with per-monitor jitter and bounded worker pool.
* **Evaluation** — round verdicts (`ok/partial/failed/inconclusive/maintenance`), confirmations + min locations + min valid probes, consecutive vs rolling-window modes, recovery threshold, UNKNOWN after 3 inconclusive rounds, config-version freshness, catch-up after downtime (bounded).
* **Incidents** — persistent state machine with validated transitions, events timeline, dedup (one open incident per monitor), cooldown reopen, ack/resolve/notes via API, listener hook for automation.
* **Auth & security** — setup token bootstrap, Argon2id, sessions (hashed tokens, idle+absolute expiry), CSRF + Origin checks, rate limiting, RBAC (owner/admin/operator/viewer), last-owner/self protections, AES-256-GCM secrets with AAD, sensitive header auto-encryption, redaction, CSP & security headers, optional Host allow-list, trusted-proxy handling, offline `reset-password`.
* **API** — `/api/v1` (auth, setup, users, monitors, monitor-groups, results, uptime, stats, incidents, maintenance-windows, overview, audit-logs, settings, node, cluster, health, readiness), uniform errors, OpenAPI 3.1 spec, `/metrics`.
* **UI** — React 19 + TS + Tailwind 4 SPA embedded in the binary: setup, login, overview dashboard, monitors list/filter, monitor create/edit with live test and policy summary, monitor details (uptime periods with coverage, 90-day bars, latency chart, rounds strip, status codes, error categories, per-node, recent checks, incidents, certificate), groups, incidents + timeline, maintenance, cluster/node page, audit log, users & roles, settings (profile, password, appearance, instance, retention). Persian RTL with Vazirmatn and Jalali dates via `Intl`, English LTR, light/dark/system themes, keyboard focus styles, responsive layout. Restricted follower view (activates when the node is not the controller).
* **Deployment** — multi-stage Dockerfile (distroless, non-root, 7.4 MB content), Compose with read-only FS, dropped caps, localhost bind, healthcheck, log rotation; `.env.example`; Makefile.

## Test results (2026-11-02, Phase 3)
* `go vet ./...` clean; `go test -race ./...`: **67 tests passed, 0 failed**. New in Phase 3:
  * `runner` — Bash and Python execution, exactly one execution for 5 concurrent + 1 later request with the same action ID, timeout kills background children (process group), output cap, runner environment not leaked, rlimits applied, interrupted action never re-run, HTTP API auth (401), `LD_PRELOAD` rejected, unknown action 404.
  * `automation` — script run with context, env templating and secret redaction; retries then continue; stop on failure; durable wait survives an engine restart; no action record and no dispatch without a fence; follower does nothing; a `dispatching` script record is recovered from the runner without re-execution; an HTTP record becomes UNKNOWN_OUTCOME, is not replayed, and continues after operator reconciliation (double reconcile refused); SSRF guard blocks loopback unless allowed, templating of URL/body/headers with secrets; address policy table; incident trigger matching by tag, dedup of repeated notifications, disabled workflows ignored; cancel; schedule fires once per slot; connection refused = definite failure; accepted-but-silent receiver = unknown outcome.
  * `cluster` — `TestFailoverDuringActionYieldsUnknownOutcome`: 3 voters, leader fence vs follower fence, leader killed while an HTTP action is in flight → new leader records UNKNOWN_OUTCOME, replicated to both survivors, receiver hit exactly once (run 3× with `-race`).
  * `api` — workflow validation (fields), secrets write-only and kept on update, disabled by default, operator restrictions, run/cancel/list, busy workflow cannot be deleted, cancelled run stays cancelled, status endpoint, reconcile of unknown action → 404.
* Playwright (Chromium): **12/12 passed** — previous suite plus: create a script workflow with env and a secret in the UI, run it on the Action Runner, output shown with the secret redacted; build an HTTP + incident-status workflow, opt-in warning shown, run against a receiver that never answers → UNKNOWN_OUTCOME banner (EN and FA), reconcile → run succeeds, receiver contacted exactly once.
* Cluster e2e, 3 local processes + shared runner (`tests/cluster/run_local.sh`): **passed** — Phase 2 scenario plus: a 10 s script dispatched on the leader, leader killed mid-script, new leader records `reconciled_succeeded` from the runner, and a second workflow confirms the script executed exactly once.
* Cluster e2e, 3-node Docker Compose + runner container (`tests/cluster/run_compose.sh`): **passed** (same scenario). Images built with `tests/cluster/build_prebuilt.sh`; the runner image used `RUNTIME_BASE=mirror.gcr.io/library/python:3.13-slim-bookworm` because Docker Hub rate-limits pulls here. The multi-stage `Dockerfile.runner` was not built in this environment (same Go build stage as the main Dockerfile).
* Phase 1–2 results remain valid. `govulncheck` could not run here (network policy); CI runs it.

## Known defects / limitations
* Cluster: no automated recovery after permanent loss of a majority of voters; no CRL or automatic node-certificate renewal (1-year certificates); `reset-password` refuses to run on cluster members.
* Cluster: elections are stock Raft with pre-vote; the mesh gate guards actions and writes, not campaigning (hashicorp/raft offers no veto hook).
* Cluster: snapshots carry 48 h of round verdicts and 35 days of hourly rollups; a node restored from a snapshot can lack older history.
* Cluster: sessions are replicated, but browser cookies are per host, so signing in on one node's URL does not sign you in on another node's URL.
* TOTP 2FA not implemented. Login throttling state is in-memory.
* `/monitors/test` uses only credentials typed into the form, not stored secrets of an existing monitor.
* Uptime 24 h per monitor in list/overview is computed with one query per monitor (fine for hundreds, should be batched for thousands).
* Charts keep a left-to-right time axis in RTL by design.
* No master-key rotation command yet.
* Automation: an incident-triggered run is created right after the incident transition commits; a crash between the two commits loses that trigger. No approval step type yet. Script output and server-side error texts are English only. Scripts have the runner container's network access (restrict egress at the network level). The runner's HTTP API is plain HTTP unless TLS is configured on it.

## Next implementation steps (Phase 4: DNS providers)
1. Provider interface (list zones/records, get, create/update/delete with optimistic checks) and a credential store per integration (encrypted with the cluster data key, write-only).
2. Cloudflare and ArvanCloud implementations against their documented APIs, exercised only through mocks/fake servers in tests (no real accounts).
3. Snapshot-before-change and rollback for DNS records; dry-run diff in the UI.
4. A `dns` workflow step that goes through the same fencing/UNKNOWN_OUTCOME protocol (idempotent "ensure record = value" semantics where the provider allows checking state).
