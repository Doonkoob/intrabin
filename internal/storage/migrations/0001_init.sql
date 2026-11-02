CREATE TABLE settings (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL,
  updated_at INTEGER NOT NULL
);

CREATE TABLE users (
  id TEXT PRIMARY KEY,
  username TEXT NOT NULL UNIQUE COLLATE NOCASE,
  display_name TEXT NOT NULL DEFAULT '',
  password_hash TEXT NOT NULL,
  role TEXT NOT NULL CHECK (role IN ('owner','admin','operator','viewer')),
  disabled INTEGER NOT NULL DEFAULT 0,
  language TEXT NOT NULL DEFAULT 'en',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  password_changed_at INTEGER NOT NULL,
  last_login_at INTEGER
);

CREATE TABLE sessions (
  token_hash TEXT PRIMARY KEY,
  user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  csrf_token TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  last_seen_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL,
  ip TEXT NOT NULL DEFAULT '',
  user_agent TEXT NOT NULL DEFAULT ''
);
CREATE INDEX sessions_user ON sessions(user_id);

CREATE TABLE monitor_groups (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL UNIQUE,
  description TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);

CREATE TABLE monitors (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  description TEXT NOT NULL DEFAULT '',
  type TEXT NOT NULL,
  target TEXT NOT NULL,
  group_id TEXT REFERENCES monitor_groups(id) ON DELETE SET NULL,
  tags TEXT NOT NULL DEFAULT '[]',
  interval_seconds INTEGER NOT NULL,
  timeout_seconds INTEGER NOT NULL,
  retries INTEGER NOT NULL DEFAULT 0,
  enabled INTEGER NOT NULL DEFAULT 1,
  settings TEXT NOT NULL DEFAULT '{}',
  secret_settings BLOB,
  policy TEXT NOT NULL DEFAULT '{}',
  node_selector TEXT NOT NULL DEFAULT '[]',
  version INTEGER NOT NULL DEFAULT 1,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);

CREATE TABLE monitor_state (
  monitor_id TEXT PRIMARY KEY REFERENCES monitors(id) ON DELETE CASCADE,
  status TEXT NOT NULL,
  consecutive_failed INTEGER NOT NULL DEFAULT 0,
  consecutive_ok INTEGER NOT NULL DEFAULT 0,
  consecutive_inconclusive INTEGER NOT NULL DEFAULT 0,
  last_round INTEGER NOT NULL DEFAULT 0,
  last_verdict TEXT NOT NULL DEFAULT '',
  last_latency_ms REAL,
  status_since INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);

CREATE TABLE check_results (
  id TEXT PRIMARY KEY,
  monitor_id TEXT NOT NULL REFERENCES monitors(id) ON DELETE CASCADE,
  node_id TEXT NOT NULL,
  config_version INTEGER NOT NULL,
  round_start INTEGER NOT NULL,
  probe_at INTEGER NOT NULL,
  received_at INTEGER NOT NULL,
  success INTEGER NOT NULL,
  latency_ms REAL,
  http_status INTEGER,
  error_category TEXT NOT NULL DEFAULT '',
  error_message TEXT NOT NULL DEFAULT '',
  attempts INTEGER NOT NULL DEFAULT 1,
  cert_expires_at INTEGER,
  slow INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX check_results_monitor_time ON check_results(monitor_id, probe_at DESC);
CREATE INDEX check_results_monitor_round ON check_results(monitor_id, round_start);
CREATE INDEX check_results_probe_at ON check_results(probe_at);

CREATE TABLE round_results (
  monitor_id TEXT NOT NULL REFERENCES monitors(id) ON DELETE CASCADE,
  round_start INTEGER NOT NULL,
  verdict TEXT NOT NULL,
  valid_probes INTEGER NOT NULL,
  failing_probes INTEGER NOT NULL,
  failing_nodes TEXT NOT NULL DEFAULT '[]',
  avg_latency_ms REAL,
  evaluated_at INTEGER NOT NULL,
  PRIMARY KEY (monitor_id, round_start)
);
CREATE INDEX round_results_time ON round_results(round_start);

CREATE TABLE monitor_hourly (
  monitor_id TEXT NOT NULL REFERENCES monitors(id) ON DELETE CASCADE,
  hour INTEGER NOT NULL,
  ok_rounds INTEGER NOT NULL DEFAULT 0,
  partial_rounds INTEGER NOT NULL DEFAULT 0,
  failed_rounds INTEGER NOT NULL DEFAULT 0,
  inconclusive_rounds INTEGER NOT NULL DEFAULT 0,
  maintenance_rounds INTEGER NOT NULL DEFAULT 0,
  latency_sum REAL NOT NULL DEFAULT 0,
  latency_count INTEGER NOT NULL DEFAULT 0,
  expected_rounds INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (monitor_id, hour)
);

CREATE TABLE incidents (
  id TEXT PRIMARY KEY,
  monitor_id TEXT NOT NULL REFERENCES monitors(id) ON DELETE CASCADE,
  status TEXT NOT NULL,
  title TEXT NOT NULL,
  started_at INTEGER NOT NULL,
  detected_at INTEGER NOT NULL,
  confirmed_at INTEGER,
  acknowledged_at INTEGER,
  acknowledged_by TEXT,
  resolved_at INTEGER,
  resolution TEXT NOT NULL DEFAULT '',
  confirmation_sources TEXT NOT NULL DEFAULT '[]',
  last_error_category TEXT NOT NULL DEFAULT '',
  updated_at INTEGER NOT NULL
);
CREATE INDEX incidents_monitor ON incidents(monitor_id, started_at DESC);
CREATE INDEX incidents_status ON incidents(status);

CREATE TABLE incident_events (
  id TEXT PRIMARY KEY,
  incident_id TEXT NOT NULL REFERENCES incidents(id) ON DELETE CASCADE,
  at INTEGER NOT NULL,
  type TEXT NOT NULL,
  from_status TEXT NOT NULL DEFAULT '',
  to_status TEXT NOT NULL DEFAULT '',
  actor TEXT NOT NULL DEFAULT 'system',
  message TEXT NOT NULL DEFAULT '',
  details TEXT NOT NULL DEFAULT '{}'
);
CREATE INDEX incident_events_incident ON incident_events(incident_id, at);

CREATE TABLE maintenance_windows (
  id TEXT PRIMARY KEY,
  monitor_id TEXT REFERENCES monitors(id) ON DELETE CASCADE,
  title TEXT NOT NULL,
  starts_at INTEGER NOT NULL,
  ends_at INTEGER NOT NULL,
  created_by TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL
);
CREATE INDEX maintenance_windows_range ON maintenance_windows(starts_at, ends_at);

CREATE TABLE audit_logs (
  id TEXT PRIMARY KEY,
  at INTEGER NOT NULL,
  actor_id TEXT NOT NULL DEFAULT '',
  actor_name TEXT NOT NULL DEFAULT '',
  action TEXT NOT NULL,
  target_type TEXT NOT NULL DEFAULT '',
  target_id TEXT NOT NULL DEFAULT '',
  ip TEXT NOT NULL DEFAULT '',
  outcome TEXT NOT NULL DEFAULT 'success',
  details TEXT NOT NULL DEFAULT '{}'
);
CREATE INDEX audit_logs_at ON audit_logs(at DESC);
CREATE INDEX audit_logs_action ON audit_logs(action);
