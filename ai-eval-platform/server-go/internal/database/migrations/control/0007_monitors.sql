-- 0007_monitors.sql (control plane)
-- A monitor is an aggregate condition over a window of production traffic:
-- "error_rate > 5% for the last 15 minutes". Unlike a per-trace online rule,
-- a monitor looks at a metric computed over time and fires once when the
-- condition becomes true, then resolves when it becomes false again.
--
-- Monitors live on the control plane: they are low-volume, human-edited
-- configuration with a state machine. The metric they watch is telemetry, and
-- the two are never joined in SQL.

CREATE TABLE IF NOT EXISTS monitors (
  id TEXT PRIMARY KEY,
  project_id TEXT NOT NULL,
  name TEXT NOT NULL,
  enabled INTEGER NOT NULL DEFAULT 1,
  metric TEXT NOT NULL,
  operator TEXT NOT NULL DEFAULT '>',
  threshold DOUBLE PRECISION NOT NULL,
  window_seconds INTEGER NOT NULL DEFAULT 900,
  channel_type TEXT NOT NULL DEFAULT 'none',
  channel_url TEXT NOT NULL DEFAULT '',
  channel_secret TEXT NOT NULL DEFAULT '',
  state TEXT NOT NULL DEFAULT 'ok',
  last_value DOUBLE PRECISION,
  last_fired_at TEXT,
  last_evaluated_at TEXT,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_monitors_project ON monitors(project_id, enabled);

-- One row per firing episode. A monitor that stays in breach produces one
-- event, not one per evaluation tick; the event is resolved when the metric
-- recovers.
CREATE TABLE IF NOT EXISTS monitor_events (
  id TEXT PRIMARY KEY,
  monitor_id TEXT NOT NULL,
  project_id TEXT NOT NULL,
  metric TEXT NOT NULL,
  operator TEXT NOT NULL,
  threshold DOUBLE PRECISION NOT NULL,
  value DOUBLE PRECISION,
  state TEXT NOT NULL DEFAULT 'firing',
  fired_at TEXT NOT NULL,
  resolved_at TEXT,
  notified INTEGER NOT NULL DEFAULT 0,
  notification_error TEXT
);
CREATE INDEX IF NOT EXISTS idx_monitor_events_project ON monitor_events(project_id, fired_at DESC);
CREATE INDEX IF NOT EXISTS idx_monitor_events_monitor ON monitor_events(monitor_id, fired_at DESC);
CREATE INDEX IF NOT EXISTS idx_monitor_events_open ON monitor_events(monitor_id, state) WHERE state='firing';
