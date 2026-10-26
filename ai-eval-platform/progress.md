# Session Progress Log

## Current State

**Last Updated:** 2026-10-26
**Active Feature:** none — feat-001/003/004/006/007/010/011 complete; feat-002/005/008/009 remain
**Session Goal:** Add first-class conversations (threads) with thread-level scoring and a
control-plane/data-plane storage split, while preserving every existing EvalForge behavior and
test suite.

## Status

### What's Done

#### Restructure (feat-001)

- [x] `server-go/main.go` (1,721 lines) split into packages:
  - `cmd/server/main.go` — flags, config, migrate, HTTP server, graceful shutdown, `--create-key`
  - `internal/config` — typed `Config`, `.env` loading, `Validate()`, `AuthRequired()`, `Issuer()`
  - `internal/logging` — `slog` construction
  - `internal/database` — SQLite pool, embedded migrations, price seeding, legacy import
  - `internal/model` — domain types, pricing, token estimation, JSON helpers
  - `internal/queue` — durable claim/lease/heartbeat/retry/reaper queue
  - `internal/worker` — bounded Python subprocess client
  - `internal/api` — `Server`, router, handlers, auth/OAuth, OTLP, MCP, versioning, middleware
- [x] Handlers are methods on `api.Server`; dependencies are constructor-injected. No
      package-level `db`, `dbMu`, `logger`, or `selfBaseURL` in new code.
- [x] Routing table is explicit in `internal/api/router.go`; all by-id resource routes enforce
      project scope via `allowProject`, returning 404 cross-project.
- [x] Old root `.go` files removed. A copy was left in
      `%TEMP%\evalforge-old\` before deletion (local safety net only, not in the repo).
- [x] `go build ./...`, `go vet ./...`, `go test ./...` green.

#### Product gaps closed

- [x] **feat-004 — Embedded migrations.** `internal/database/migrations/0001_init.sql` and
      `0002_product.sql`, sorted via `embed.FS`, tracked in `schema_migrations`; each migration
      runs in its own transaction. Legacy DB import still works.
- [x] **feat-006 — Immutable dataset snapshots.** `dataset_versions` stores the full
      `items_json` blob, a SHA-256 `version_hash`, and added/modified/deleted counts.
      Create/add/promote snapshot inside the same transaction as the row write. Experiments
      pin `dataset_version_id`; the worker reads `/api/v1/dataset_versions/{id}`.
- [x] **feat-007 — Prompt pinning.** `POST /projects/{id}/evaluations` accepts
      `prompt_name` + `prompt_version` (0 = latest) or a direct `prompt_version_id` (reruns).
      The resolved id is stored on the experiment and fetched by the worker. Cross-project
      prompt ids are rejected with 404.
- [x] **Token provenance.** LLM spans record `token_source`:
      `provider` when the caller reported usage, `estimated` when the server estimated it,
      empty for non-LLM spans. Exposed in trace detail and labelled `(est.)` in the UI.
- [x] **Internal worker auth.** The server generates an `X-Internal-Token` per boot and passes
      it only through the worker child environment. Evaluations now complete with
      `AUTH_MODE=required` without provisioning a long-lived key.
- [x] **UI.** Dataset detail shows snapshot hash + prompt selector; experiment detail shows the
      pinned dataset version / prompt and offers **Rerun pinned**.
- [x] **Bootstrap.** `--create-key` keeps stdout clean (structured logs are suppressed), so a
      shell capture only receives the key.

#### Conversations & control/data plane split (feat-011)

- [x] **Two planes** (originally two SQLite files; superseded by the PostgreSQL + DuckDB migration
      below). The control plane owns projects, datasets/versions/records, prompts, experiments,
      scorers, model prices, jobs, API keys/OAuth and feedback definitions; the telemetry plane owns
      spans, feedback, eval_runs, threads and thread_scores. Handlers read one plane and merge in Go;
      there are no cross-plane write transactions.
- [x] **Upgrade path.** Historical SQLite data is transported once into the new engines and every
      historical trace is backfilled into a one-turn conversation. The SQLite files are left intact.

- [x] **Thread identity and lifecycle.** Native ingest and OTLP accept an explicit `thread_id`;
      absent it, the trace id becomes a one-turn thread, so every trace belongs to a conversation.
      Threads are `open`/`closed`, rebuilt after each ingest, closed by an idle sweeper
      (`THREAD_IDLE_SECONDS`, default 1800 s), and reopened by new activity. Manual close/open
      endpoints exist.
- [x] **Thread scoring.** `thread_scores` is separate from per-trace feedback. Manual scores and
      worker scores coexist; the detail API returns a per-scorer average, recent scores and
      evaluation history.
- [x] **Thread evaluators.** Deterministic `thread_completion`, `thread_error_free`,
      `thread_turn_consistency`, plus LLM `thread_coherence_judge` and
      `thread_context_retention_judge`. Evaluations run through the durable queue as
      `kind='thread_eval'`; the worker callback is idempotent per job. LLM judges fall back to
      deterministic heuristics (labelled `heuristic_fallback`) when the provider is missing **or**
      returns an error.
- [x] **UI.** New Conversations list and conversation detail (turn timeline, scores, manual
      scoring, evaluation history, close/reopen/evaluate). Dashboard shows conversation counts,
      turn/duration averages and score aggregates. Traces list has a conversation column and
      `?thread_id=` filter; trace detail links back to its conversation.
- [x] **Demo seed.** Upgrade-aware: if the demo project exists without multi-turn conversations,
      it adds 8 three-turn conversations and sample conversation scores instead of exiting or
      duplicating prompts/datasets. One conversation runs a real worker evaluation.

#### Storage engines: PostgreSQL + DuckDB (feat-012)

- [x] **Control plane on PostgreSQL.** `CONTROL_DB_URL` + `CONTROL_DB_SCHEMA`, pgx driver, statements
      keep `?` and `internal/pgbind` rewrites them to `$1..$n` at the driver boundary (including inside
      transactions). Migrations are applied statement-by-statement in one transaction; a private
      schema per test/harness run is created and dropped automatically.
- [x] **Telemetry on DuckDB.** `TELEMETRY_DB_PATH` (default `data/telemetry.duckdb`), one connector and
      a bounded pool, columnar scans for dashboards, BIGINT millisecond columns (SQLite's INTEGER was
      dynamically typed and hid the overflow).
- [x] **One-time transport.** `internal/database/import.go` reads `ai_eval.db`, `telemetry.db` and
      `control.db` (or `LEGACY_DB`) read-only, routes the whitelisted tables per plane, inserts with
      `ON CONFLICT DO NOTHING`, and records each file in `import_state`. Verified against the demo data:
      245 spans, 28 feedback, 25 eval runs, 56 conversations, 28 scores, plus all control rows.
- [x] **Harness isolation.** Since PostgreSQL is not inside `DATA_DIR`, each Go test and each Python
      harness pins its own schema; `scripts/_pg.py` creates/drops it, and `fault_test.py` inspects job
      rows through psql while the server is deliberately down.
- [x] **Verified:** `go test ./...`, smoke **48/48**, auth **44/44**, MCP **23/23**, fault **16/16**,
      load **PASS** (360/360 traces, 0 errors), each against PostgreSQL + DuckDB.
#### Online evaluation: rules over live traffic (feat-013)

- [x] **Rules.** `online_rules` (PostgreSQL) holds a project-scoped rule: name, enabled, sample rate,
      root-status/model/name filters, scorer names and an alert threshold. Validation accepts only
      `scope=record` scorers from the registry, so there is still no path from the API to arbitrary code.
- [x] **Automatic scoring on ingest.** Native and OTLP ingest project the committed batch into
      `{trace, root name, status, models}`; matching costs no extra query. Scoring runs off the request
      path in a bounded pool (`ONLINE_CONCURRENCY`, `ONLINE_TIMEOUT_SECONDS`).
- [x] **Deterministic sampling + idempotency.** `hash(rule_id|trace_id) < sample_rate`; a trace already
      scored for a rule is skipped before the worker starts; `online_scores` is unique on
      `(rule_id, trace_id, scorer)`, and alerts are written only for an inserted score.
- [x] **Worker mode.** `worker.py --online --trace-id ... --scorers ...` reads the stored trace, resolves
      scorer kinds, and prints one JSON document; progress stays on stderr, so the server parses without
      a new callback endpoint.
- [x] **Alerts.** Score below threshold → `online_alerts` (PostgreSQL) with `open/acknowledged` state.
      `POST /alerts/{id}/ack` is idempotent; acknowledged alerts remain as an audit trail.
- [x] **Surface.** CRUD rules, manual "score this trace" run, alerts list/ack, `online_scores` on the
      trace view, dashboard block, and an **Online evaluation** page (rules table, create form,
      pause/resume/delete, open alerts).
- [x] **Verified:** Go tests cover matching/sampling, ingest-triggered scoring, alerts, ack, filters,
      sampling=0, disabled rules and PATCH merge; smoke **48/48** against the real worker.

#### Documentation / harness

- [x] `AGENTS.md`, `README.md`, `docs/ARCHITECTURE.md` updated for the package layout and
      `go build ./cmd/server`.
- [x] `init.sh` and all Python suites build `./cmd/server` and boot their own isolated server.
- [x] `feature_list.json` reflects verified status.

### What's In Progress

- [ ] feat-009 (Go httptest suite): broad coverage exists for auth/OAuth, MCP, OTLP, dataset
      snapshots, prompt pinning, token provenance, conversations and scoped access, but the 80%
      route-coverage target has not been measured.

### What's Next

1. Commit only `ai-eval-platform/` (explicit pathspec), leaving the unrelated
   `horizontal-scaling-with-read-replicas/` deletions untouched.
2. Optional product work, in priority order:
   - **feat-002** typed API errors (`api.Error{Status, Code, Message, Err}`) + one rendering
     middleware; stop returning raw driver errors from handlers.
   - **feat-008** feedback/scorer definitions in the database with seeded builtins and a
     `POST /feedback-definitions` surface.
   - **feat-005** repository interfaces (`TelemetryStore`, `MetadataStore`) so the SQLite
     implementation can be swapped for ClickHouse later.
   - **feat-009** route-coverage measurement and missing handler tests.

## Verification Evidence

Run on 2026-10-26 after the conversations + dual-plane changes:

| Check | Command | Result |
|---|---|---|
| Build | `cd server-go && go build ./...` | pass |
| Go tests | `cd server-go && go test ./... -count=1 -timeout 180s` | pass (api 5.3s), including `threads_test.go` |
| Smoke | `python scripts/smoke_api.py` | 41/41 |
| Auth | `python scripts/auth_test.py` | 44/44 |
| MCP | `python scripts/mcp_test.py` | 23/23 |
| Fault | `python scripts/fault_test.py` | 16/16 (crash/restart, timeout, retry) |
| Load | `python scripts/load_test.py` | 360/360 traces, 0 errors, 46 spans/s (1080 spans) |
| Frontend | `cd frontend && npm run build` | pass |
| Demo migration | boot against `data/ai_eval.db` | control/telemetry split; 119 spans, 16 feedback, 25 eval_runs, 36 backfilled threads |
| Browser | Conversations list/detail, trace filter, trace context bar, evaluate | pass; worker job completed and scores rendered |

## Decisions Made

- **Final package layout** is `cmd/server` + `internal/{config,logging,database,model,worker,queue,api}`.
  MCP stays inside `internal/api` because it shares auth and routing helpers; splitting it
  would add a dependency cycle for no runtime benefit.
- **Snapshots are full blobs, not deltas.** Dataset sizes here are small, and a full immutable
  snapshot makes a pinned experiment trivially reproducible. Delta counts are metadata only.
- **Prompt pinning resolves at experiment creation.** The worker never resolves names or
  "latest", so a later prompt edit cannot change a running or rerun experiment.
- **Token estimates are labelled, never silent.** Zero usage on an LLM span is treated as
  "not reported", estimated, and marked `estimated`; explicit usage is `provider`.
- **Internal token instead of a provisioned key.** It is boot-scoped, never persisted, and only
  exists in the worker child environment.
- **404, not 403, for cross-project by-id access.** A scoped credential cannot confirm that
  another project's resource exists.
- **Control and telemetry are separate SQLite files, merged in Go.** The split is physical, not
  just logical: each file can be backed up, vacuumed, retained or moved independently. Joins across
  planes are forbidden, which keeps a future ClickHouse/Postgres telemetry migration honest.
- **A trace is one turn; `thread_id` groups turns.** Explicit ids come from ingest/OTLP; the
  trace-id fallback guarantees every trace is addressable as a conversation. The server does not
  guess a conversation from user/session metadata.
- **Thread evaluations use the same durable queue as experiments.** `kind='thread_eval'` jobs
  survive restarts and the callback replaces a job's scores on retry, so redelivery is idempotent.

## Blockers / Risks

- **Unrelated working-tree deletions.** `horizontal-scaling-with-read-replicas/` is gone from
  disk and shows as deleted in the parent repo. This session did not delete it. Do not stage
  those paths; restore from git only if the user confirms.
- **feat-002/005/008 are real gaps**, not polish: handlers still surface some raw SQL errors,
  scorers are still Go literals, and handlers still issue SQL directly despite the physical plane
  split. The product works, but these are the next architectural steps.
- **SQLite single-writer assumption** remains per file. The queue is crash-safe but not
  multi-instance coordinated; a second server would need lease-clock tuning and a shared writer.
- **Windows dev environment.** `init.sh` is bash; PowerShell users can run the same commands
  individually (documented in README/AGENTS).

## Files Modified This Session

### New Go layout

- `server-go/cmd/server/main.go`
- `server-go/internal/config/config.go`
- `server-go/internal/logging/logging.go`
- `server-go/internal/database/{db,migrate,legacy}.go`
- `server-go/internal/database/migrations/control/{0001_init,0002_product,0003_thread_jobs}.sql`
- `server-go/internal/database/migrations/telemetry/{0001_init,0002_threads}.sql`
- `server-go/internal/model/*.go`
- `server-go/internal/queue/queue.go`
- `server-go/internal/worker/client.go`
- `server-go/internal/api/{server,router,handlers,auth,middleware,otlp,mcp,mcp_http,versioning,threads}.go`
- `server-go/internal/api/{helpers,auth,otlp,versioning,threads}_test.go`
- `frontend/src/pages/{Conversations,ThreadDetail}.jsx`

### Modified

- `server-go/cmd/server/main.go` — open both planes, pass them to `api.New`
- `server-go/internal/config/config.go` — control/telemetry paths + thread idle timeout
- `server-go/internal/api/handlers.go` — telemetry routing, ingest thread resolution, dashboard metrics
- `worker-py/worker.py` — thread evaluators, LLM judges, provider-error heuristic fallback
- `worker-py/seed.py` — upgrade-aware multi-turn conversations and conversation scores
- `frontend/src/{main.jsx,components/Layout.jsx}`, `frontend/src/pages/{Dashboard,Traces,TraceDetail}.jsx`
- `scripts/{smoke_api,fault_test}.py` — thread checks and control.db queue reads
- `README.md`, `docs/ARCHITECTURE.md`, `AGENTS.md`, `init.sh`
- `feature_list.json`, `progress.md`

### Deleted

- `server-go/{main,auth,auth_test,jobs,mcp,mcp_http,middleware,otlp,otlp_test}.go`

## Notes for Next Session

- Start with `./init.sh` (or the equivalent commands) before changing anything.
- The fastest honest demo path: seed via `worker-py/seed.py`, open the dashboard, open a
  conversation and run its evaluation, promote a trace to a dataset, run an experiment with a
  pinned prompt, compare against a baseline, then rerun pinned.
- If continuing the architecture work, do feat-002 first: it makes the handler error surface
  consistent and is a prerequisite for a clean repository layer.
