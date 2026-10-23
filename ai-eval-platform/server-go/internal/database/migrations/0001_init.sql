-- 0001_init.sql
-- The schema as it existed before migrations were introduced. It is idempotent
-- so it can also bootstrap an existing database on first upgrade.

CREATE TABLE IF NOT EXISTS projects (
  id TEXT PRIMARY KEY, name TEXT NOT NULL, description TEXT DEFAULT '',
  created_at TEXT NOT NULL
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
  created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_spans_project ON spans(project_id);
CREATE INDEX IF NOT EXISTS idx_spans_trace ON spans(trace_id);

CREATE TABLE IF NOT EXISTS feedback (
  id TEXT PRIMARY KEY, trace_id TEXT, span_id TEXT, kind TEXT NOT NULL,
  value TEXT, comment TEXT, created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS datasets (
  id TEXT PRIMARY KEY, project_id TEXT NOT NULL, name TEXT NOT NULL,
  description TEXT DEFAULT '', version INTEGER DEFAULT 1, created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS dataset_records (
  id TEXT PRIMARY KEY, dataset_id TEXT NOT NULL,
  input TEXT NOT NULL, expected TEXT, metadata TEXT, tags TEXT,
  source TEXT DEFAULT 'manual', created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS scorers (
  id TEXT PRIMARY KEY, project_id TEXT, name TEXT NOT NULL,
  kind TEXT NOT NULL, config TEXT, description TEXT DEFAULT '', created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS experiments (
  id TEXT PRIMARY KEY, project_id TEXT NOT NULL, name TEXT NOT NULL,
  dataset_id TEXT, baseline_experiment_id TEXT,
  status TEXT NOT NULL DEFAULT 'pending', summary TEXT,
  created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS eval_runs (
  id TEXT PRIMARY KEY, experiment_id TEXT NOT NULL, record_id TEXT,
  input TEXT, expected TEXT, output TEXT, error TEXT,
  latency_ms INTEGER, scores TEXT, output_mode TEXT DEFAULT '', created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS prompts (
  id TEXT PRIMARY KEY, project_id TEXT NOT NULL, name TEXT NOT NULL, version INTEGER NOT NULL,
  model TEXT, template TEXT, params TEXT, created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS model_prices (
  model TEXT PRIMARY KEY, input_per_1k REAL, output_per_1k REAL
);

CREATE TABLE IF NOT EXISTS jobs (
  id TEXT PRIMARY KEY, kind TEXT NOT NULL DEFAULT 'eval',
  experiment_id TEXT NOT NULL, project_id TEXT NOT NULL, payload TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'queued',
  attempts INTEGER NOT NULL DEFAULT 0, max_attempts INTEGER NOT NULL DEFAULT 2,
  lease_expires_at TEXT, last_error TEXT,
  created_at TEXT NOT NULL, updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_jobs_claimable ON jobs(status, created_at);

CREATE TABLE IF NOT EXISTS api_keys (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  prefix TEXT NOT NULL,
  key_hash TEXT NOT NULL UNIQUE,
  owner TEXT NOT NULL DEFAULT '',
  scopes TEXT NOT NULL DEFAULT 'read,write',
  project_id TEXT,
  created_at TEXT NOT NULL,
  last_used_at TEXT,
  expires_at TEXT,
  revoked_at TEXT
);

CREATE TABLE IF NOT EXISTS oauth_clients (
  client_id TEXT PRIMARY KEY,
  client_secret_hash TEXT,
  client_name TEXT NOT NULL DEFAULT '',
  redirect_uris TEXT NOT NULL,
  grant_types TEXT NOT NULL DEFAULT 'authorization_code',
  scope TEXT,
  token_endpoint_auth_method TEXT NOT NULL DEFAULT 'none',
  created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS oauth_codes (
  code_hash TEXT PRIMARY KEY,
  client_id TEXT NOT NULL,
  key_id TEXT NOT NULL,
  owner TEXT NOT NULL DEFAULT '',
  project_id TEXT,
  redirect_uri TEXT NOT NULL,
  scope TEXT NOT NULL DEFAULT '',
  code_challenge TEXT NOT NULL,
  code_challenge_method TEXT NOT NULL DEFAULT 'S256',
  resource TEXT,
  created_at TEXT NOT NULL,
  expires_at TEXT NOT NULL,
  used_at TEXT
);

CREATE TABLE IF NOT EXISTS oauth_tokens (
  token_hash TEXT PRIMARY KEY,
  kind TEXT NOT NULL,
  client_id TEXT NOT NULL,
  key_id TEXT NOT NULL,
  owner TEXT NOT NULL DEFAULT '',
  project_id TEXT,
  scope TEXT NOT NULL DEFAULT '',
  resource TEXT,
  created_at TEXT NOT NULL,
  expires_at TEXT NOT NULL,
  revoked_at TEXT,
  rotated_from TEXT
);
CREATE INDEX IF NOT EXISTS idx_oauth_tokens_key ON oauth_tokens(key_id);
