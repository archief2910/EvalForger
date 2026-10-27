-- 0004_online_thread_scores.sql (telemetry plane)
-- Automatic scores for whole conversations, produced by online rules whose
-- scope is 'thread'.
--
-- These are kept apart from `thread_scores` because that table holds scores
-- produced by a human evaluation job (and by manual scoring), while these are
-- produced by the live rule loop. The two answer different questions: "what
-- did the evaluation say about this conversation" and "what did the live rule
-- say about it when it closed".

CREATE TABLE IF NOT EXISTS online_thread_scores (
  id TEXT PRIMARY KEY,
  project_id TEXT NOT NULL,
  rule_id TEXT NOT NULL,
  thread_id TEXT NOT NULL,
  scorer TEXT NOT NULL,
  score DOUBLE,
  rationale TEXT NOT NULL DEFAULT '',
  judge_mode TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  UNIQUE(rule_id, thread_id, scorer)
);
CREATE INDEX IF NOT EXISTS idx_online_thread_scores_thread ON online_thread_scores(project_id, thread_id);
CREATE INDEX IF NOT EXISTS idx_online_thread_scores_project ON online_thread_scores(project_id, created_at DESC);
