-- 0002_threads.sql (telemetry plane)
-- Conversations are first-class: every span carries the thread it belongs to,
-- a thread owns its lifecycle (open/closed by activity), and scores can target
-- the conversation rather than one trace.

ALTER TABLE spans ADD COLUMN thread_id TEXT;
CREATE INDEX IF NOT EXISTS idx_spans_thread ON spans(project_id, thread_id, start_ms);
CREATE INDEX IF NOT EXISTS idx_spans_thread_trace ON spans(project_id, thread_id, trace_id);

CREATE TABLE IF NOT EXISTS threads (
  project_id TEXT NOT NULL,
  id TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'open',
  started_at_ms INTEGER NOT NULL,
  last_activity_ms INTEGER NOT NULL,
  ended_at_ms INTEGER,
  trace_count INTEGER NOT NULL DEFAULT 0,
  metadata TEXT,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  PRIMARY KEY(project_id, id)
);
CREATE INDEX IF NOT EXISTS idx_threads_project_activity ON threads(project_id, last_activity_ms DESC);
CREATE INDEX IF NOT EXISTS idx_threads_project_status ON threads(project_id, status, last_activity_ms DESC);

CREATE TABLE IF NOT EXISTS thread_scores (
  id TEXT PRIMARY KEY,
  project_id TEXT NOT NULL,
  thread_id TEXT NOT NULL,
  job_id TEXT,
  name TEXT NOT NULL,
  score REAL,
  value TEXT,
  rationale TEXT DEFAULT '',
  source TEXT NOT NULL DEFAULT 'api',
  judge_mode TEXT DEFAULT '',
  created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_thread_scores_thread ON thread_scores(project_id, thread_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_thread_scores_job ON thread_scores(job_id);
