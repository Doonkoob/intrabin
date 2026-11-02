# ADR-0008: How the control plane is replicated

Status: Accepted (Phase 2). Refines ADR-0003 and ADR-0004.

## Decisions

1. **One write path.** Every control-plane mutation goes through `internal/controlplane` as a typed,
   JSON-encoded `Command{op, at, data}`. Standalone nodes execute commands directly; cluster members
   commit them through `hashicorp/raft` and every node executes them in its FSM. Commands are
   *state-based* (full records, explicit IDs) and carry their own timestamp (`at`), so execution never
   reads the local clock or random sources and replicas converge. A unit test replays a recorded
   command stream on a second database and compares the result.
2. **What is replicated.** Users, sessions (create/touch/delete), monitor groups, monitors (including
   encrypted secrets), monitor state, maintenance windows, incidents and their events, audit entries,
   settings (except `node.*` and `local.*` keys), enrollment tokens and cluster node metadata.
   **Round verdicts and hourly rollups** are also committed through the log, one command per evaluated
   round, so a new leader has the full availability history (this refines ADR-0004, which only
   committed state and incidents). **Raw probe results** stay out of the log: followers store them
   locally and ship them to the leader (durable outbox, at-least-once, deduplicated by UUID).
3. **Snapshots** are a gzip+gob dump of the replicated tables. To bound snapshot size, round verdicts
   are included for 48 h and hourly rollups for 35 days; older history on a node that is restored
   from a snapshot is kept if present and otherwise absent (reported as reduced coverage).
4. **Seeding.** When a standalone node founds a cluster, its current state is committed as the first
   command (`state.restore`). Every later member replays the log from the beginning and therefore
   starts from identical data, whether or not a snapshot exists yet.
5. **Restart safety.** The FSM persists the last applied index. After a restart without a snapshot,
   entries up to that index are skipped; after a snapshot restore the index is reset.
6. **Writes on followers.** Management writes are rejected on followers (`node_read_only`, with a
   link to the leader). Only session create/touch/delete, last-login and audit entries are forwarded
   to the leader over mTLS (an allow-list), so users can sign in to any node's status page. The
   follower waits until it has applied the entry (read-your-writes).
7. **Secrets.** The founding node creates a random 256-bit cluster data key and re-encrypts existing
   monitor secrets with it. A joining node receives the data key wrapped with an ephemeral X25519
   key exchange (HKDF-SHA256 → AES-256-GCM) inside the pinned TLS channel and stores it sealed with its
   own master key. The master key never leaves a node; plaintext secrets are never sent between nodes.
8. **Mesh safety gate.** The leader evaluates rounds, commits writes and changes membership only while
   it holds fresh (≤ 5 s) two-way reachability proofs with a majority of voters, itself included.
   Elections use stock Raft with pre-vote; hashicorp/raft has no hook to veto a campaign, so the gate
   protects *actions*, not elections. A leader that loses the gate reports `NO_QUORUM` and stops
   acting even if Raft has not stepped it down yet.

## Consequences
* A cluster write costs one Raft round trip (~1–5 ms on a LAN). Round evaluation adds one command
  per monitor per interval (e.g. 500 monitors at 60 s ≈ 8 commands/s), well within Raft's capacity.
* Users must sign in again after the leader changes only if they were on a node that disappeared;
  sessions themselves are replicated.
* Membership changes are always explicit; nothing removes a voter or relaxes quorum automatically.
