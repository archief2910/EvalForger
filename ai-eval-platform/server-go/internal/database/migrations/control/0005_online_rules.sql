-- 0005_online_rules.sql (control plane)
-- Online evaluation: a rule watches live traffic and scores matching traces
-- automatically, instead of waiting for someone to promote a trace to a
-- dataset. Alerts are control state (low volume, acknowledged by a human), so
-- they live next to the rule that produced them.

CREATE TABLE IF NOT EXISTS online_rules (
  id TEXT PRIMARY KEY,
  project_id TEXT NOT NULL,
  name TEXT NOT NULL,
  enabled INTEGER NOT NULL DEFAULT 1,
  sample_rate DOUBLE PRECISION NOT NULL DEFAULT 1,
  filter_status TEXT NOT NULL DEFAULT '',
  filter_model TEXT NOT NULL DEFAULT '',
  filter_name TEXT NOT NULL DEFAULT '',
  scorers TEXT NOT NULL,
  alert_threshold DOUBLE PRECISION,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_online_rules_project ON online_rules(project_id, enabled);

CREATE TABLE IF NOT EXISTS online_alerts (
  id TEXT PRIMARY KEY,
  project_id TEXT NOT NULL,
  rule_id TEXT NOT NULL,
  trace_id TEXT NOT NULL,
  scorer TEXT NOT NULL,
  score DOUBLE PRECISION,
  threshold DOUBLE PRECISION NOT NULL,
  status TEXT NOT NULL DEFAULT 'open',
  created_at TEXT NOT NULL,
  acknowledged_at TEXT,
  UNIQUE(rule_id, trace_id, scorer)
);
CREATE INDEX IF NOT EXISTS idx_online_alerts_project ON online_alerts(project_id, status, created_at DESC);
