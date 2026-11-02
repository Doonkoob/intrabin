-- Phase 4: DNS provider integrations (replicated control-plane state).

CREATE TABLE dns_providers (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  type TEXT NOT NULL,                -- cloudflare | arvancloud
  credentials BLOB NOT NULL,         -- sealed with the cluster data key
  created_by TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  last_verified_at INTEGER,
  last_error TEXT NOT NULL DEFAULT ''
);

CREATE TABLE dns_snapshots (
  id TEXT PRIMARY KEY,
  provider_id TEXT NOT NULL REFERENCES dns_providers(id) ON DELETE CASCADE,
  zone_id TEXT NOT NULL,
  zone_name TEXT NOT NULL,
  reason TEXT NOT NULL DEFAULT '',
  records TEXT NOT NULL,             -- JSON array of records at snapshot time
  created_by TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL
);
CREATE INDEX dns_snapshots_zone ON dns_snapshots(provider_id, zone_id, created_at DESC);

CREATE TABLE dns_changes (
  id TEXT PRIMARY KEY,
  provider_id TEXT NOT NULL REFERENCES dns_providers(id) ON DELETE CASCADE,
  zone_id TEXT NOT NULL,
  zone_name TEXT NOT NULL,
  action TEXT NOT NULL,              -- create | update | delete
  record_type TEXT NOT NULL,
  record_name TEXT NOT NULL,
  record_id TEXT NOT NULL DEFAULT '',
  before TEXT,                       -- JSON record before the change (null for create)
  after TEXT,                        -- JSON desired record (null for delete)
  status TEXT NOT NULL,              -- dispatching | applied | failed | unknown_outcome | reconciled_applied | reconciled_not_applied
  verified INTEGER NOT NULL DEFAULT 0,
  snapshot_id TEXT NOT NULL DEFAULT '',
  source TEXT NOT NULL DEFAULT '',   -- manual | workflow:<run> | revert:<change> | restore:<snapshot>
  fence_term INTEGER NOT NULL DEFAULT 0,
  fence_leader TEXT NOT NULL DEFAULT '',
  actor TEXT NOT NULL DEFAULT '',
  error TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  finished_at INTEGER
);
CREATE INDEX dns_changes_status ON dns_changes(status);
CREATE INDEX dns_changes_zone ON dns_changes(provider_id, zone_id, created_at DESC);
