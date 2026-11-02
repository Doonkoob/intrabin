-- Per-node hourly probe aggregates (latency average per monitor and monitoring node).
-- Written with each recorded round, so every node holds the same values.
CREATE TABLE monitor_node_hourly (
  monitor_id TEXT NOT NULL REFERENCES monitors(id) ON DELETE CASCADE,
  node_id TEXT NOT NULL,
  hour INTEGER NOT NULL,
  probes INTEGER NOT NULL DEFAULT 0,
  failures INTEGER NOT NULL DEFAULT 0,
  latency_sum REAL NOT NULL DEFAULT 0,
  latency_count INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (monitor_id, node_id, hour)
);
CREATE INDEX monitor_node_hourly_hour ON monitor_node_hourly(hour);
