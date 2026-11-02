# Disaster recovery (DR) plans

A DR plan moves one DNS record (A, AAAA or CNAME) between a **primary** and a **secondary** value,
for example from the main data centre's IP to a standby site's IP. Plans live under
**Disaster recovery** in the UI (`dr.manage` permission: owner and admin) and `/api/v1/dr-plans`.

## Before you create a plan
1. Add the DNS integration (Cloudflare or ArvanCloud) under **DNS** and check that the record is
   visible in the zone browser.
2. Create monitors for the primary site (one or more) and, strongly recommended, one for the
   secondary site.
3. Lower the record's TTL (60 s) well before you need it, so clients follow a switch quickly.

## States
| State | Meaning |
|---|---|
| On primary | The record points to the primary value. |
| Failing over / Failing back | A switch run is in progress (link to the run on the plan page). |
| On secondary | The record points to the secondary value. |
| Needs attention | The last switch failed, was cancelled or its outcome is unknown. Check the provider panel, then **Resolve** (records the state, changes nothing) or switch again. |

## Manual failover and failback
**Fail over** / **Fail back** opens a dialog that runs every safety check and a DNS dry run
without changing anything:

* **Active controller** — this node is the leader and a majority confirms it. Never overridable.
* **Plan state** — the plan is in the state the switch starts from.
* **Primary health** — informational for a manual failover (a planned failover is allowed); for a
  failback every primary monitor must be UP.
* **Secondary healthy** — the secondary's monitor is UP (or degraded).
* **Cooldown** — no failover within the cooldown (default 30 minutes).
* **DNS record change** — the provider dry run (`old → new`). Never overridable.

A failed overridable check can be overridden by ticking **Override the failed checks** and giving
a reason; the reason is stored with the switch and in the audit log. The switch runs as a workflow
run: the optional extra steps before the DNS change (API only), the DNS change (fenced, written
once, read back), the extra steps after, and incident status updates when the switch is tied to an
incident.

## Automatic failover (opt-in)
Off by default. When enabled on a plan, the active controller fails over by itself only when all of
these hold:

* an incident on one of the plan's primary monitors is open and was **confirmed** by the detection
  policy (several nodes / rounds agree, see the monitor's policy) or acknowledged;
* the secondary's monitor is UP (a secondary monitor is required to enable automatic failover);
* the cooldown since the last failover has passed;
* the plan is on primary and no other switch is running.

It fails over at most once per incident and never overrides a check. The plan list and page show
why an automatic plan is waiting. **Failback is always manual**, after you have verified the
primary and its data.

## Clusters and split brain
Only the Raft leader with quorum (the active controller) can start a switch or write to DNS. Nodes
in a minority partition cannot. Starting a switch is one replicated compare-and-set, so concurrent
attempts from two nodes or two browser tabs result in exactly one switch. If the leader dies during
a switch, the next leader finishes or reconciles the run; an uncertain DNS write is settled by
reading the zone, never by sending it again.

## Testing a plan
Point a test plan at a test record (for example `dr-test.example.com`) in the same zone and run a
failover and failback. The project's automated tests run against a fake of both provider APIs
(`tests/dnsmock`); they never touch real accounts.
