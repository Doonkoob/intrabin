-- Phase 3: automation (all tables are replicated control-plane state).

CREATE TABLE workflows (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  description TEXT NOT NULL DEFAULT '',
  enabled INTEGER NOT NULL DEFAULT 0,
  trigger TEXT NOT NULL DEFAULT '{}',
  steps TEXT NOT NULL DEFAULT '[]',
  secret_env BLOB,
  secret_keys TEXT NOT NULL DEFAULT '[]',
  operator_can_run INTEGER NOT NULL DEFAULT 0,
  version INTEGER NOT NULL DEFAULT 1,
  next_fire_at INTEGER,
  created_by TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);

CREATE TABLE workflow_runs (
  id TEXT PRIMARY KEY,
  workflow_id TEXT NOT NULL REFERENCES workflows(id) ON DELETE CASCADE,
  workflow_version INTEGER NOT NULL,
  definition TEXT NOT NULL,          -- snapshot of the steps when the run was created
  trigger_type TEXT NOT NULL,
  trigger_ref TEXT NOT NULL DEFAULT '',
  context TEXT NOT NULL DEFAULT '{}',
  status TEXT NOT NULL,
  current_step INTEGER NOT NULL DEFAULT 0,
  next_at INTEGER,                   -- durable timer for waits
  created_by TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  started_at INTEGER,
  finished_at INTEGER,
  error TEXT NOT NULL DEFAULT '',
  updated_at INTEGER NOT NULL
);
CREATE INDEX workflow_runs_status ON workflow_runs(status);
CREATE INDEX workflow_runs_workflow ON workflow_runs(workflow_id, created_at DESC);
CREATE UNIQUE INDEX workflow_runs_dedup ON workflow_runs(workflow_id, trigger_ref) WHERE trigger_ref <> '';

-- One row per dispatch attempt of an external action (the action ID is the fencing record).
CREATE TABLE workflow_actions (
  id TEXT PRIMARY KEY,
  run_id TEXT NOT NULL REFERENCES workflow_runs(id) ON DELETE CASCADE,
  step_index INTEGER NOT NULL,
  step_type TEXT NOT NULL,
  attempt INTEGER NOT NULL DEFAULT 1,
  status TEXT NOT NULL,              -- dispatching|succeeded|failed|unknown_outcome|reconciled_succeeded|reconciled_failed
  fence_term INTEGER NOT NULL DEFAULT 0,
  fence_leader TEXT NOT NULL DEFAULT '',
  started_at INTEGER NOT NULL,
  finished_at INTEGER,
  exit_code INTEGER,
  output TEXT NOT NULL DEFAULT '',
  error TEXT NOT NULL DEFAULT '',
  reconciled_by TEXT NOT NULL DEFAULT ''
);
CREATE INDEX workflow_actions_run ON workflow_actions(run_id, step_index, attempt);
CREATE INDEX workflow_actions_status ON workflow_actions(status);
