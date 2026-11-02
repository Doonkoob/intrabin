# ADR-0011: Disaster-recovery plans

Status: Accepted

## Context
Operators need to move a service from a primary to a secondary site, usually by changing one DNS
record, and back again. A wrong or duplicated switch is worse than a slow one: two nodes failing
over at once, a flapping primary that bounces traffic, or a failover to a secondary that is itself
down all cause outages.

## Decision
* A DR plan is replicated control-plane state (`dr_plans`, migration 0005) attached to a workflow
  row of kind `dr`. It is hidden from the ordinary workflow list and API.
* A plan has its own state machine: `primary → failing_over → failed_over → failing_back → primary`.
  A switch whose run fails, is cancelled or ends with an unknown outcome moves the plan to
  `attention`. Only a person leaves `attention`, by recording where traffic goes (no provider call)
  or by starting a new switch with an override.
* Starting a switch is a single Raft-committed compare-and-set on the plan state. The same command
  inserts the workflow run that does the work, so two nodes or two clicks cannot start two switches,
  and a switch never exists without its run.
* The run uses the existing engine and the `dns` step: every external action is fenced, an
  uncertain write is settled by reading the zone back and is never re-sent (ADR-0009, ADR-0010).
* Safety checks run before every switch and are recorded with it: active controller (leader +
  quorum confirmation), plan state, primary health, secondary monitor UP, cooldown since the last
  failover (default 30 min), and a DNS dry run. Blocking checks can be overridden only by a person,
  only where marked overridable, and only with a note. The controller and DNS checks are never
  overridable.
* Automatic failover is **opt-in per plan** and off by default. It runs only on the active
  controller, only for an incident the detection policy confirmed (or acknowledged), never during
  the cooldown, never when the secondary's monitor is not UP, and at most once per incident (the run
  trigger reference is unique). Overrides are never applied automatically. **Failback is always
  manual.**

## Consequences
* Split brain is bounded by Raft: a minority partition has no active controller, so it can neither
  start a switch nor dispatch the DNS write. A node that loses leadership mid-switch leaves the run
  to the next leader, which reconciles the DNS action from the zone.
* Without a secondary monitor the secondary check is skipped for manual switches; automatic
  failover cannot be enabled without one.
* There is no approval step type yet; a person approving a switch is modelled as starting it.
