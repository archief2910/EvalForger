# Session Progress Log

## Current State

**Last Updated:** 2026-10-23 21:40
**Active Feature:** none — feat-001/003/004/006/007/010 complete; feat-002/005/008/009 remain
**Session Goal:** Finish the monolith → idiomatic Go restructure, then close the highest-value
product gaps (dataset snapshots, prompt pinning, token provenance) without regressing the
existing platform.

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

#### Documentation / harness

- [x] `AGENTS.md`, `README.md`, `docs/ARCHITECTURE.md` updated for the package layout and
      `go build ./cmd/server`.
- [x] `init.sh` and all Python suites build `./cmd/server` and boot their own isolated server.
- [x] `feature_list.json` reflects verified status.

### What's In Progress

- [ ] feat-009 (Go httptest suite): broad coverage exists for auth/OAuth, MCP, OTLP, dataset
      snapshots, prompt pinning, token provenance and scoped access, but the 80% route-coverage
      target has not been measured.
- [ ] Working tree is not committed yet. Unrelated tracked deletions under
      `../horizontal-scaling-with-read-replicas/` were present before this work and must not be
      committed.

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

Run on 2026-10-23 after the final code changes:

| Check | Command | Result |
|---|---|---|
| Build | `cd server-go && go build -o evalforge.exe ./cmd/server` | pass |
| Vet | `cd server-go && go vet ./...` | pass |
| Go tests | `cd server-go && go test ./... -count=1` | pass (3 consecutive runs) |
| Smoke | `python scripts/smoke_api.py` | 31/31 |
| Auth | `python scripts/auth_test.py` | 44/44 |
| MCP | `python scripts/mcp_test.py` | 23/23 |
| Fault | `python scripts/fault_test.py` | 16/16 (crash/restart, timeout, retry) |
| Load | `python scripts/load_test.py` | 360 traces, 0 errors, ~1,000 spans/s |
| Frontend | `cd frontend && npm run build` | pass |
| Auth-required eval | manual run with `AUTH_MODE=required` | completed, 2 runs, snapshot pinned |

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

## Blockers / Risks

- **Unrelated working-tree deletions.** `horizontal-scaling-with-read-replicas/` is gone from
  disk and shows as deleted in the parent repo. This session did not delete it. Do not stage
  those paths; restore from git only if the user confirms.
- **Untracked harness files** (`AGENTS.md`, `feature_list.json`, `progress.md`, `init.sh`,
  `session-handoff.md`) and `server-go/{cmd,internal}/` still need to be added on commit.
- **feat-002/005/008 are real gaps**, not polish: handlers still surface some raw SQL errors,
  and scorers are still Go literals. The product works, but these are the next architectural
  steps.
- **SQLite single-writer assumption** remains. The queue is crash-safe but not multi-instance
  coordinated; a second server would need lease-clock tuning and a shared writer.
- **Windows dev environment.** `init.sh` is bash; PowerShell users can run the same commands
  individually (documented in README/AGENTS).

## Files Modified This Session

### New Go layout

- `server-go/cmd/server/main.go`
- `server-go/internal/config/config.go`
- `server-go/internal/logging/logging.go`
- `server-go/internal/database/db.go`, `migrations/0001_init.sql`, `migrations/0002_product.sql`
- `server-go/internal/model/*.go`
- `server-go/internal/queue/queue.go`
- `server-go/internal/worker/client.go`
- `server-go/internal/api/{server,router,handlers,auth,middleware,otlp,mcp,mcp_http,versioning}.go`
- `server-go/internal/api/{helpers,auth,otlp,versioning}_test.go`

### Modified

- `worker-py/worker.py` — internal token, dataset-version fetch, prompt rendering
- `frontend/src/pages/{DatasetDetail,ExperimentDetail,TraceDetail}.jsx`
- `scripts/{smoke_api,auth_test,mcp_test,fault_test,load_test}.py` — build path
- `README.md`, `docs/ARCHITECTURE.md`, `AGENTS.md`, `init.sh`
- `feature_list.json`, `progress.md`

### Deleted

- `server-go/{main,auth,auth_test,jobs,mcp,mcp_http,middleware,otlp,otlp_test}.go`

## Notes for Next Session

- Start with `./init.sh` (or the equivalent commands) before changing anything.
- The fastest honest demo path: seed via `worker-py/seed.py`, open the dashboard, promote a
  trace to a dataset, run an evaluation with a pinned prompt, compare against a baseline, then
  rerun pinned.
- If continuing the architecture work, do feat-002 first: it makes the handler error surface
  consistent and is a prerequisite for a clean repository layer.
