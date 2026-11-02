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
| Control plane | users, sessions, monitors, policies, groups, monitor state, **round verdicts + hourly rollups**, incidents, maintenance, audit, settings, enrollment tokens (later: workflow state, timers, DNS snapshots) | Committed through Raft in a cluster (ADR-0008); local SQLite transactions when standalone | SQLite, written only through `internal/controlplane` |
| Raw monitoring data | probe results | At-least-once delivery to the leader, idempotent by `result_id` | SQLite `check_results` on each node (own results) and on the leader (all results); followers buffer unsent IDs in `result_outbox` |

Raw probe results are **not** written to the Raft log: at 30 s intervals × N monitors × M
nodes this would dominate log size and snapshot time. Instead followers push batches to the
leader over an authenticated channel, with local durable buffering and dedup by UUID. A new
leader may miss a few seconds of raw history, which only affects per-probe views; round verdicts,
monitor state and incidents are replicated (ADR-0004, ADR-0008).

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

## 5. Cluster design (Phase 2, implemented)

```
   node1 (LEADER)                       node2 (FOLLOWER)                    node3 (FOLLOWER)
 ┌──────────────────┐   Raft log      ┌──────────────────┐               ┌──────────────────┐
 │ API ─▶ controlplane ─▶ raft.Apply ─▶│ FSM ─▶ SQLite     │               │ FSM ─▶ SQLite     │
 │ FSM ─▶ SQLite     │◀── results ─────│ engine (probes)  │──── results ─▶│ engine (probes)  │
 │ engine: probes +  │   (mTLS, outbox)│ outbox           │               │ outbox           │
 │ round evaluation  │◀──── ping ─────▶│ mesh pinger      │◀──── ping ───▶│ mesh pinger      │
 └──────────────────┘                  └──────────────────┘               └──────────────────┘
          cluster port 7443: TLS 1.3, ALPN "intrabin-raft" (Raft streams) or HTTP (peer API)
```

* **Consensus**: `hashicorp/raft` (pre-vote enabled) with `raft-boltdb` log/stable store and file
  snapshots under `data/raft/`. No custom voting logic. The FSM executes control-plane commands
  (ADR-0008). Heartbeat/election timeout 1 s, leader lease 500 ms.
* **Transport**: one TCP port multiplexed by ALPN. Raft streams require a verified client
  certificate; the peer API (`/cluster/v1/join|ping|apply|results`) requires a member certificate
  except for `join`, which is authenticated by the enrollment token.
* **Enrollment**: cluster CA (ECDSA P-256, 10 y) created at cluster creation; its key is replicated
  sealed with the cluster data key so any future leader can sign. Join tokens embed the cluster ID,
  token ID, secret, CA fingerprint and voter addresses. New members join as **non-voters**; admins
  promote them after they are reachable and caught up.
* **Roles**: `STANDALONE`, `LEADER` (Raft leader with an open mesh gate), `FOLLOWER` (fresh contact
  with a leader), `NO_QUORUM` (anything else). Only `STANDALONE` and `LEADER` serve management APIs
  and evaluate rounds; every node probes.
* **Strict mesh gate**: each node pings every member each second; a peer counts when both our ping to
  it and its ping to us are fresh (≤ 5 s). The leader commits writes, evaluates and changes membership
  only while `1 + reachable voters ≥ ⌊voters/2⌋ + 1`. The gate is in addition to Raft's majority and
  is never relaxed automatically. Elections are stock Raft (no veto hook exists in hashicorp/raft).
* **Result shipping**: followers insert their results locally, record IDs in `result_outbox`, and
  ship batches (≤ 500) to the leader immediately and every second; the leader accepts only results
  whose `node_id` matches the client certificate, dedups by ID and acknowledges. A node that becomes
  leader clears its outbox (its results are already local).
* **Fencing for external actions** (Phase 3+): actions will record `(term, leader_id, action_id)`
  through the log before dispatch and re-check leadership and the gate immediately before dispatch;
  uncertain outcomes become `UNKNOWN_OUTCOME`. Control-plane writes are already fenced by Raft itself.

Sizing: 1 node = no HA; 2 voters tolerate 0 failures; 3 voters tolerate 1; 5 voters tolerate 2.
Three voters is the recommended HA deployment. Operations guide: [docs/operations/cluster.md](../operations/cluster.md).

## 6a. Automation (Phase 3, implemented — see ADR-0009 and docs/operations/automation.md)
* Workflows, runs (with a step snapshot), durable wait timers and per-dispatch **action records** are
  replicated control-plane state. `internal/automation.Engine` runs only on the active controller; it
  is kicked by commits (`run.create`, `workflow.*`, `state.restore`), role changes and a 1 s ticker.
* Triggers: incident transitions (`incidents.Manager.OnTransition`, deduplicated by a unique
  `(workflow_id, trigger_ref)` index), schedules (`workflow.fire` advances `next_fire_at` and creates the
  run in one command) and manual runs.
* External steps follow the fencing protocol: `Fence` (leader + `VerifyLeader` + mesh gate, term) →
  commit `dispatching(term, leader)` → `Fence` again → dispatch with the action ID → commit outcome and
  next run state together. Records left `dispatching` are reconciled by the next leader from the
  runner's durable per-action record, or marked UNKNOWN_OUTCOME — never re-dispatched.
* Scripts execute in `intrabin runner` (separate container, token auth, rlimit wrapper `runner-exec`,
  process-group kill, output cap, durable results and `started` markers keyed by action ID).

## 6. Security model (summary — see docs/security/threat-model.md)

Argon2id password hashing; random 256-bit session tokens stored hashed; HttpOnly SameSite cookies;
double-submit CSRF header; login rate limiting; one-time setup token printed to logs (no default
password); AES-256-GCM secrets at rest with a master key that never leaves the node in plaintext;
secrets are write-only through the API; structured JSON logs with redaction.
