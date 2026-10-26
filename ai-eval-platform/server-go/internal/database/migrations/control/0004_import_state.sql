-- 0004_import_state.sql (control plane)
-- One row per pre-PostgreSQL SQLite file that was transported into the new
-- engines. It makes the import idempotent and leaves an audit trail of how many
-- rows each table contributed.

CREATE TABLE IF NOT EXISTS import_state (
  source TEXT PRIMARY KEY,
  imported_at TEXT NOT NULL,
  rows_json TEXT NOT NULL DEFAULT '{}'
);
