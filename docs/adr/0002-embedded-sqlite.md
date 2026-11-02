# ADR-0002: Embedded SQLite (pure Go driver) for persistence

Status: Accepted

## Decision
Use SQLite in WAL mode via `modernc.org/sqlite` (pure Go, no CGO). One database file per node in
the data volume. Schema migrations are embedded and applied at startup in a transaction.

## Rejected
* PostgreSQL per node: extra service, and a local PostgreSQL does not provide HA by itself.
* Copying SQLite files between nodes: unsafe; replication must happen through consensus (ADR-0003).

## Consequences
Single-writer database; all writes are short transactions with `busy_timeout`. Adequate for
thousands of monitors at 30 s intervals (≈ tens of writes/s).
