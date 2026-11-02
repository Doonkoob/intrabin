-- Phase 2: clustering.

-- Enrollment tokens (replicated). Only a SHA-256 hash of the secret is stored.
CREATE TABLE enrollment_tokens (
  id TEXT PRIMARY KEY,
  secret_hash TEXT NOT NULL,
  description TEXT NOT NULL DEFAULT '',
  created_by TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL,
  used_at INTEGER,
  used_by_node TEXT NOT NULL DEFAULT '',
  revoked_at INTEGER
);

-- Cluster member metadata (replicated). Raft's configuration remains the source of truth
-- for membership and suffrage; this table carries display names and addresses.
CREATE TABLE cluster_nodes (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  cluster_addr TEXT NOT NULL,
  api_url TEXT NOT NULL DEFAULT '',
  location TEXT NOT NULL DEFAULT '',
  version TEXT NOT NULL DEFAULT '',
  joined_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);

-- Local durable buffer of probe results not yet acknowledged by the leader (not replicated).
CREATE TABLE result_outbox (
  result_id TEXT PRIMARY KEY,
  queued_at INTEGER NOT NULL
);
