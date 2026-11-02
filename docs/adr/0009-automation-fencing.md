# ADR-0009: Workflow engine, action fencing and the Action Runner

Status: Accepted (Phase 3).

## Context
Recovery automation acts on the outside world (scripts, webhooks, later DNS changes). In a cluster,
leadership can move at any moment, and an external system cannot be rolled back by Raft. We need
automation that survives failover without running an action twice by accident, and without letting
a node that has lost the majority act on its own.

## Decisions
1. **Everything durable is replicated.** Workflow definitions, runs (with a snapshot of the steps at
   creation), wait timers (`next_at`) and one *action record* per external dispatch are control-plane
   state, written through the same command path as everything else (ADR-0008). A new leader resumes
   runs from the log; a restart resumes them from the database.
2. **Only the active controller executes** (standalone node, or the Raft leader while the mesh gate is
   open). Followers and NO_QUORUM nodes never start, advance or dispatch anything.
3. **Fencing protocol for every external action:**
   1. `Fence`: the node is the Raft leader, `VerifyLeader` succeeds (a majority confirms it), the
      mesh gate is open; read the term.
   2. Commit an action record `dispatching` with `(term, leader node ID)` through Raft.
   3. `Fence` again; if the term or leader changed, do not dispatch (the record is marked
      `failed: not dispatched` when that write still succeeds).
   4. Dispatch with the action ID as the idempotency key.
   5. Commit the outcome together with the run's next state in one command.
   A fence is not a lease: a deposed leader can still be between steps 3 and 4. That window is why
   step 2 exists.
4. **Never replay an uncertain action.** A record left `dispatching` (leader died, network cut,
   shutdown) is resolved by the next leader:
   * script steps: the runner keeps a durable record per action ID. If it has a result, that result is
     recorded as `reconciled_succeeded/failed (by runner)`; if it is still running, the leader waits;
     if it never saw the action, or the runner itself was restarted mid-execution (`interrupted`),
     the action becomes **UNKNOWN_OUTCOME**;
   * HTTP steps: always **UNKNOWN_OUTCOME** (the receiver may or may not have acted). An HTTP request
     that failed to *connect* is a definite failure; one that failed after connecting is unknown.
   A run with an unknown action stops in `unknown_outcome` until an operator records what actually
   happened (`succeeded` → continue, `failed` → the step's failure policy, never an automatic retry).
   Retries only follow definite failures. We do not claim exactly-once external execution.
5. **Isolated Action Runner.** Scripts never run in the API/control-plane process. `intrabin runner`
   is a separate process (its own container image with Bash and Python, non-root, read-only root
   filesystem, no capabilities, no Docker socket, no access to the database or master key) with a
   bearer-token API. Each script gets a fresh working directory, a minimal environment, `setrlimit`
   limits applied by re-executing the binary as `runner-exec` before `exec`ing the interpreter (CPU,
   address space, processes, file size, open files, no core dumps), a process-group kill on timeout,
   an output cap and bounded concurrency. Results are stored durably by action ID, and a `started`
   marker is written before execution so a crashed runner reports `interrupted` instead of re-running.
   Sharing one runner between all nodes lets a new leader recover script outcomes after failover.
6. **HTTP steps and SSRF.** Every resolved address is checked at connect time (no DNS-rebinding gap):
   loopback, private, link-local and CGNAT ranges need `allow_private_network`; cloud metadata,
   multicast and unspecified addresses are always refused. Proxies are ignored and redirects are not
   followed.
7. **Opt-in automation.** New workflows are disabled; a disabled workflow can still be run manually.
   Enabling an incident or schedule trigger shows an explicit warning in the UI. Incident triggers are
   deduplicated by `(workflow, incident, status, transition time)`; schedules fire at most once per
   slot and skip missed slots rather than replaying them.
8. **Secrets** are encrypted with the cluster data key (AAD `workflow:<id>:secrets`), never returned by
   the API, passed to scripts as environment variables or to HTTP steps as `{{secret.NAME}}`, and
   redacted from stored output.

## Consequences
* An operator must reconcile `unknown_outcome` runs; the UI and `/automation/status` surface them.
* Runner-side deduplication depends on the runner's work directory surviving. If that volume is wiped
  while an action is in flight, the next leader marks the action UNKNOWN_OUTCOME (the runner has no
  record), but a delayed request from the old leader could still execute on the wiped runner. Keep
  the runner state on a persistent volume.
* Incident-triggered runs are created right after the incident transition commits; a crash between
  the two commits loses that trigger (documented limitation).
