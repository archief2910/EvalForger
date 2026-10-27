-- 0005_guardrail_events.sql (telemetry plane)
-- One row per policy decision. Events live on the telemetry plane because
-- their volume scales with traffic, not with configuration.
--
-- The matched text is deliberately not stored. An event records what kind of
-- finding it was and a short, redacted excerpt: keeping the original would make
-- the guardrail itself the leak it exists to prevent.

CREATE TABLE IF NOT EXISTS guardrail_events (
  id TEXT PRIMARY KEY,
  project_id TEXT NOT NULL,
  policy_id TEXT NOT NULL,
  policy_name TEXT NOT NULL,
  action TEXT NOT NULL,
  check_type TEXT NOT NULL,
  matched TEXT NOT NULL DEFAULT '',
  detail TEXT NOT NULL DEFAULT '',
  preview TEXT NOT NULL DEFAULT '',
  trace_id TEXT NOT NULL DEFAULT '',
  span_id TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_guardrail_events_project ON guardrail_events(project_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_guardrail_events_trace ON guardrail_events(project_id, trace_id);