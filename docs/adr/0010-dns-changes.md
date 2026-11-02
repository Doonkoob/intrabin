# ADR-0010: DNS integrations and safe record changes

Status: Accepted (Phase 4).

## Context
DNS is the main lever for failover, and a wrong or duplicated DNS change can take a service down.
Provider APIs are not transactional, do not offer compare-and-set on records, and a write can succeed
at the provider while its response is lost. Changes can come from an operator in the UI or from a
workflow step during an incident, in a cluster whose leader can change at any moment.

## Decisions
1. **Documented APIs only.** Cloudflare API v4 (`Authorization: Bearer`, `/zones`,
   `/zones/{id}/dns_records`, `/user/tokens/verify`) and ArvanCloud CDN API 4.0 (`Authorization: Apikey …`,
   `/domains`, `/domains/{domain}/dns-records`). Every request and response shape used was checked against
   the providers' published documentation; tests use an in-memory fake of exactly those shapes
   (`internal/dns/dnsmock`). Real accounts are never contacted by tests.
2. **Small editable surface.** Only A, AAAA, CNAME and TXT can be created, changed or deleted. Other record
   types are listed read-only. Record types cannot be changed in place. Provider-specific settings that we do
   not model (ArvanCloud weights, ports, upstream settings, IP filter mode) are preserved on update by
   re-sending the original fields.
3. **Credentials** are verified with the provider before they are stored, sealed with the cluster data key
   (AAD `dnsprovider:<id>`), replicated encrypted, never returned by the API and re-encrypted with the rest of
   the secrets when a cluster is created.
4. **Plan, then apply.** Every manual change is a dry run first. Applying sends the reviewed plan back; the
   server re-reads the zone and refuses with `dns_conflict` if the target record changed since. A record is
   matched by ID, or by type + name when that is unique (otherwise `dns_ambiguous`).
5. **Snapshot before change.** The zone's records are stored (replicated) before every write; snapshots
   can be restored with a dry-run plan. Deleting records that are not in the snapshot is opt-in.
   Snapshots are kept 90 days, and always the newest 20 per zone.
6. **Fencing and at-most-once writes** follow ADR-0009: fence, commit a `dispatching` change record, fence
   again, write once (writes are never retried by the HTTP client; reads are), commit the outcome.
   A workflow `dns` step uses its action ID as the change ID, so the next leader finds the change.
7. **Uncertain outcomes are settled by reading, never by re-sending.** A 4xx is a definite failure. A
   transport error, timeout or 5xx makes the outcome unknown: after a short delay the zone is read back and
   the change becomes `reconciled_applied` (requested state present), `reconciled_not_applied` (previous state
   still present) or stays `unknown_outcome` (neither, e.g. someone else changed the record) for an operator
   to reconcile with a note. Changes left `dispatching` by a node that stopped are settled the same way by
   the leader after two minutes. A workflow step that ends `reconciled_not_applied` counts as a definite
   failure, so the step's retries apply.
8. **Read-back verification.** After a successful write the zone is read again and the change is marked
   `verified` only if the record has the requested value.

## Consequences
* A change can be applied at most once per change record. Exactly-once is not claimed: an operator can
  still issue two different changes, and a provider-side change made between our read-back and our decision
  is reported as unknown rather than guessed.
* The conflict check narrows but does not close the window between our read and our write, because the
  providers offer no conditional update.
* Revert creates a new, separately fenced change from the stored before-state.
