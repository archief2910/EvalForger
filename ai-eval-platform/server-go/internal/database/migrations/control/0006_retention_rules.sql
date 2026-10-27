-- 0006_retention_rules.sql (control plane)
-- Retention is a policy, not a job: the rule says how long a project keeps its
-- telemetry, and the sweeper enforces it on the telemetry plane. Keeping the
-- rule on the control side means changing the policy never touches the
-- append-heavy store, and the rule survives a telemetry rebuild.

CREATE TABLE IF NOT EXISTS retention_rules (
  id TEXT PRIMARY KEY,
  project_id TEXT NOT NULL,
  name TEXT NOT NULL DEFAULT '',
  retention_days INTEGER NOT NULL,
  apply_to_past INTEGER NOT NULL DEFAULT 1,
  enabled INTEGER NOT NULL DEFAULT 1,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_retention_rules_project ON retention_rules(project_id);
