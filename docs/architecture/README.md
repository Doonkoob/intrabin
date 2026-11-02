# Intrabin Architecture

Intrabin is a single self-hosted Go binary (with an embedded React UI) that monitors
services, detects incidents with multi-location confirmation, and — in later phases —
runs durable recovery workflows. Every installation runs the **same** software; its
effective role (STANDALONE, FOLLOWER/OBSERVER, LEADER) is derived at runtime.

## 1. Process layout

```
                 ┌──────────────────────── intrabin (one process) ───────────────────────┐
 Browser ──TLS──▶│ HTTP API /api/v1  ─ auth/RBAC/CSRF ─ services ─ storage (SQLite, WAL) │
                 │ Embedded web UI (React, static)                                         │
                 │ Probe scheduler ─▶ checkers (HTTP/HTTPS/TCP/DNS/TLS) ─▶ result store    │
                 │ Round evaluator ─▶ monitor state ─▶ incident state machine              │
                 │ [Phase 2] Raft (hashicorp/raft) ─ control-plane FSM ─ peer mTLS          │
                 │ [Phase 3] Durable scheduler / workflow engine ─▶ Action Runner (gRPC/HTTP)│
                 └──────────────────────────────────────────────────────────────────────────┘
                                                        │ (Phase 3, separate container)
                                                        ▼
                                         intrabin-runner: Bash/Python, non-root, rlimits
```

The web/API process never executes user-supplied scripts. Scripts run in a separate
`runner` container with no Docker socket, read-only root FS, and dropped capabilities.

## 2. Data classes

| Class | Examples | Consistency | Store |
|---|---|---|---|
| Control plane | users, roles, monitors, policies, groups, incidents, workflow state, timers, DNS snapshots, encrypted secrets, audit of sensitive actions | Linearizable via Raft (Phase 2). Standalone: local SQLite transactions. | SQLite `control` tables, written only through the control-plane service |
| Monitoring data | probe results, round verdicts, hourly rollups | Eventually consistent, at-least-once delivery with idempotent `result_id` | SQLite `check_results`, `round_results`, `monitor_hourly` on the evaluating (leader) node; followers buffer locally |

Raw probe results are **not** written to the Raft log: at 30 s intervals × N monitors × M
nodes this would dominate log size and snapshot time. Instead followers push batches to the
leader over an authenticated channel, with local durable buffering and dedup by UUID. A new
leader may miss a few seconds of raw history, which only affects charts; incident state and
monitor state (counters) are control-plane data and are replicated (see ADR-0004).

## 3. Monitoring semantics (core correctness)

* **Probe**: one execution of a check by one node (with in-probe retries). Result has an
  error *category* — `http_5xx`, `unexpected_status`, `body_mismatch`, `timeout`,
  `dns_failure`, `connection_refused`, `network_unreachable`, `tls_error`,
  `cert_expiring`, `connection_reset`, `unknown`. A timeout is never reported as HTTP 5xx.
* **Round**: the interval-aligned time bucket `[k·interval, (k+1)·interval)`. Each node
  contributes at most one probe per round (latest wins).
* **Round verdict** (evaluated after the round closes + grace):
  * `inconclusive` — fewer than `min_valid_probes` fresh probes. Missing data is *not* failure.
  * `failed` — failing probes ≥ `confirmations_required` **and** distinct failing locations ≥ `min_locations`.
  * `partial` — some probes failed, but not enough independent confirmation (e.g. one isolated agent).
  * `ok` — all valid probes succeeded.
* **Monitor state** from round verdicts:
  * `fail_mode=consecutive`: `DOWN` after `failure_rounds` consecutive *failed rounds* (not probes).
    Inconclusive rounds neither advance nor reset the counter.
  * `fail_mode=window`: `DOWN` when ≥ `failure_rounds` failed rounds exist within the last
    `window_seconds` (a rolling window, explicitly different from "consecutive").
  * `SUSPECTED_DOWN` while failing but below threshold; `DEGRADED` on `partial` or slow responses;
    `UP` after `recovery_rounds` consecutive ok rounds; `UNKNOWN` after 3 inconclusive rounds;
    `PAUSED` when disabled; `MAINTENANCE` inside a maintenance window (no incidents opened).
* **Availability vs coverage**: uptime % = ok+partial rounds / conclusive rounds; coverage % =
  conclusive rounds / expected rounds. Both are shown.

Example from the requirements: interval 30 s, 3 nodes, `confirmations_required=2`,
`failure_rounds=6` → incident is confirmed only after six consecutive 30-second rounds in which
at least two nodes failed — never after six probes from three agents within two rounds.

## 4. Incident state machine

```
             failed round                 failure threshold met
 (none) ───────────────▶ SUSPECTED ───────────────────────────▶ CONFIRMED ──ack──▶ ACKNOWLEDGED
                            │ recovered before confirm               │                 │
                            ▼                                        ▼                 ▼
                         RESOLVED (cleared_before_confirmation)   RECOVERY_PENDING ─▶ RECOVERING ─▶ FAILED_OVER
                                                                     │  (Phase 3/5 workflows)        │
                    MANUAL_INTERVENTION ◀── workflow escalation ─────┘                               │
                                    recovery threshold met / manual resolve ──▶ RESOLVED ◀───────────┘
```

Transitions are validated by a pure function (`incidents.CanTransition`) and every transition is
persisted with an event row (actor, timestamp, reason). One open incident per monitor
(deduplication). A failure within `cooldown_seconds` of resolution reopens the same incident.

## 5. Cluster design (Phase 2, designed now)

* Consensus: `hashicorp/raft` with `raft-boltdb` log store and file snapshots; the FSM applies
  control-plane commands to the local SQLite DB. No custom voting logic.
* New nodes join as **non-voters**; admins promote to voter after sync + connectivity checks.
* **Strict mesh gate**: before a node starts an election (Raft pre-vote hook point) and before the
  leader dispatches any mutation, it requires fresh (≤ `mesh_freshness`) bidirectional reachability
  proofs with a majority of *configured voters*. The failed leader is not required to be reachable.
  This is an additional gate; Raft's own majority rule is never weakened.
* **Fencing**: every external mutation records `(term, leader_id, action_id)` durably *before*
  dispatch, checks `raft.VerifyLeader()` immediately before dispatch, and stops on leadership loss.
  In-flight actions with uncertain outcome become `UNKNOWN_OUTCOME` and are reconciled, never replayed.
* Peer transport: mutual TLS with per-node certificates issued by a cluster CA during enrollment.

Sizing: 1 node = no HA; 2 voters tolerate 0 failures; 3 voters tolerate 1; 5 voters tolerate 2.
Three voters is the recommended HA deployment.

## 6. Security model (summary — see docs/security/threat-model.md)

Argon2id password hashing; random 256-bit session tokens stored hashed; HttpOnly SameSite cookies;
double-submit CSRF header; login rate limiting; one-time setup token printed to logs (no default
password); AES-256-GCM secrets at rest with a master key that never leaves the node in plaintext;
secrets are write-only through the API; structured JSON logs with redaction.
