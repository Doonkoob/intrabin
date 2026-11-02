# Implementation status

_Last updated: 2026-11-02. Keep this file accurate: a feature is "done" only when its backend works and it is tested._

## Summary
| Phase | State |
|---|---|
| 0 — Architecture & foundation | **Done** (architecture doc, 7 ADRs, threat model, build tooling, Dockerfile, Compose, CI workflow) |
| 1 — Functional standalone product | **Done** (see below; verified by Go tests, Playwright UI tests and a Compose smoke test) |
| 2 — Distributed nodes / Raft | **Done** (see below; verified by in-process 3-node tests, a 3-process e2e test and a 3-node Docker Compose failover test) |
| 3 — Automation / workflows / runner | Not started (incident listener hook exists: `incidents.Manager.OnTransition`) |
| 4 — Cloudflare / ArvanCloud DNS | Not started |
| 5 — Disaster-recovery workflows | Not started (incident states RECOVERY_PENDING…FAILED_OVER exist in the state machine) |
| 6 — Hardening & release | Partial (CI workflow, docs, license) |

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

## Test results (2026-11-02)
* `go vet ./...` clean; `go test -race ./...`: **42 tests passed, 0 failed**. New in Phase 2:
  * `cluster` — `TestClusterLifecycle` (create cluster from a node with data and an encrypted secret → join fresh node → single-use token rejected on reuse → node with data refused without a choice → join with export → replicated data and decryptable secrets on followers → promotion → followers reject management writes but forward sessions with read-your-writes → follower results shipped and outbox drained → leader shut down → new leader elected and accepts writes → old leader restarts, catches up and stays follower), `TestJoinRejectsWrongCluster` (wrong CA fingerprint, wrong secret; node stays standalone), `TestQuorumLossStopsWrites` (survivor goes NO_QUORUM, writes refused, gate closed). Run 5× with `-race` without failures.
  * `controlplane` — replicas converge after replaying a recorded command stream (including conflicts, idempotent round recording, local settings excluded); error codes round-trip.
  * `api` — cluster endpoints on a standalone node (setup/join requires the setup token, malformed tokens, tokens refused when not clustered, join is owner-only, data_action validated).
* Playwright (Chromium): **10/10 passed** — the Phase 1 suite plus creating a cluster in the UI, issuing a join token (shown once) and revoking it.
* Cluster e2e, 3 local processes (`make test-cluster`): **passed** — setup, create cluster, join two fresh nodes through the first-run join API, token reuse refused, promote both, 3 voters with open gate, a monitor probed by all 3 nodes and UP, follower login and read-only management with leader link, kill the leader → new leader within seconds, writes and evaluation continue, old leader restarts as follower and catches up, stop two nodes → survivor NO_QUORUM and refuses writes, restore → leader re-elected.
* Cluster e2e, 3-node Docker Compose (`tests/cluster/run_compose.sh`): **passed** (same scenario with containers stopped/started). The image for this run was built from a host-built static binary (`tests/cluster/build_prebuilt.sh`) because Docker Hub rate-limited pulls of the Go build image; the multi-stage Dockerfile itself was verified in Phase 1.
* Phase 1 results remain valid (Compose smoke test, live incident checks).
* `govulncheck` could not run in this environment (vulnerability database blocked by the network policy); CI runs it.

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

## Next implementation steps (Phase 3: automation)
1. Durable scheduler and workflow engine on the control plane (workflow definitions, runs, steps and timers as replicated commands).
2. Action fencing: record `(term, leader_id, action_id)` through Raft before dispatch, re-check leadership and the mesh gate right before dispatch, `UNKNOWN_OUTCOME` for uncertain results, reconciliation instead of replay.
3. Isolated Action Runner container (Bash/Python, non-root, rlimits, no Docker socket) with an authenticated API.
4. Incident-triggered workflows via `incidents.Manager.OnTransition`; webhook/HTTP actions with SSRF controls.
5. UI: workflow builder, run history, approvals.
