# ADR-0004: Raw monitoring results are not replicated through Raft

Status: Accepted

## Decision
Probe results are high-volume, append-only, and individually low value. They are shipped from
followers to the leader in batches with at-least-once delivery, identified by UUID for idempotent
insert, buffered durably on the follower while disconnected. Evaluated **monitor state** and
**incidents** are control-plane data and are committed through Raft.

## Trade-off
After a leader change some raw history (seconds) may be absent on the new leader; charts may show a
gap, which the UI reports as reduced *coverage* rather than downtime. Incident correctness is
preserved because counters and incident state are replicated.
