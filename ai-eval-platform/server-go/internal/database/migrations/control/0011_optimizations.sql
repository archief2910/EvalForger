-- 0011_optimizations.sql (control plane)
-- An optimization run searches for a better prompt. The inputs are pinned at
-- creation (dataset snapshot, baseline prompt version, scorers) so a run is
-- reproducible; the output is a new prompt version plus the scores that
-- justified choosing it.

CREATE TABLE IF NOT EXISTS optimizations (
  id TEXT PRIMARY KEY,
  project_id TEXT NOT NULL,
  name TEXT NOT NULL,
  dataset_id TEXT NOT NULL,
  dataset_version_id TEXT NOT NULL,
  baseline_prompt_version_id TEXT NOT NULL DEFAULT '',
  best_prompt_version_id TEXT NOT NULL DEFAULT '',
  scorers TEXT NOT NULL,
  max_candidates INTEGER NOT NULL DEFAULT 4,
  status TEXT NOT NULL DEFAULT 'queued',
  baseline_score DOUBLE PRECISION,
  best_score DOUBLE PRECISION,
  summary TEXT NOT NULL DEFAULT '',
  error TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_optimizations_project ON optimizations(project_id, created_at DESC);