-- Phase 5: disaster-recovery plans (replicated control-plane state).
-- A DR plan is backed by a workflows row (kind = 'dr') that holds its name, secrets and version,
-- so failover and failback runs use the workflow engine, its fencing and its history.
ALTER TABLE workflows ADD COLUMN kind TEXT NOT NULL DEFAULT 'workflow';

CREATE TABLE dr_plans (
  id TEXT PRIMARY KEY REFERENCES workflows(id) ON DELETE CASCADE,
  config TEXT NOT NULL DEFAULT '{}',
  state TEXT NOT NULL DEFAULT 'primary',      -- primary|failing_over|failed_over|failing_back|attention
  active_run_id TEXT NOT NULL DEFAULT '',
  direction TEXT NOT NULL DEFAULT '',          -- failover|failback of the active/last run
  state_note TEXT NOT NULL DEFAULT '',
  last_failover_at INTEGER,
  last_transition_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
