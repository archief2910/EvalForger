-- 0003_online_scores.sql (telemetry plane)
-- Scores produced by online rules. Kept separate from `feedback` (human
-- thumbs/comments) so automatic and human judgement never overwrite each
-- other, while the trace view can still show both.

CREATE TABLE IF NOT EXISTS online_scores (
  id TEXT PRIMARY KEY,
  project_id TEXT NOT NULL,
  rule_id TEXT NOT NULL,
  trace_id TEXT NOT NULL,
  scorer TEXT NOT NULL,
  score DOUBLE,
  rationale TEXT NOT NULL DEFAULT '',
  judge_mode TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  UNIQUE(rule_id, trace_id, scorer)
);
CREATE INDEX IF NOT EXISTS idx_online_scores_project ON online_scores(project_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_online_scores_trace ON online_scores(trace_id);
