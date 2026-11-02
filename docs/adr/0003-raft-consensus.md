# ADR-0003: hashicorp/raft for control-plane consensus (Phase 2)

Status: Accepted (implementation in Phase 2)

## Decision
Use `github.com/hashicorp/raft` (used by Consul, Nomad, Vault) with `raft-boltdb` for the log and
stable store and a file snapshot store. The FSM applies typed control-plane commands to SQLite.
Snapshots are SQLite online backups (`VACUUM INTO`). Membership changes use `AddNonvoter`,
`AddVoter`, `RemoveServer` — all through consensus.

An application-level **strict mesh gate** (fresh bidirectional reachability with a majority of
configured voters) is layered on top for (a) allowing a node to campaign and (b) allowing any
external mutation. It never lowers Raft's majority requirement.

## Rejected
etcd embed (heavier, gRPC-centric), dqlite/rqlite (CGO or separate process), custom voting (unsafe).
