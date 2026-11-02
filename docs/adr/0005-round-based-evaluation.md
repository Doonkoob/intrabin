# ADR-0005: Round-based multi-location evaluation

Status: Accepted

## Decision
Failure detection operates on interval-aligned **rounds**, not on individual probes. A round is
`failed` only when enough independent nodes confirm failure; missing data makes a round
`inconclusive`, which neither confirms downtime nor resets counters. Two explicit modes exist:
`consecutive` (N consecutive failed rounds) and `window` (N failed rounds within a rolling window).

## Rationale
Prevents counting six probes from three agents as six consecutive failures, and prevents a single
isolated agent from declaring an outage.
