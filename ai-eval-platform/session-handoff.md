# Session Handoff

## Current Objective

- Goal: finish the EvalForge restructure into idiomatic Go and close the highest-value product
  gaps (dataset snapshots, prompt pinning, token provenance) without regressing the platform.
- Current status: restructure and the product gaps are implemented and verified. feat-002,
  feat-005, feat-008 and the full feat-009 coverage target remain.
- Branch / commit: parent repo `system_crafter`, commit to be created for `ai-eval-platform/`
  only. Do **not** stage the unrelated `horizontal-scaling-with-read-replicas/` deletions.

## Completed This Session

- [x] Split the 1,721-line `server-go/main.go` into `cmd/server` +
      `internal/{config,logging,database,model,queue,worker,api}`.
- [x] Embedded, transactional, forward-only SQL migrations with `schema_migrations`.
- [x] Immutable dataset snapshots (`dataset_versions`, SHA-256 hash, delta counts) and
      experiment pinning; worker reads the pinned snapshot.
- [x] Prompt version pinning by name/version or direct id; worker fetches the pinned template.
- [x] Token provenance (`provider` / `estimated` / empty) through native and OTLP ingest.
- [x] Boot-scoped internal worker token so evaluations work with `AUTH_MODE=required`.
- [x] Project-scoped credentials enforced on by-id routes (404 cross-project).
- [x] UI: snapshot hash, prompt selector, pinned version display, rerun pinned.
- [x] `--create-key` keeps stdout clean; docs/harness updated for `go build ./cmd/server`.

## Verification Evidence

| Check | Command | Result | Notes |
|---|---|---|---|
| Build | `cd server-go && go build -o evalforge.exe ./cmd/server` | pass | |
| Vet | `cd server-go && go vet ./...` | pass | |
| Go tests | `cd server-go && go test ./... -count=1` | pass | 3 consecutive runs |
| Smoke | `python scripts/smoke_api.py` | 31/31 | |
| Auth | `python scripts/auth_test.py` | 44/44 | |
| MCP | `python scripts/mcp_test.py` | 23/23 | |
| Fault | `python scripts/fault_test.py` | 16/16 | crash/restart, timeout, retry |
| Load | `python scripts/load_test.py` | pass | 360 traces, 0 errors, ~1,000 spans/s |
| Frontend | `cd frontend && npm run build` | pass | |
| Auth-required eval | manual run | pass | completed, 2 runs, snapshot pinned |

## Files Changed

- New: `server-go/cmd/`, `server-go/internal/`, `internal/database/migrations/`
- Modified: `worker-py/worker.py`, three frontend pages, five Python test scripts,
  `README.md`, `docs/ARCHITECTURE.md`, `AGENTS.md`, `init.sh`,
  `feature_list.json`, `progress.md`
- Deleted: the old root `server-go/*.go` files (copied to `%TEMP%\evalforge-old\`)

## Decisions Made

- MCP stays in `internal/api` (shared auth/routing, no cycle).
- Dataset snapshots are full blobs; deltas are metadata only.
- Prompt references resolve once at experiment creation; reruns pin the resolved id.
- Zero usage on an LLM span means "not reported" → estimate and label `estimated`.
- Internal worker token is boot-scoped and only passed through the child environment.
- Cross-project by-id access returns 404, never 403.

## Blockers / Risks

- Unrelated tracked deletions under `horizontal-scaling-with-read-replicas/` are still in the
  working tree. Do not commit them; restore from git only with user confirmation.
- feat-002 (typed errors), feat-005 (repository interfaces), feat-008 (DB-backed scorer
  definitions) are real architectural gaps, not cosmetic.
- SQLite remains a single-writer deployment assumption.
- feat-009 route coverage has not been measured against the 80% target.

## Next Session Startup

1. Read `AGENTS.md`.
2. Read `feature_list.json` and `progress.md`.
3. Review this handoff.
4. Run `./init.sh` or the documented verification commands before editing.

## Recommended Next Step

- Implement feat-002: introduce a typed API error, render it in one middleware, log the cause
  with the request id, and remove raw driver errors from handler responses. Then re-run
  `./init.sh`.
