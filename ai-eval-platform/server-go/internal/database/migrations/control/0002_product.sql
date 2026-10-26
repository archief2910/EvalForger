-- 0002_product.sql (control plane)
-- Reproducibility: immutable dataset snapshots, pinned prompt versions and a
-- database-backed feedback definition registry.

CREATE TABLE IF NOT EXISTS dataset_versions (
  id TEXT PRIMARY KEY,
  dataset_id TEXT NOT NULL,
  seq INTEGER NOT NULL,
  version_hash TEXT NOT NULL,
  items_json TEXT NOT NULL,
  items_total INTEGER NOT NULL DEFAULT 0,
  items_added INTEGER NOT NULL DEFAULT 0,
  items_modified INTEGER NOT NULL DEFAULT 0,
  items_deleted INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL,
  UNIQUE(dataset_id, version_hash)
);
CREATE INDEX IF NOT EXISTS idx_dataset_versions_dataset ON dataset_versions(dataset_id, seq DESC);

ALTER TABLE experiments ADD COLUMN dataset_version_id TEXT;
ALTER TABLE experiments ADD COLUMN prompt_version_id TEXT;

CREATE TABLE IF NOT EXISTS feedback_definitions (
  id TEXT PRIMARY KEY,
  project_id TEXT,
  name TEXT NOT NULL,
  kind TEXT NOT NULL,
  config TEXT,
  description TEXT DEFAULT '',
  created_at TEXT NOT NULL,
  UNIQUE(project_id, name)
);
CREATE INDEX IF NOT EXISTS idx_feedback_definitions_project ON feedback_definitions(project_id);
