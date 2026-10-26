-- 0001_init.sql (telemetry plane)
-- Append-heavy observability state: spans, trace feedback and experiment
-- result rows. This file can be backed up, vacuumed or retained independently
-- from the control plane.

CREATE TABLE IF NOT EXISTS plane_meta (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS spans (
  id TEXT PRIMARY KEY,
  trace_id TEXT NOT NULL,
  parent_id TEXT,
  name TEXT NOT NULL,
  type TEXT NOT NULL DEFAULT 'task',
  model TEXT,
  input TEXT, output TEXT, error TEXT, metadata TEXT,
  start_ms INTEGER NOT NULL, end_ms INTEGER NOT NULL, latency_ms INTEGER NOT NULL,
  prompt_tokens INTEGER DEFAULT 0, completion_tokens INTEGER DEFAULT 0,
  cost_usd REAL DEFAULT 0,
  status TEXT NOT NULL DEFAULT 'ok',
  project_id TEXT NOT NULL,
  created_at TEXT NOT NULL,
  token_source TEXT DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_spans_project ON spans(project_id);
CREATE INDEX IF NOT EXISTS idx_spans_trace ON spans(project_id, trace_id);

CREATE TABLE IF NOT EXISTS feedback (
  id TEXT PRIMARY KEY, trace_id TEXT, span_id TEXT, kind TEXT NOT NULL,
  value TEXT, comment TEXT, created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_feedback_trace ON feedback(trace_id, created_at DESC);

CREATE TABLE IF NOT EXISTS eval_runs (
  id TEXT PRIMARY KEY, experiment_id TEXT NOT NULL, record_id TEXT,
  input TEXT, expected TEXT, output TEXT, error TEXT,
  latency_ms INTEGER, scores TEXT, output_mode TEXT DEFAULT '', created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_eval_runs_experiment ON eval_runs(experiment_id, created_at);
