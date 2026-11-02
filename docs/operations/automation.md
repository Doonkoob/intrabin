# Automation: workflows and the Action Runner

Workflows run on the **active controller**: the standalone node, or the cluster leader while the mesh
gate is open. Followers and nodes in NO_QUORUM never run anything.

## Triggers
| Trigger | Starts a run when… | Notes |
|---|---|---|
| Manual | someone presses **Run now** (or `POST /api/v1/workflows/{id}/run`) | Allowed even when the workflow is disabled. Operators need `operator_can_run`. |
| Incident | an incident of a matching monitor reaches one of the selected statuses (default CONFIRMED) | Filter by monitor, group or tag; empty filters match all monitors. One run per incident transition. |
| Schedule | every N minutes (≥ 1) | Missed slots (e.g. during a failover) are skipped, not replayed. |

New workflows are **disabled**. Automatic triggers only fire after you enable the workflow; the UI
warns when you do. Test with **Run now** first.

## Steps
* **Script** (Bash or Python 3) — runs in the Action Runner. Context is available as environment
  variables: `INTRABIN_INCIDENT_ID`, `INTRABIN_INCIDENT_STATUS`, `INTRABIN_INCIDENT_TITLE`,
  `INTRABIN_MONITOR_ID`, `INTRABIN_MONITOR_NAME`, `INTRABIN_MONITOR_TARGET`, `INTRABIN_RUN_ID`,
  `INTRABIN_ACTION_ID` (use it as an idempotency key towards your own systems), `INTRABIN_WORKFLOW_ID`.
  Workflow secrets are set under their own names. Exit code 0 is success.
* **HTTP request** — method, URL, headers, body with `{{placeholders}}` (`{{incident_id}}`,
  `{{monitor_name}}`, `{{run_id}}`, `{{secret.NAME}}`, …) in the path, query, headers and body.
  Expected status codes such as `2xx` or `200, 409`. Private network addresses are refused unless
  **Allow private network addresses** is ticked; cloud metadata addresses are always refused;
  redirects are not followed.
* **Wait** — a durable timer: survives restarts and failover.
* **Update incident** — moves the run's incident to ACKNOWLEDGED, RECOVERY_PENDING, RECOVERING,
  FAILED_OVER, RESOLVED or MANUAL_INTERVENTION (if the state machine allows it).

Each step has a failure policy (stop the run, or continue) and up to 5 retries. Retries only happen
after a **definite** failure (non-zero exit, unexpected HTTP status, connection refused).

## Unknown outcomes and reconciliation
If the leader changes, crashes or loses contact after an action was dispatched and before its result
was recorded, the outcome may be unknown. Intrabin **never repeats such an action automatically**:

* For scripts, the new leader asks the runner for its record of the action ID. If the runner has the
  result, it is recorded (`Reconciled: succeeded/failed`, by `runner`) and the run continues.
* Otherwise (HTTP steps, a runner that never saw the request, or a runner restarted mid-script) the
  action and the run go to **Unknown outcome**. Check the target system, then open the run and press
  **Reconcile**: choose whether it took effect and explain how you verified it (audited). "Succeeded"
  continues with the next step; "failed" applies the step's failure policy without retrying.

The Automation page shows how many runs need reconciliation. Workflows with active or unreconciled
runs cannot be deleted.

## Running the Action Runner
The runner is the same binary in another mode, shipped as a separate image with Bash and Python:

```bash
# single node (Compose): set both values in .env, then
#   INTRABIN_RUNNER_URL=http://runner:8090
#   INTRABIN_RUNNER_TOKEN=$(openssl rand -hex 32)
docker compose --profile automation up -d --build
```

| Variable | Where | Meaning |
|---|---|---|
| `INTRABIN_RUNNER_URL` | intrabin | Runner base URL (http or https). Without it, script steps fail with a clear error; other steps work. |
| `INTRABIN_RUNNER_TOKEN` / `_FILE` | both | Shared bearer token, ≥ 24 characters. |
| `INTRABIN_RUNNER_LISTEN` | runner | Listen address (default `:8090`). |
| `INTRABIN_RUNNER_WORKDIR` | runner | Durable result store and job directories (keep it on a volume). |
| `INTRABIN_RUNNER_MAX_TIMEOUT` | runner | Upper bound for step timeouts (default `10m`). |
| `INTRABIN_TLS_CERT` / `_KEY` | runner | Serve HTTPS (recommended when the runner is not on the same private network). |

Isolation: separate container, non-root UID 10001, read-only root filesystem, all capabilities
dropped, `no-new-privileges`, PID and memory limits, no Docker socket and no access to Intrabin's
database or master key. Inside, each script gets its own temporary directory as `$HOME`/`TMPDIR`, a
minimal environment, rlimits (CPU 60 s, 512 MiB address space, 64 processes, 64 MiB files, 256 open
files, no core dumps), its whole process group is killed on timeout, and output is capped at 64 KiB
(16 KiB stored). Scripts **do** have the container's network access: restrict it with your container
network or firewall if scripts should only reach specific systems.

**Clusters:** point every node at the same runner (or a runner reachable from every node). Then a new
leader can recover the outcome of a script dispatched by the previous leader instead of marking it
unknown. `tests/cluster/compose.yaml` shows this layout.

## Permissions
| Role | Can |
|---|---|
| Viewer | see workflows, runs and (redacted) output |
| Operator | run and cancel workflows marked `operator_can_run` |
| Admin / Owner | create, edit, delete, run, cancel, reconcile |

All changes, runs, cancellations and reconciliations are written to the audit log.
