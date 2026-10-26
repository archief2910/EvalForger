-- 0003_thread_jobs.sql (control plane)
-- Thread evaluations use the same durable queue as experiment evaluations.
-- The target thread lives in the telemetry plane; this column is the routing
-- key the dispatcher needs before it can read that plane.

ALTER TABLE jobs ADD COLUMN thread_id TEXT;
CREATE INDEX IF NOT EXISTS idx_jobs_thread ON jobs(project_id, thread_id, created_at);
