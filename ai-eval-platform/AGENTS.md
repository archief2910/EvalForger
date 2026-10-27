# AGENTS.md

EvalForge — LLM observability + evaluation platform. Go API server, Python eval worker, React frontend.

## Startup Workflow

Before writing code:

1. **Confirm working directory** with `pwd`
2. **Read this file** completely
3. **Read docs**: `docs/ARCHITECTURE.md`, `docs/OPIK_ALIGNMENT.md`, `README.md`
4. **Run `./init.sh`** to verify environment is healthy
5. **Read `feature_list.json`** to see current feature state
6. **Review recent commits** with `git log --oneline -5`

If baseline verification is failing, repair that first before adding new scope.

## Architecture Overview

| Component       | Location                  | Tech                                |
|-----------------|---------------------------|-------------------------------------|
| API Server      | `server-go/cmd/server` + `server-go/internal/` | Go, net/http, pgx (PostgreSQL), go-duckdb |
| Eval Worker     | `worker-py/worker.py`     | Python 3.12, stdlib only            |
| Sandbox         | `server-go/internal/sandbox` | Docker container per custom metric; fails closed when Docker is absent |
| Frontend        | `frontend/`               | React 19 + Vite, hand-written CSS   |
| SDK sample      | `sdk/sample_client.py`    | Proves the ingestion surface        |
| Tests           | `scripts/`                | smoke, auth, fault, load, MCP       |
| Database        | PostgreSQL (`CONTROL_DB_URL`) + `data/telemetry.duckdb` | control metadata in PG, telemetry in DuckDB, embedded migrations per plane |

## Working Rules

- **One feature at a time**: Pick exactly one unfinished feature from `feature_list.json`
- **Verification required**: Don't claim done without running verification commands
- **Update artifacts**: Before ending session, update `progress.md` and `feature_list.json`
- **Stay in scope**: Don't modify files unrelated to the current feature
- **Leave clean state**: Next session must be able to run `./init.sh` immediately
- **Idiomatic Go**: Use interfaces, constructor injection, `context.Context`, typed errors — no globals for new code
- **No ORM**: Use `database/sql` + raw SQL, small repository interfaces
- **Test at the HTTP surface**: `httptest`-based handler tests, black-box Python scripts for cross-process

## Required Artifacts

- `feature_list.json` — Feature state tracker (source of truth)
- `progress.md` — Session continuity log
- `init.sh` — Standard startup and verification path
- `DEPLOY.md` — Docker deployment guide (compose stack, demo script, security notes)
- `session-handoff.md` — Optional, for larger sessions

## Deployment

Three services in `docker-compose.yml` (Postgres, API + eval worker, nginx-served console) and three
one-shot tools behind the `tools` profile (`seed`, `create-key`, `pull-sandbox`).

```bash
docker compose up --build -d
docker compose run --rm seed
open http://localhost:8080
```

Read `DEPLOY.md` before changing anything under `deploy/`. Two constraints are load-bearing:

- The server image installs the **docker CLI** and mounts the host socket so custom metrics run in their
  own container. `SANDBOX_WORK_DIR` and its bind mount must stay the **same absolute path** on both sides,
  because the daemon resolves `-v` paths on the host, not inside the API container.
- `--create-key` opens **only** the control plane (`database.OpenControl`). DuckDB admits one writer per
  file, so a maintenance command that opened telemetry would fail against a running server.

## Definition of Done

A feature is done only when ALL of the following are true:

- [ ] Target behavior is implemented
- [ ] Go tests pass: `cd server-go && go test ./...`
- [ ] Smoke tests pass: `python scripts/smoke_api.py` (69 checks)
- [ ] Auth tests pass: `python scripts/auth_test.py` (44 checks)
- [ ] MCP tests pass: `python scripts/mcp_test.py` (23 checks)
- [ ] Evidence recorded in `feature_list.json` or `progress.md`
- [ ] Repository remains restartable from standard startup path

## End of Session

Before ending a session:

1. Update `progress.md` with current state
2. Update `feature_list.json` with new feature status
3. Record any unresolved risks or blockers
4. Commit with descriptive message once work is in safe state
5. Leave repo clean enough for next session to run `./init.sh` immediately

## Verification Commands

```bash
# Full verification (recommended)
./init.sh
```

Required checks:
- `cd server-go && go build -o evalforge.exe ./cmd/server`
- `cd server-go && go test ./...`
- `python scripts/smoke_api.py`
- `python scripts/auth_test.py`
- `python scripts/mcp_test.py`

Optional (slow):
- `python scripts/fault_test.py` (16 checks, uses taskkill)
- `python scripts/load_test.py` (benchmark + integrity)

## Escalation

If you encounter:
- **Architecture decisions**: Consult `docs/ARCHITECTURE.md` and `docs/OPIK_ALIGNMENT.md`
- **Opik parity questions**: Consult `docs/OPIK_ALIGNMENT.md` §4 (decisions) and §6 (gap roadmap)
- **Unclear requirements**: Check the README or ask user
- **Repeated test failures**: Update progress, flag for human review
- **Scope ambiguity**: Re-read `feature_list.json` for definition of done



