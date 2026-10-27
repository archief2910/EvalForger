-- 0009_custom_scorers.sql (control plane)
-- User-authored metrics. The code is the only column in this schema that is
-- executed, so it is also the only one that needs a sandbox at run time.
--
-- Registration is not execution: storing a metric here says who wrote it and
-- what it is called, not that it is safe. The sandbox is what makes it safe,
-- and a metric with no reachable sandbox fails closed rather than running on
-- the host.

CREATE TABLE IF NOT EXISTS custom_scorers (
  id TEXT PRIMARY KEY,
  project_id TEXT NOT NULL,
  name TEXT NOT NULL,
  description TEXT NOT NULL DEFAULT '',
  code TEXT NOT NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  UNIQUE(project_id, name)
);
CREATE INDEX IF NOT EXISTS idx_custom_scorers_project ON custom_scorers(project_id);