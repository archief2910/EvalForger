-- 0010_guardrails.sql (control plane)
-- A guardrail policy is configuration: which checks run, what happens when one
-- trips, and whether the policy is on. It is deliberately separate from the
-- scorer registry, because a guardrail protects a request in flight while a
-- scorer judges it afterwards.

CREATE TABLE IF NOT EXISTS guardrail_policies (
  id TEXT PRIMARY KEY,
  project_id TEXT NOT NULL,
  name TEXT NOT NULL,
  enabled INTEGER NOT NULL DEFAULT 1,
  -- flag: record the finding and store the text as it arrived.
  -- redact: mask the matched values before they are stored.
  -- block: refuse the batch; the text never enters the store.
  mode TEXT NOT NULL DEFAULT 'flag',
  checks TEXT NOT NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  UNIQUE(project_id, name)
);
CREATE INDEX IF NOT EXISTS idx_guardrail_policies_project ON guardrail_policies(project_id, enabled);