# EvalForge

An original, production-lean implementation of the LLM observability + evaluation loop:
**instrument → observe traces → review → promote to dataset → run evaluations → compare vs baseline.**




## Stack
- **API server**: Go (`server-go/`, module `evalforge`) — one binary, owns the PostgreSQL control plane and the
  DuckDB telemetry store and the whole `/api/v1` surface (ingest, traces, conversations, dashboard, datasets,
  prompts, experiments) plus the MCP and OAuth endpoints.
- **Eval worker**: Python (`worker-py/worker.py`) — the Go server dispatches an eval job to it; the worker
  generates outputs, applies the scorers, and posts results back over the worker-callback API.
- **Frontend**: React 19 + Vite, original dark UI (`frontend/`), proxies `/api` → `localhost:8000`.
- **SDK sample**: `sdk/sample_client.py` — proves the ingestion surface (batching, nested spans, flush).

### Go layout
```
server-go/
  cmd/server/main.go        # flags, config, migrate, serve, graceful shutdown
  internal/config/          # typed env config + validation
  internal/database/        # PG + DuckDB planes, migrations, SQLite import
  internal/pgbind/          # ? placeholder → $n for PostgreSQL, at the driver
  internal/model/           # domain types, pricing, token estimation
  internal/queue/           # durable claim/lease/heartbeat/retry queue
  internal/worker/          # Python subprocess boundary
  internal/api/             # routing, handlers, auth, OTLP, MCP, versioning
```
Handlers are methods on `api.Server`; dependencies are constructor-injected. The binary is built with
`go build ./cmd/server`, not from the repository root.

### Control plane and data plane

Storage is deliberately split, the same shape Opik uses for MySQL + ClickHouse: a transactional
database for metadata and a columnar file for telemetry.

| Engine | Owns | Why |
|---|---|---|
| **PostgreSQL** (`CONTROL_DB_URL`, schema `CONTROL_DB_SCHEMA`) | projects, datasets, dataset versions/records, prompts, experiments, scorers, model prices, jobs, API keys/OAuth, feedback definitions, **online rules and alerts** | small, relational, transactional; online backup and concurrent writers without a process-wide mutex |
| **DuckDB** (`TELEMETRY_DB_PATH`, `data/telemetry.duckdb`) | spans, feedback, eval runs, conversations (`threads`), conversation scores, **automatic online scores** | append-heavy, columnar scans for dashboards, backed up as one file, retained independently |

Handlers never join across the planes. They query one engine, then merge in Go, so telemetry can move
to ClickHouse later without rewriting the product surface. Statements are written once with `?`
placeholders: DuckDB uses them natively and `internal/pgbind` rewrites them to `$1..$n` at the
PostgreSQL driver boundary, including inside transactions.

The move off SQLite is automatic and one-time. On first boot the server imports any existing
`data/ai_eval.db`, `data/telemetry.db` and `data/control.db` it finds (or `LEGACY_DB`): control tables
land in PostgreSQL, telemetry tables in DuckDB, each row with `ON CONFLICT DO NOTHING`, each file
recorded in `import_state`. The SQLite files are opened read-only and left on disk, so nothing is lost
and the import can be retried safely. Every historical trace is then backfilled into a one-turn
conversation.

## Run it

### One command, with Docker

```bash
cp .env.example .env          # optional: add OPENROUTER_API_KEY
docker compose up --build -d  # postgres + API/eval worker + web console
docker compose run --rm seed  # demo data
open http://localhost:8080
```

That is the whole deployment: Postgres for metadata, DuckDB telemetry on a volume, the API with its
Python worker, and the console behind nginx. Full instructions, the demo script, security notes and
troubleshooting are in **[DEPLOY.md](DEPLOY.md)**. The same file has a Render section: `render.yaml`
provisions a managed Postgres, the API with a telemetry disk, and the console.

### From source, without Docker

Telemetry is DuckDB, and its Go driver uses cgo, so the build needs a C compiler
as well as Go 1.27+, Node 20+, Python 3.12+ and PostgreSQL 16:

- **Windows:** `winget install --id BrechtSanders.WinLibs.POSIX.UCRT --exact`
- **macOS:** `xcode-select --install`
- **Debian/Ubuntu:** `sudo apt-get install build-essential`

On Windows, one script starts the whole local stack:

```powershell
# from ai-eval-platform
powershell -ExecutionPolicy Bypass -File .\scripts\start-local.ps1
```

It starts the project-local PostgreSQL cluster on `:55432`, builds the API with
cgo, seeds the demo project, and starts Vite. Open **http://127.0.0.1:5173**.
Stop it with `.\scripts\stop-local.ps1`; data stays in `data\`.

The manual equivalent, if you prefer separate terminals:

```powershell
# 1) PostgreSQL 16 on :55432 (skip initdb if data\pgdata already exists)
& "C:\Program Files\PostgreSQL\16\bin\initdb.exe" -D data\pgdata -U evalforge --auth=trust --encoding=UTF8 --no-locale
Start-Process -FilePath "C:\Program Files\PostgreSQL\16\bin\pg_ctl.exe" `
  -ArgumentList @('-D', "$PWD\data\pgdata", '-o', '"-p 55432 -h 127.0.0.1"', '-l', "$PWD\data\pg.log", 'start') `
  -WindowStyle Hidden -Wait
& "C:\Program Files\PostgreSQL\16\bin\createdb.exe" -h 127.0.0.1 -p 55432 -U evalforge evalforge
& "C:\Program Files\PostgreSQL\16\bin\createdb.exe" -h 127.0.0.1 -p 55432 -U evalforge evalforge_test

# 2) API server (cgo is required)
$env:CGO_ENABLED = "1"      # and CC=<path to gcc> if gcc is not on PATH
cd server-go
go build -o evalforge.exe ./cmd/server
$env:AUTH_MODE = "off"
.\evalforge.exe                                  # → http://localhost:8000

# 3) demo data (second terminal, from ai-eval-platform)
python worker-py/seed.py

# 4) console (third terminal, from ai-eval-platform)
cd frontend
npm install      # already done if node_modules exists
npm run dev      # → http://127.0.0.1:5173
```

The default `CONTROL_DB_URL` is
`postgres://evalforge@127.0.0.1:55432/evalforge?sslmode=disable`; point it at any
PostgreSQL 16 instance to use a different control plane. Telemetry lives in
`data/telemetry.duckdb` unless `TELEMETRY_DB_PATH` says otherwise.

Optional real LLM provider (higher-quality judges/tasks; everything works offline without it).
Put these in `ai-eval-platform/.env` (git-ignored, never committed):
```bash
OPENROUTER_API_KEY=sk-or-...
OPENROUTER_BASE_URL=https://openrouter.ai/api/v1
```
The Go server loads `.env` on start and passes the environment to the worker. The model defaults to
`openrouter/free`; set `OPENROUTER_MODEL` (e.g. `openai/gpt-4o-mini`) to pin a real one. Either way the
provider must be an OpenRouter-compatible endpoint, because that is what the worker speaks.

The free router picks reasoning models, which can spend the whole token budget before writing an
answer. The worker handles that by escalating `max_tokens` and retrying, and records
`judge_mode: provider_error` (never a fake `0.0`) when no answer arrives.

## The core workflow (5 minutes)
1. **Logs & Traces** — filter by status/model/has-feedback/conversation, open a trace: nested span tree + timeline, per-span tokens/cost.
2. **Conversations** — every trace belongs to a conversation (`thread_id`, falling back to the trace id). Open one to see its turns, close/reopen it, add a conversation-level score, or run the thread evaluators (completion, error-free, turn consistency, coherence/context-retention judges). Idle conversations close automatically after `THREAD_IDLE_SECONDS`.
3. **Review** — thumbs + comment on a trace, edit the *expected* answer.
4. **Add to dataset** — one click promotes the trace (`input` copied, `expected` = your corrected answer, `metadata.origin_trace_id` = reference back to the trace).
5. **Run evaluation** — pick a dataset, select scorers (deterministic + LLM-as-judge), optionally pin a prompt version, choose a baseline experiment → an immutable experiment is created. The dataset is snapshotted first, so later edits cannot change what the run saw.
6. **Compare** — per-case deltas by input alignment; improvement/regression badges per scorer. **Rerun pinned** reuses the same dataset snapshot and prompt version.
7. **Dashboard** — requests, error rate, p50/p95/p99 latency, token/cost totals, model breakdown, conversation counts, conversation-score averages and the online-evaluation block. Token counts reported by the provider are labelled `provider`; estimates are labelled `estimated` in the trace view.
8. **Online evaluation** — the continuous half of the loop. A rule (`Online evaluation` in the sidebar) matches live traffic on root status/name/model, samples a fraction of matches, runs the selected trace scorers on ingest, and writes an automatic score to the trace. When a score falls below the rule's threshold an alert is raised and can be acknowledged. No dataset promotion is involved; the scores appear on the trace next to human feedback.

```bash
# a rule that scores every error trace for valid JSON output, alerting below 0.5
curl -s -X POST localhost:8000/api/v1/projects/$PID/online-rules \
  -H 'Content-Type: application/json' \
  -d '{"name":"json guard","scorers":["json_valid"],"sample_rate":1,
       "filter_status":"error","alert_threshold":0.5}'
curl -s "localhost:8000/api/v1/projects/$PID/alerts?status=open"
```

## API surface (`/api/v1`)
| Area | Endpoints |
|---|---|
| Projects | `GET/POST /projects`, `GET /projects/{id}` |
| Ingest | `POST /projects/{id}/spans:ingest` (batch of span events), `POST /v1/traces` (OTLP/HTTP, protobuf + JSON) |
| Observe | `GET /projects/{id}/traces` (incl. `?thread_id=`), `GET /projects/{id}/traces/{trace_id}`, `POST /projects/{id}/traces/{trace_id}/feedback` |
| Conversations | `GET /projects/{id}/threads`, `GET /projects/{id}/threads/{id}`, `POST …/{id}/close`, `POST …/{id}/open`, `GET/POST …/{id}/scores`, `POST …/{id}/evaluate` |
| Dashboard | `GET /projects/{id}/dashboard` (traces + conversation metrics and score aggregates) |
| Datasets | `GET/POST /projects/{id}/datasets`, `GET /datasets/{id}`, `POST /datasets/{id}/records` (single object **or** array), `POST /datasets/{id}/records:from_trace`, `GET /datasets/{id}/versions`, `GET /dataset_versions/{id}` |
| Evaluate | `GET /scorers`, `POST /projects/{id}/evaluations`, `GET /projects/{id}/experiments`, `GET /experiments/{id}`, `GET /experiments/{id}/compare?baseline_id=` |
| Prompts | `GET/POST /projects/{id}/prompts`, `GET /prompts/{version_id}` |
| Credentials | `GET/POST /keys`, `DELETE /keys/{id}`, `GET /whoami` |
| Worker | `POST /worker_callback/{experiment_id}`, `POST /worker_callback/thread/{job_id}` (results written by the Python worker) |

## Ingest from your app

**Any OpenTelemetry SDK works** — point the OTLP/HTTP exporter at `/v1/traces` and set the project
name. Traces appear with model, tokens and cost mapped from `gen_ai.*` / `llm.*` attributes.

```python
from opentelemetry.exporter.otlp.proto.http.trace_exporter import OTLPSpanExporter
exporter = OTLPSpanExporter(
    endpoint="http://localhost:8000/v1/traces",
    headers={"project-name": "my-agent"},   # auto-created on first batch
)
# then attach it to a TracerProvider with BatchSpanProcessor(...)
```

Our own SDK, for structured spans and the batched fast path:

```python
import sys; sys.path.insert(0, "sdk")
from sample_client import Client
c = Client("<project-id>", api_key="efk_...")   # api_key optional unless AUTH_MODE=required
root = c.start_span("my_agent", input="...")            # nested spans
tool = c.start_span("retrieve", type="tool", input={...}); tool.end(output={...})
llm  = c.start_span("answer", type="llm", model="gpt-4o-mini", input=[...]); llm.end(output="...")
c.flush()
```

## Use it from Claude Code / Cursor (MCP)

The binary speaks **Model Context Protocol** over two transports, so an AI host can query and operate
the platform directly — the same idea as Opik's `opik-mcp`.

**Local (stdio).** The host launches the binary; it reads `MCP_API_KEY` for its credential.

```bash
claude mcp add evalforge \
  --env MCP_API_BASE=http://localhost:8000 \
  --env MCP_API_KEY=efk_... \
  -- ./server-go/evalforge.exe --mcp
```

Or in `mcpServers` JSON (Cursor / VS Code / Claude Desktop):

```json
{
  "mcpServers": {
    "evalforge": {
      "command": "C:/absolute/path/to/server-go/evalforge.exe",
      "args": ["--mcp"],
      "env": { "MCP_API_BASE": "http://localhost:8000", "MCP_API_KEY": "efk_..." }
    }
  }
}
```

**Remote (Streamable HTTP).** Point the host at `POST /mcp` with an OAuth bearer token. The server
publishes RFC 9728 protected-resource metadata, so a compliant client discovers the authorization
server from the `WWW-Authenticate` challenge and runs the flow itself: dynamic registration →
authorization code + PKCE (S256) → short-lived access token → refresh.

```bash
curl -s http://localhost:8000/.well-known/oauth-protected-resource
curl -s -X POST http://localhost:8000/mcp \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list"}'
```

Tools: `list_projects`, `search_traces`, `get_trace`, `get_dashboard`, `list_datasets`, `get_dataset`,
`list_experiments`, `get_experiment`, `compare_experiments`, `add_feedback`, `run_evaluation`.

Then ask things like *"which traces errored in the last 24 hours?"*, *"pull trace X and tell me what the
model got wrong"*, or *"run the regression dataset against this baseline and list the regressed cases"*.

The MCP layer is an HTTP client of the API and opens no database, so it can point at any deployment,
and every tool call carries the caller's own credential — an agent can never see more than the key or
token that authorized it.

## Auth: keys, OAuth 2.1, multi-user

Two credentials resolve to the same principal (owner + scopes + optional project):

| Credential | Lifetime | Use |
|---|---|---|
| API key `efk_…` | long (optional expiry) | humans, CI, the stdio MCP server; sent as `Authorization: Bearer` or `X-API-Key` |
| OAuth access token `efa_…` | 1 hour | remote MCP clients; issued by the code + PKCE flow, refreshable |

Scopes are `read`, `write`, `admin` (`admin` implies the others). A key can be scoped to a single
project, and then every route — including agent tool calls — answers 404 for any other project.

Enforcement is off by default for the local loop. Turn it on for any shared deployment:

```bash
AUTH_MODE=required PUBLIC_BASE_URL=https://eval.example.com ./evalforge.exe
# first key: the bootstrap window lets an empty deployment mint one over HTTP,
# or mint it locally without opening a port:
./evalforge.exe --create-key "alice" --owner alice@example.com
```

With `AUTH_MODE=required` and zero keys, `POST /api/v1/keys` is allowed once and the resulting key is
forced to `admin` — otherwise a fresh deployment would be locked out of its own credential management.

```bash
KEY=$(./evalforge.exe --create-key "ci" --owner ci@example.com)
curl -s -X POST localhost:8000/api/v1/keys -H "Authorization: Bearer $KEY" \
  -H 'Content-Type: application/json' \
  -d '{"name":"dashboard","owner":"viewer@example.com","scopes":["read"]}'
```

The plaintext key is returned exactly once; only its SHA-256 hash is stored. `DELETE /api/v1/keys/{id}`
revokes the key **and every OAuth token issued from it**. The access log carries `owner` for every
authenticated request, so multi-user activity is attributable.

OAuth surface (all of it, since MCP clients require it): `GET /.well-known/oauth-protected-resource`,
`GET /.well-known/oauth-authorization-server`, `POST /oauth/register` (RFC 7591 dynamic registration,
loopback-or-https redirects only), `GET/POST /oauth/authorize` (consent page, PKCE S256 **required**),
`POST /oauth/token` (code + refresh, rotation with reuse detection), `POST /oauth/revoke` (RFC 7009).
Expired codes and tokens are scrubbed hourly.

## Scripts
```bash
python scripts/smoke_api.py     # 48-check contract run on a private server + private schema (demo data untouched)
python scripts/auth_test.py     # 44 checks: keys, scopes, PKCE flow, refresh rotation, revocation, MCP auth
python scripts/fault_test.py    # 16 checks: crash mid-eval, hung worker, worker crash + retry
python scripts/load_test.py     # concurrent ingest/query benchmark with integrity assertions
python scripts/mcp_test.py      # 23 MCP checks: handshake, tools, reads, writes, eval, errors
python worker-py/seed.py        # re-seed the demo project (no-op if it already exists)
```
`smoke_api.py` boots its own server instance on a free port with a throw-away `DATA_DIR` and a private
PostgreSQL schema, and no provider key, so it covers ingest → traces → feedback → dashboard → datasets
→ prompts → eval → compare deterministically without touching your demo data. Every harness drops its
schema on exit; pass `--base URL` to point it at an already-running server instead.

`auth_test.py` starts a second private server with `AUTH_MODE=required` and drives the flow a real MCP
client would: registration, authorize (form + PKCE), token exchange, a protected MCP call, refresh,
replay rejection, revocation, and cross-project isolation.

`fault_test.py` hard-kills the server mid-evaluation (`taskkill /T /F`), restarts it on the same
schema and DuckDB file and asserts the experiment finishes with no duplicated runs; it also proves a hung worker is
killed at `WORKER_TIMEOUT_SECONDS` and a crashed worker is retried exactly once. `load_test.py`
reports spans/second and p50/p95 for ingest, trace list and dashboard, and fails if any span is lost
or duplicated.

Environment knobs for the server/worker split:

| Variable | Default | Meaning |
|---|---|---|
| `DATA_DIR` | `../data` | base directory for the telemetry file, logs and one-time imports |
| `CONTROL_DB_URL` | `postgres://evalforge@127.0.0.1:55432/evalforge?sslmode=disable` | PostgreSQL DSN for the control plane |
| `CONTROL_DB_SCHEMA` | `public` | schema inside that database (harnesses and tests pin a private one) |
| `TELEMETRY_DB_PATH` | `$DATA_DIR/telemetry.duckdb` | DuckDB file for traces, feedback, conversations, scores |
| `LEGACY_DB` | – | extra SQLite file to import once, on top of the conventional names in `DATA_DIR` |
| `THREAD_IDLE_SECONDS` | `1800` | idle conversations close after this many seconds; new activity reopens them |
| `ADDR` | `:8000` (or `$PORT`) | listen address; on Render/Fly/Cloud Run the platform-assigned `PORT` is used when `ADDR` is unset |
| `AUTH_MODE` | `off` | `required` enforces API keys / OAuth tokens on every non-public route |
| `PUBLIC_BASE_URL` | listen address | public origin used in OAuth discovery and token audience (set it behind a proxy) |
| `WORKER_PATH` | `../worker-py/worker.py` | worker script the server executes |
| `WORKER_TIMEOUT_SECONDS` | `720` | hard cap per evaluation; a stalled worker is marked `failed` instead of running forever |
| `ONLINE_CONCURRENCY` | `2` | traces scored at once by online rules; bounds provider load |
| `ONLINE_TIMEOUT_SECONDS` | `180` | cap per online evaluation, so one slow judge cannot pin a slot |
| `RETENTION_SWEEP_SECONDS` | `3600` | how often per-project retention policies are enforced |
| `MONITOR_INTERVAL_SECONDS` | `60` | how often windowed monitors are evaluated |
| `SANDBOX_IMAGE` | `python:3.12-slim` | interpreter image custom metrics run in |
| `SANDBOX_ALLOW_UNSAFE_LOCAL` | – | `1` runs custom metrics in a local subprocess; **not an isolation boundary**, development only |
| `LOG_LEVEL` | `info` | structured JSON logs (`debug`, `info`, `warn`, `error`) |
| `GO_SERVER` | `http://localhost:8000` | where the worker calls back to |
| `MCP_API_BASE` | `http://localhost:8000` | API the stdio MCP server talks to |
| `MCP_API_KEY` | – | credential the stdio MCP server sends |

## Product notes (why this loops)
- Traces, datasets and experiments share one span-shaped data model, so production failures become regression tests without re-plumbing.
- Conversations are first-class: a trace is one turn, `thread_id` groups turns, and thread-level scores are stored separately from per-trace scores so multi-turn quality is measurable on its own.
- Online evaluation has two scopes: a `trace` rule scores one request on ingest, a `thread` rule scores the whole conversation when it closes. Both write scores without a human, and alerts name their subject (`trace` or `thread`).
- Monitors watch aggregates, not requests: `error_rate > 5%` over a window fires once and resolves when it recovers, and can deliver to a webhook or Slack. Delivery success is recorded on the event, so a broken channel is visible.
- Retention is a per-project policy, enforced by a sweeper on the telemetry plane; `apply_to_past=false` stops a new policy from deleting history.
- Custom Python metrics are accepted only through a sandbox (Docker: no network, read-only, non-root, caps dropped, resource caps). With no sandbox reachable they fail closed — they never fall back to the host.
- Guardrails are protection, not scoring: a policy runs on ingest and through `POST /guardrails/check`, and its action is `flag`, `redact` or `block`. The audit log stores a redacted preview, never the matched value.
- Optimization closes the loop: a run measures a baseline and prompt candidates on a pinned dataset snapshot and promotes the winner only when it actually improved. A tie is recorded and discarded.
- Guardrails are protection, not evaluation: a policy runs before telemetry is stored and can be called before text reaches a model. `flag` records, `redact` masks, `block` refuses — and the audit log keeps a masked preview, never the matched value.
- Optimization closes the loop: a run measures a baseline prompt and a set of candidates on the same pinned dataset snapshot, then promotes the winner only if it actually scored higher. A tie or a regression is reported and discarded.
- Honest scoring: every score carries its `judge_mode` (`llm` vs `heuristic_fallback`) and an unparseable judge answer is recorded as *absent*, never as a silent `0.0`. Conversation judges also fall back to deterministic heuristics when the provider is configured but broken.
- Metering-friendly by design: spans carry byte/estimable payload; every recorded score is attributable (the two metrics Braintrust bills on).



