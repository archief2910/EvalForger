# Opik → EvalForge: architecture review and corrections

This document is the result of reading the Opik source in [`opik/`](../../opik) (Java/Dropwizard backend, React frontend,
Python + TypeScript SDKs) and comparing it, decision by decision, with our Go + Python platform.

It is deliberately **not** a port. Opik is a multi-tenant SaaS-shaped system built by a large team; we are one Go
binary plus one Python worker. The useful output of reading it is a list of *decisions worth copying*, *decisions worth
adapting*, and *decisions to reject at our scale* — each with the idiomatic Go shape it should take.

---

## 1. What Opik actually is

| Layer | Opik | Evidence |
|---|---|---|
| API | Java 21, Dropwizard + Jersey, ~40 JAX-RS resources under `/v1/private` | [`api/resources/v1/priv`](../../opik/apps/opik-backend/src/main/java/com/comet/opik/api/resources/v1/priv) |
| Transactional store | MySQL, Liquibase migrations (`000046` referenced in code) | [`DatasetVersionDAO.java`](../../opik/apps/opik-backend/src/main/java/com/comet/opik/domain/DatasetVersionDAO.java) |
| Telemetry store | ClickHouse (`clickhouse` + `clickhouse-init` services, `clickhouse_config/`) | [`deployment/docker-compose`](../../opik/deployment/docker-compose/docker-compose.yaml) |
| Cache / locks / id-map | Redis (Redisson), used for OTel→Opik trace id mapping | [`OpenTelemetryService.java`](../../opik/apps/opik-backend/src/main/java/com/comet/opik/domain/OpenTelemetryService.java) |
| Blobs | MinIO/S3 for attachments | compose services `minio`, `minio-init` |
| Ingest | OTLP HTTP **protobuf + JSON**, plus native REST batch ingest | [`OpenTelemetryResource.java`](../../opik/apps/opik-backend/src/main/java/com/comet/opik/api/resources/v1/priv/OpenTelemetryResource.java) |
| Evaluation compute | Java job executor + Redis queue; Python for optimizers/guardrails | `domain/*Job*`, `apps/opik-python-backend` |
| API contract | OpenAPI generated from Java annotations, consumed by Python/TS SDKs | `apps/opik-documentation` |

**The single most important structural decision in Opik is the split between MySQL and ClickHouse.** Metadata
(projects, datasets, experiments, prompts, feedback definitions) is small, transactional, and needs unique constraints.
Telemetry (traces, spans, feedback scores) is append-only, huge, and needs columnar scans for dashboards. Forcing both
through one engine is why every "just use Postgres for everything" observability product hits a wall around 10⁷ spans.

We implemented that split: the control plane now runs on PostgreSQL (small, transactional, unique constraints) and
telemetry on DuckDB (columnar, append-heavy, one file). Statements stay single-spelled with `?`; a driver-level shim
rewrites them to `$1..$n` for PostgreSQL. The embedded DuckDB is the stand-in for ClickHouse — same boundary, no
cluster to operate yet.

---

## 2. Contract-level facts worth matching exactly

These are cheap to copy and expensive to get wrong, because they are the parts a real SDK or user touches.

| Contract | Opik | Why it matters |
|---|---|---|
| OTLP path | `POST /v1/private/otel/v1/traces` (note: **not** `/v1/traces`) | Exporters are configured with a base URL; the path is what the user pastes |
| OTLP encodings | `application/x-protobuf` **and** `application/json` | Every OTel SDK defaults to protobuf; JSON is what `curl` and the collector use |
| OTLP project routing | `project-name` header, default project if absent | Projects must be selectable without editing the payload |
| Empty OTLP export | `200` no-op, and **no project created** | Collectors send empty batches; a 500 causes retry storms |
| Dataset versioning | immutable version per write, identified by commit hash, `latest` tag, delta counts | Reproducibility: an experiment must be tied to the items it actually saw |
| Experiment → dataset | `datasetVersionId`, auto-linked to latest at creation | Same |
| Prompt versioning | append-only versions with commit hash + `v<N>`, linked from experiments | A/B testing requires pinning *a version*, not "the prompt" |
| Experiment status | `running` / `completed` / `cancelled` | Client-driven completion; distinguishes "still working" from "abandoned" |
| Score definitions | `numerical` (min/max) and `categorical` (category → number) | Stored definitions, not hardcoded metrics |

---

## 3. Where our repo stands (measured, not remembered)

`server-go/main.go` is 1,579 lines in one `package main`:

- routing by splitting `r.URL.Path` strings in `main()` — no router, no route table, no method dispatch table;
- one `*sql.DB` was **pinned to a single connection** behind a global `dbMu sync.Mutex`, because SQLite's default
  `delete` journal mode serialises writers anyway. Now: WAL, an 8-connection pool, `busy_timeout(5000)`, write
  serialisation kept explicit;
- `schema` as a Go string constant, applied with `CREATE TABLE IF NOT EXISTS` — no migration versioning, no down path,
  and no way to alter a column on an existing database;
- evaluation dispatch **was** one goroutine + `exec.CommandContext(python worker.py)`; a restart left the experiment
  in `running` until a timeout fired on a goroutine that no longer existed. Now: a `jobs` row with claim/lease/
  heartbeat, boot requeue and a reaper — proven by hard-killing the server mid-eval (see `scripts/fault_test.py`);
- no health/readiness distinction (auth has since been added: see D13). Request ids, JSON access logs, panic recovery and graceful shutdown
  were added in this pass;
- prompt versions exist as rows, but `POST /evaluations` accepts no prompt reference, so the worker invents its own
  message — the Prompts page is decorative.

Everything below is ordered by *what breaks first when someone other than us uses it*.

**Status of this pass:** D1 (partly), D2, D6, D9, D12, D13, D14, D15, D16, D17, D18, D19 and D20 are implemented. The rest are decisions with a written
implementation path, not code.

### Implemented since the review

| Change | Evidence |
|---|---|
| OTLP/HTTP ingest (`POST /v1/traces`, protobuf **and** JSON, `project-name` routing, empty-batch no-op, idempotent by span id) | `server-go/otlp.go`, `server-go/otlp_test.go`, 5 new smoke checks — verified with the real Python OTel SDK over protobuf |
| SQLite WAL + connection pool + busy timeout | `openDB`; `PRAGMA journal_mode` now reports `wal` |
| Request ids, JSON access logs, panic recovery, graceful shutdown | `server-go/middleware.go`, `main()` |
| Go HTTP-level tests | `server-go/otlp_test.go` (protobuf, JSON, retry, empty batch, malformed body) |
| Durable job queue: claim/lease/heartbeat, boot requeue, reaper, one retry on abnormal exit | `server-go/jobs.go`; `scripts/fault_test.py` |
| Fault injection suite (crash, timeout, retry) | `python scripts/fault_test.py` → **16/16**, including a real `taskkill /T /F` mid-evaluation |
| Load benchmark with integrity assertions | `python scripts/load_test.py` → 1,080 spans in 1.4–2.8 s (390–770 spans/s), read p95 ~33 ms, 0 errors, 0 duplicates |
| UI end-to-end (feedback → promote → eval → compare) | driven in a browser; dataset v3→v4, experiment completed with 7 runs, job `done attempts=1` |
| MCP server (`--mcp`): 11 tools over stdio JSON-RPC, API-client design | `server-go/mcp.go`; `scripts/mcp_test.py` **23/23**; also driven by the official `mcp` Python SDK |
| Control/data plane split (PostgreSQL + DuckDB), one-time SQLite import, and first-class conversations with thread-level scoring | `internal/database/{db,import,migrate,legacy}.go`, `internal/pgbind`, `internal/api/threads.go`; smoke **41/41**, fault **16/16**, auth **44/44**, MCP **23/23** |
| Online evaluation (D14): rules over live traffic, deterministic sampling, idempotent scores, threshold alerts | `internal/online`, `internal/api/online.go`, `worker.py --online`; smoke 48/48 at the time |
| Retention policies (D16): per-project rules, children-first sweeper, `apply_to_past` | `internal/retention`, `internal/api/retention.go`, `control/0006_retention_rules.sql`; 5 Go tests, smoke checks |
| Windowed monitors (D17): metric-over-window state machine, webhook/Slack delivery, delivery recorded | `internal/monitor`, `internal/api/monitor.go`, `control/0007_monitors.sql`; 7 Go tests |
| Conversation-scoped online rules (D18): score on thread close, thread alerts with a typed subject | `internal/online` (scope), `internal/api/online_thread_test.go`, `worker.py --online --thread-id`; 6 Go tests |
| Sandboxed custom metrics (D15): Docker runner, fail-closed local runner, registry API | `internal/sandbox`, `internal/api/custom_scorers.go`; 5 sandbox tests, 7 API tests |
| Documented corrections and roadmap | this file |

---

## 4. Decisions

### D1 — Split metadata from telemetry: PostgreSQL + DuckDB (implemented)

**Opik:** MySQL for metadata, ClickHouse for traces/spans.
**Us (before):** two SQLite files — the right boundary, the wrong engines.

**Decision: adopt the boundary *and* the engines, embedded where possible.** The control plane is PostgreSQL
(`CONTROL_DB_URL`, schema `CONTROL_DB_SCHEMA`); telemetry is a DuckDB file (`TELEMETRY_DB_PATH`). Handlers still
issue SQL directly and merge planes in Go; the split is physical, so each engine has its own migrations, backup and
retention path.

**Why DuckDB rather than ClickHouse now:** the boundary is what buys the option. DuckDB is columnar, scans spans for
dashboards without loading an index per row, and is a single file — no extra service for a laptop or a small
deployment. When span volume justifies a cluster, only the telemetry plane changes: the API surface, the merge logic
and the SQL shape stay.

**One spelling of SQL.** Every statement uses `?`. DuckDB accepts it natively; `internal/pgbind` wraps the pgx driver
and rewrites `?` → `$1..$n` at `Prepare`, `ExecContext`, `QueryContext` and inside `driver.Tx`, skipping literals,
quoted identifiers and comments. That keeps transactions and the dynamic filter builders honest without maintaining
two copies of ninety statements.

**Data migration is part of the feature.** `internal/database/import.go` reads any `ai_eval.db`, `telemetry.db`,
`control.db` (or `LEGACY_DB`) once, routes each whitelisted table to its plane, inserts with `ON CONFLICT DO NOTHING`
and records the file in `import_state`. SQLite files are opened read-only and retained. Booting this build against the
previous `data/` directory transported 245 spans, 28 feedback rows, 25 eval runs, 56 conversations, 28 scores and
the full metadata set in one run.

**Costs, stated plainly:** DuckDB needs cgo (gcc on Windows), allows one writer process per file, and is not a
network service — a second API instance needs its own file or a different telemetry backend. PostgreSQL must be
reachable before the server starts; there is no embedded fallback by design, because silently falling back is how
two divergent schemas happen.

### D2 — Ingest speaks OTLP, not a bespoke schema

**Opik:** `/v1/private/otel/v1/traces`, protobuf + JSON, `project-name` header, empty batch → `200`.
**Us:** `POST /api/v1/projects/{pid}/spans:ingest` with our own JSON, so **no third-party SDK can send us traces**.

**Decision: implement OTLP/HTTP now, keep the native endpoint as a fast path.** This is the difference between "an eval
demo" and "an observability backend". Mapping rules follow Opik's:

| OTel attribute | Our column |
|---|---|
| `gen_ai.request.model`, `gen_ai.response.model`, `llm.model_name`, `model` | `spans.model` |
| `gen_ai.usage.input_tokens` / `output_tokens` | `prompt_tokens` / `completion_tokens` |
| `llm.token_count.prompt` / `completion` | same |
| `input`, `output`, `gen_ai.prompt`, `gen_ai.completion` | `input` / `output` |
| `openinference.span.kind`, `gen_ai.operation.name` | `spans.type` |
| span status `ERROR`, or `error` attribute | `spans.status` |

Implementation in `internal/ingest/otlp.go`: decode with `protojson` (JSON) or `proto.Unmarshal` (protobuf) into
`collectortrace.ExportTraceServiceRequest`, flatten `ResourceSpans → ScopeSpans → Spans`, derive our trace id from the
OTLP `TraceId` (hex, 32 chars), keep `SpanId` hex as the primary key (**idempotent by construction**), and reuse the
existing insert transaction. Return the OTLP `ExportTraceServiceResponse` (empty message on success).

### D3 — Dataset versions are immutable snapshots, not a counter

**Opik:** `dataset_versions` rows with `version_hash`, `items_total/added/modified/deleted`, `latest` tag; experiments
pin `dataset_version_id` at creation.
**Us:** `datasets.version INTEGER` that nothing reads, and experiments that re-read the live table.

**Decision: snapshot on write.** Each mutation inserts a version row containing the full item set (JSON), a content
hash, and delta counts; `datasets.latest_version_id` moves forward. Experiments store `dataset_version_id` and the
worker reads *that snapshot*, never the live table.

```sql
CREATE TABLE dataset_versions (
  id TEXT PRIMARY KEY, dataset_id TEXT NOT NULL, seq INTEGER NOT NULL,
  version_hash TEXT NOT NULL, items_json TEXT NOT NULL,
  items_total INTEGER, items_added INTEGER, items_modified INTEGER, items_deleted INTEGER,
  created_at TEXT NOT NULL, UNIQUE(dataset_id, version_hash)
);
```

Storage cost is real (full snapshot per write). That is exactly why Opik moved to deltas; at our size, snapshots are
the honest, obvious implementation, and `items_json` is the migration seam if it ever gets large.

### D4 — Prompts are versioned and pinned by experiments

**Opik:** `PromptVersion` with commit hash and `v<N>`, `Experiment.promptVersions[]` linking the exact versions used.
**Us:** prompt rows with a `version` integer, unused by evaluations.

**Decision:** append-only `prompt_versions`; `POST /projects/{id}/evaluations` accepts
`prompt: {name, commit | version_number}`; the resolved template is passed to the worker on the command line/env and
recorded on the experiment. Without this, the Prompts page cannot justify existing.

### D5 — Feedback definitions live in the database

**Opik:** `FeedbackDefinition` as `numerical{min,max}` or `categorical{category→score}`.
**Us:** `scorers` table that is empty and unused; the registry is a Go literal, so custom scoring requires a rebuild.

**Decision:** seed the registry from Go on first boot, then read it from the DB; expose
`POST /projects/{id}/feedback-definitions`. Deterministic scorers stay in Python; the *definition* (name, kind, config)
becomes data so a new numeric score can be created without a deploy.

### D6 — Durable jobs, not a goroutine

**Opik:** queue + workers, retries, per-item progress.
**Us:** `go runWorkerJob(...)` behind `workerMu`; a restart strands the experiment.

**Decision (implemented):** a `jobs` table with claim/lease semantics; one dispatcher goroutine polls it every 500 ms.
Restart-safety is a property, not a hope: at boot every `running` job is requeued (this process is the only writer), a
heartbeat renews the lease every 15 s while the worker runs, and a reaper requeues expired leases on a ticker. `asynq`+Redis (the boilerplate's choice) is the right answer *when Redis
already exists*; introducing Redis only for job dispatch would add a second failure domain to a single-binary app, so
keep `JobQueue` an interface and swap the implementation later.

```sql
CREATE TABLE jobs (
  id TEXT PRIMARY KEY, kind TEXT NOT NULL, experiment_id TEXT NOT NULL, project_id TEXT NOT NULL,
  payload TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'queued',      -- queued | running | done | failed
  attempts INTEGER NOT NULL DEFAULT 0, max_attempts INTEGER NOT NULL DEFAULT 2,
  lease_expires_at TEXT, last_error TEXT,
  created_at TEXT NOT NULL, updated_at TEXT NOT NULL
);
-- claim: UPDATE jobs SET status='running', attempts=attempts+1, lease_expires_at=?
--        WHERE id=? AND (status='queued' OR lease_expires_at < now())   -- the UPDATE is the lock
```

The reaper is a ticker, not a startup special case: expired leases must be reclaimed while the process runs.

### D7 — Errors are typed, and one handler writes them

**Opik:** exception mappers per resource, stable JSON error bodies.
**Us:** `errJSON(w, 500, err.Error())` scattered through 1,500 lines, leaking SQL text to clients.

**Decision:** one `*api.Error{Code, Message, Status, Err}` type; every handler returns it; a single middleware renders
it and logs `err.Err` with the request id. Never render `err.Error()` of a driver error to a client.

### D8 — Configuration is a validated struct, loaded once

**Opik:** Dropwizard YAML config bound to immutable objects.
**Us:** ~20 `os.Getenv` calls spread across the file, defaults inline, no validation.

**Decision:** `internal/config` with a typed struct, defaults, and `Validate()`; fail fast at boot on a bad value
instead of silently misrouting evals (we already shipped that bug once with `GO_SERVER`).

### D9 — Observability: request IDs and structured logs before APM

**Opik:** Dropwizard metrics + OTel internals + per-operation timers.
**Us:** `log.Printf` with no correlation.

**Decision:** `log/slog` JSON handler, a `request_id` middleware that also sets `X-Request-Id`, and a per-request
completion log (method, path, status, duration_ms, request_id). New Relic (the boilerplate's choice) plugs into the same
seam later; the seam matters more than the vendor.

### D10 — Migrations are numbered and embedded

**Opik:** Liquibase changelogs with explicit version numbers.
**Us:** one `CREATE TABLE IF NOT EXISTS` blob.

**Decision:** `internal/database/migrations/0001_init.sql`, `0002_…`, embedded with `embed.FS`, applied in order inside
a transaction, recorded in `schema_migrations(version, applied_at)`. No down migrations (Opik's Liquibase also runs
up-only here); forward-only is fine when every release ships a migration.

### D11 — Tests hit the HTTP surface, not the internals

**Opik:** JUnit integration tests against MySQL/ClickHouse containers.
**Us:** one out-of-band Python smoke script, plus a Go repo with no `_test.go`.

**Decision:** `httptest`-based handler tests in Go (`internal/api/*_test.go`) using a private PostgreSQL schema and a temporary DuckDB file per test, plus the
existing black-box smoke script for cross-process checks. Testcontainers only becomes relevant when Postgres arrives.

### D13 — Expose the platform to agents over MCP (implemented)

**Opik:** `opik-mcp`, listed as "Drive Opik from Claude Code, Cursor, or VS Code via Model Context
Protocol", plus an OAuth 2.1 authorization server in the backend (`oauth/authorize`, `oauth/token`,
`oauth/register`, `/.well-known/oauth-authorization-server`, PKCE S256, token scrub job). MCP is an
*agent interface*, not an ingest path.
**Us:** nothing — the platform was only reachable through a browser or hand-written HTTP.

**Decision: implement an MCP server in the same binary, as a client of the public API, and the OAuth
2.1 authorization server that remote clients require.**

```bash
MCP_API_BASE=http://localhost:8000 ./server-go/evalforge.exe --mcp
```

Why this shape, in Go terms:

- **Stdio transport, newline-delimited JSON-RPC 2.0.** The host spawns the process; stdout carries
  protocol messages, logs go to stderr (`log/slog` elsewhere is unaffected). No HTTP server, no
  websocket, no SSE — one `bufio.Scanner` over `os.Stdin`.
- **The MCP process is an API client.** It does not open either database. That keeps one writer process
  for the control plane and one for the DuckDB file (D6), makes the tool layer testable without fixtures,
  and lets an editor on a laptop drive a remote deployment by changing `MCP_API_BASE`.
- **11 tools, read-first with two writes** (`add_feedback`, `run_evaluation`) so an agent can inspect
  traces, datasets, dashboards and experiments, then act. `project_name` is accepted anywhere an id is,
  so a model can say "Demo Support Agent" instead of resolving ids first.
- **Spec-correct error semantics:** unknown tool → JSON-RPC `-32602`; a tool that runs and fails →
  `result.isError = true`; unknown method → `-32601`.

**OAuth is no longer skipped.** It existed in Opik to let remote, multi-user MCP clients authenticate,
and the same need appears the moment the endpoint leaves loopback. Implemented in
[auth.go](../server-go/auth.go):

| Piece | Spec | Note |
|---|---|---|
| API keys (`efk_…`) with `read`/`write`/`admin` and optional project scope | – | SHA-256 hash stored; plaintext shown once; revocation kills derived tokens |
| Protected Resource Metadata | RFC 9728 | WWW-Authenticate points at the metadata document |
| Authorization Server Metadata | RFC 8414 | PKCE S256 is the only advertised method |
| Dynamic client registration | RFC 7591 | loopback-or-https redirects, no implicit/password grants |
| Authorization code + PKCE | RFC 6749 / 7636 | code is single-use (conditional `UPDATE` is the lock), 5-minute TTL |
| Refresh rotation + reuse detection | RFC 6749 §10.4 | a reused refresh token revokes the whole family |
| Revocation | RFC 7009 | always `200`, refresh token revokes the family |
| Token scrub | – | hourly ticker, Opik's job equivalent |

What is still missing is a real *user* directory: the consent screen has the operator paste an API key,
which is honest for a token-based product but not a login. Orgs/teams and a session cookie are the next
step, not a hidden gap.

~~The MCP layer is loopback-only until then.~~ With `AUTH_MODE=required`, `POST /mcp` demands a bearer
token and per-tool scopes; evidence is `scripts/auth_test.py` (44/44).

### D14 — Online evaluation: score live traffic, not only datasets (implemented)

**Opik:** online evaluation rules evaluate production traces continuously — a filter, a sampling rate,
a set of metrics — and write the scores back to the trace. Alerts/monitors sit on top.
**Us (before):** every score came from a dataset experiment a human started. Production could regress for
days without anyone noticing.

**Decision: implement the rule loop with the trusted scorer registry.** A rule stores
trace filters (root status, model, name substring), a sample rate, scorer names and an alert threshold.
On ingest — native and OTLP — the handler projects the batch into `{trace, root name, status, models}`,
matching costs no extra query, and scoring runs **off the request path** in a bounded pool
(`ONLINE_CONCURRENCY`, `ONLINE_TIMEOUT_SECONDS`).

- **Deterministic sampling:** `hash(rule_id|trace_id) < sample_rate`. Retries and replays make the same
decision; a partial sample never becomes "score the noisy trace twice".
- **Idempotent persistence:** `online_scores` is unique on `(rule_id, trace_id, scorer)`, the
  already-scored check runs before the worker starts, and an alert is written only for a score row that
  was actually inserted. A retried ingest cannot duplicate scores or alerts.
- **Execution:** a new `worker.py --online` mode fetches the stored trace, resolves scorer kinds from the
  registry, runs them, and prints one JSON document (progress stays on stderr). The server writes the
  rows, so no new callback endpoint or auth window exists.
- **Planes:** rules/alerts are control rows, scores are telemetry rows; the trace view, the alerts list
  and the dashboard block merge them in Go, like every other cross-plane read.
- **Guardrail:** rule validation accepts only `scope=record` scorers from the registry. There is no path
  from an API request to arbitrary code execution, which is what keeps D15 optional.

**What we deliberately do not copy yet:** Opik's streaming evaluators (we spawn one worker per trace),
windowed monitors that fire once for an aggregate condition (our alert is per score), and notification
channels (Slack/email/PagerDuty). Those are product breadth, not protocol.

### D15 — Custom Python metrics run in a sandbox (implemented)

**Opik:** users write Python metrics, so the backend executes untrusted code. The answer is a
disposable sandbox: container-per-job, no network, non-root, read-only root filesystem, memory/CPU/PID
caps, hard timeout. Opik ships exactly this as `apps/opik-sandbox-executor-python`: a container that
takes code plus a payload and runs it under a non-root user, with the runner compiled to bytecode and
package managers removed so there is nothing to install at run time.

**Decision: accept custom code only through a sandbox, and fail closed when there isn't one.**

We implemented the boundary rather than the feature alone:

| Piece | Shape |
|---|---|
| Execution | `internal/sandbox`: `Runner` interface over `DockerRunner` and a refusing `LocalRunner` |
| Flags | `--network none --read-only --user 65534:65534 --memory 256m --pids-limit 64 --cpus 1 --cap-drop ALL --security-opt no-new-privileges --tmpfs /tmp:rw,noexec,nosuid,size=16m` |
| Contract | `def score(payload) -> float \| {score, rationale}`, or a class with a `score` method |
| Storage | `custom_scorers` (control plane). Storing code is not running it. |
| Boundary | `online.Engine.splitScorers` decides which names are custom, so the trusted worker never sees user code |
| Refusal | no Docker → `503`, no score, `sandbox_available: false`; `LocalRunner` refuses unless `SANDBOX_ALLOW_UNSAFE_LOCAL=1` |

What we did **not** do: a hardened subprocess on the host. A subprocess with a timeout is not a security
boundary, and shipping one behind a friendly name is how a security model rots. The local runner exists
for iterating on a metric on a laptop, it says so in its error message, and it logs a warning at boot.

**Verified:** the runner's contract end to end (function, dict return, class form, payload reading,
syntax error, missing `score`, raised exception, non-numeric score, out-of-range score) and the refusal
paths (no daemon → `ErrUnavailable`; local runner → refuses; API → `503`). The Docker path itself is
unverified on this machine because no daemon is running — stated plainly rather than implied.

### D16 — Retention is a policy on the control plane, enforced on telemetry (implemented)

**Opik:** `RetentionRule` per project with a retention period, `apply_to_past`, and a catch-up cursor.
**Us (before):** telemetry was kept forever; the plane split made per-plane retention *possible* but
nothing enforced it.

**Decision: the rule is control state, the sweep is a telemetry job.** Deleting is ordered
children-first — feedback and automatic scores reference traces, so they go before the spans, and a
conversation whose turns are all gone goes with its scores. `apply_to_past=false` bounds the window at
the rule's creation time, so adopting a policy cannot retroactively delete history.

**Costs:** the sweep is a ticker, not a distributed job; a very large project sweeps in one transaction,
which DuckDB handles but which would want batching at ClickHouse scale.

### D17 — Monitors add a state machine to alerting (implemented)

**Opik:** metric alerts with a threshold, an operator, a window, and a debouncing bucket; channels are
general webhook / Slack / PagerDuty.
**Us (before):** alerts were a per-score threshold. That tells you a trace scored badly, not that the
fleet is unhealthy, and it re-fires forever while the condition holds.

**Decision: a monitor is an aggregate over a window with a state machine.** `ok → firing` emits one
event and one notification; `firing → ok` resolves it. Metrics are `error_rate`, `avg_score`,
`avg_latency_ms`, `p95_latency_ms`, `avg_cost_usd`, `span_count`, `trace_count`, `error_count`.
Delivery is recorded on the event (`notified`, `notification_error`), so a broken channel is visible
rather than silent.

**What we did not copy yet:** PagerDuty and email channels, and a Redis-backed debounce bucket (a single
dispatcher and a SQL state transition are the lock here).

### D18 — Conversation-scoped online evaluation (implemented)

**Opik:** `TraceThreadOnlineScorerPublisher` enqueues threads when they close, and the automation-rule
types include `trace_thread_llm_as_judge` and `trace_thread_user_defined_metric_python`.
**Us (before):** online rules only scored single traces; the thread scorers existed but only ran from a
manual/dataset evaluation job.

**Decision: scope is part of the rule, and the trigger is the close.** A `thread` rule is evaluated when
a conversation closes (idle sweep or manual close) and runs thread-scoped scorers through
`worker.py --online --thread-id`. Scores land in `online_thread_scores` (unique on
`(rule_id, thread_id, scorer)`), and an alert points at the conversation via
`subject_type='thread'`.

This is the most important parity item for agent evaluation: a single trace says "this request went
wrong"; a conversation says "the agent forgot the user's name after turn four". Those are different
products, and only the second one is what an agent team needs.

### D19 — Guardrails are a runtime layer, not a scorer (implemented)

**Opik:** a separate guardrails service (`apps/opik-guardrails-backend`) validates text against PII,
topic, prompt-injection and custom-classifier checks, and the backend stores `Guardrail` rows attached
to the entity that was checked.
**Us (before):** `regex_guard` existed — an offline scorer that runs *after* the fact on a dataset or a
trace. Nothing stopped a value from being stored or sent.

**Decision: protection is a separate mechanism with an action, not a score.**

| Piece | Shape |
|---|---|
| Checks | `PII` (email, phone, card with Luhn, SSN, IBAN, IP, URL), `PROMPT_INJECTION` (override phrasings), `TOPIC` (restrict or allow), `CUSTOM` (operator regexes and keywords) |
| Actions | `flag` records the finding, `redact` masks before storage, `block` refuses the batch |
| Runtime endpoint | `POST /projects/{id}/guardrails/check` — what an application calls before it sends text to a model |
| Ingest enforcement | native and OTLP paths run the same checks before a span is written |
| Audit | `guardrail_events` on the telemetry plane, with a redacted preview; the original is never stored |

Two deliberate calls:

- **The strictest policy wins.** A `block` policy is not watered down by a `flag` policy that also
  matched, and a mixed set reports the strongest action.
- **The event log cannot become the leak.** The preview is redacted and truncated, because a guardrail
  that stores what it blocked has moved the problem rather than solved it.

**What we did not copy:** a trained custom classifier. Opik fine-tunes a small model
(`Qwen2.5-1.5B-Instruct`) from labelled examples and serves it. We ship a `CUSTOM` check over operator
regexes and keywords instead, and say so: no training pipeline, no inference service. Card detection is
gated on the Luhn checksum so order numbers are not reported as PII — a guardrail that cries wolf gets
switched off.

### D20 — Optimization closes the loop by changing something (implemented)

**Opik:** Optimization Studio runs `opik_optimizer` in a separate Python worker over a dataset and
metrics, tries prompt variants, and records an `Optimization` row with baseline and best objective
scores. It is a separate worker because optimization is long-running and compute-heavy.
**Us (before):** the loop stopped at "compare experiments". Nothing produced a better prompt.

**Decision: a durable job that proposes, measures and promotes.**

```
POST /optimizations  (dataset snapshot + baseline prompt + scorers pinned)
        │
        ▼
 jobs.kind='optimize' ──► worker.py --optimize
                             │  baseline + candidate strategies
                             │  (concise, step_by_step, grounded, structured, no_hedge)
                             ▼
                     score every candidate on the same snapshot
                             │
                             ▼
              POST /worker_callback/optimization/{id}
                             │
                             ▼
        better than baseline? ──► new prompt version ("optimized: <run>")
                             └─► tie or worse: recorded, discarded
```

- **Pinned inputs.** The dataset snapshot, baseline prompt version and scorer list are stored on the
  run, so re-reading the live table cannot change what was measured.
- **Promotion is conditional.** A candidate that ties or loses is reported and never becomes a prompt.
  This is the difference between a search and a rubber stamp.
- **The job is durable.** It goes through the same claim/lease/reaper queue as evaluations, so a restart
  mid-run does not strand it; a failed run records its reason on the row.
- **The winner is a normal prompt version**, listed by `GET /prompts/{id}`, so the existing experiment
  flow can pin and compare it.

**What we did not copy:** a learned optimizer. Opik's `opik_optimizer` supports evolutionary and
few-shot strategies; ours is a fixed set of instruction mutations measured honestly. That is a real
capability with an honest ceiling, rather than a claim of parity.

### D12 — Reject these, with reasons

| Opik pattern | Why not here |
|---|---|
| Reactor `Mono`/`Flux` pipelines in services | Go's `context` + explicit errors are simpler; reactive composition is what you do when threads are expensive |
| Java DAO per table with annotation mappers | `database/sql` + small typed structs is the idiomatic equivalent; don't build an ORM |
| 40 JAX-RS resources before the core loop is correct | Breadth without depth; annotation queues and automation rules are meaningless without reliable ingest |
| Redisson distributed locks | Single dispatcher; PostgreSQL transactions + unique constraints are the lock |
| Base64-ing OTel ids in Redis as an id-map | We can key spans by the OTel span id directly; no mapping table needed |
| Separate Python backend for guardrails | Guardrails are a request-path decision with an action; a second service would add a network hop to the hot path for checks that are regexes and word lists. In-process is faster and simpler. (The sandbox *is* separate, because it runs untrusted code — a different problem.) |
| A learned prompt optimizer | A fixed set of measured instruction mutations is honest and testable; claiming evolutionary search without the library behind it would not be |

---

## 5. Target Go layout

```text
server-go/
  cmd/server/main.go              # wiring only: config → logger → db → repos → services → router → serve
  internal/
    config/config.go              # typed env config + Validate()
    logging/logging.go            # slog JSON, request-scoped logger
    database/
      db.go                       # open, pragmas, ping, close
      migrate.go                  # embed.FS runner + schema_migrations
      migrations/0001_init.sql
    model/                        # Span, Trace, Dataset, Experiment, Prompt, Job
    repository/
      repository.go               # interfaces (D1)
      postgres/                   # one store per aggregate (control plane)
      duckdb/                     # telemetry plane: spans, feedback, eval runs
    service/
      ingest.go                   # native + OTLP normalisation, cost, tree rollup
      eval.go                     # experiment lifecycle, dispatch
      jobqueue.go                 # claim/lease/reap (D6)
    api/
      router.go                   # echo.New(); route table in one place
      errors.go                   # api.Error + error handler (D7)
      middleware.go               # request id, recover, access log, CORS, auth
      handlers_*.go               # one file per resource, thin: bind → service → respond
    worker/client.go              # exec/python boundary, timeout, env
```

Wiring stays boring and explicit — no DI framework:

```go
cfg := config.MustLoad()
log := logging.New(cfg.Log)
db, err := database.Open(cfg.DB.Path)
if err != nil { log.Error("db open", "err", err); os.Exit(1) }
defer db.Close()
if err := database.Migrate(db); err != nil { ... }

tel := sqlite.NewTelemetry(db, log)
meta := sqlite.NewMetadata(db, log)
ingestSvc := service.NewIngest(tel, log)
evalSvc := service.NewEval(meta, worker.New(cfg.Worker), log)
```

Router is a table, not string splitting:

```go
e := echo.New()
e.HTTPErrorHandler = api.ErrorHandler(log)
e.Use(middleware.RequestID(), api.Recover(log), api.AccessLog(log), middleware.CORS())
v1 := e.Group("/api/v1")
v1.GET("/projects", h.ListProjects)
v1.POST("/projects/:pid/spans:ingest", h.Ingest)
v1.POST("/v1/traces", h.OTLP)          // OTLP/HTTP, root-mounted like a collector
v1.GET("/projects/:pid/traces", h.ListTraces)
v1.GET("/experiments/:eid", h.GetExperiment)
```

`echo` is a reasonable choice for the interview target and is what the reference boilerplate uses. The migration is
mechanical because handlers are already `(w, r)`-shaped; `pgx` slots in only when Postgres arrives.

---

## 6. Gap roadmap

Ordered by *what makes the product real*, not by what is fun to build. Each item has a testable acceptance criterion.

### P0 — blocks anyone else using it

| # | Gap | Acceptance criterion |
|---|---|---|
| 1 | ~~No OTLP ingest~~ **(done)** | Verified: Python OTel SDK → `POST /v1/traces` (protobuf) produced a 2-span trace with `model=gpt-4o-mini`, tokens 50/20, cost 0.0000195; empty batch returns 200 and creates nothing; retry does not duplicate |
| 2 | No auth or tenancy | Every `/api/v1` route requires a key; a key scopes to one workspace; cross-workspace reads return 404 |
| 3 | ~~Evals not durable~~ **(done)** | `taskkill /T /F` mid-eval → restart requeued the job → experiment `completed`, 3 runs, no duplicates. Hung worker → `failed` with a reason; worker crash → retried (16/16 fault checks) |
| 4 | No migration story | A schema change on an existing DB applies without data loss; `schema_migrations` matches the embedded set |

### P1 — makes the loop mean something

| # | Gap | Acceptance criterion |
|---|---|---|
| 5 | Prompts unused by evals | Two experiments pinning prompt v1 and v2 produce different outputs and a comparable score delta |
| 6 | Datasets not versioned | Adding a record after an experiment leaves that experiment's inputs unchanged; the experiment reports the version hash it used |
| 7 | Token/cost estimated as `len/4` | Usage comes from the provider response when present; `estimate` is labelled in the UI when not |
| 8 | Scorer registry is code | Create a numerical feedback definition via API; score a trace with it; it appears in aggregates |
| 9 | No trace filtering/ordering parity | List traces by status/model/feedback/latency with stable cursor pagination |

### P2 — makes it a product

| # | Gap | Acceptance criterion |
|---|---|---|
| 10 | Embedded DuckDB telemetry | ClickHouse behind the same plane boundary; migration documented and benchmarked |
| 11 | ~~No retention~~ **done** | Per-project policy expires spans, feedback, scores and empty conversations on a schedule; `apply_to_past` bounds it at rule creation |
| 12 | ~~No online evaluation~~ **done** | A rule "on every new trace in project X, run scorer Y" produces scores without a human — implemented (`online_rules`, `online_scores`, `--online` worker mode) |
| 13 | ~~Alerting is threshold-only~~ **done** | Windowed monitors with `ok/firing` state fire once and resolve; webhook and Slack channels; delivery results recorded |
| 14 | No OpenAPI contract | `openapi.json` generated from handlers; a TS client generated from it compiles in CI |
| 15 | No metering | Spans ingested, scores computed, and bytes stored are attributable per workspace per day |
| 16 | ~~No conversation-level online scoring~~ **done** | A `thread`-scoped rule scores a conversation when it closes; alerts carry `subject_type='thread'` |
| 17 | ~~No custom Python metrics~~ **done** | `custom_scorers` + a Docker sandbox that fails closed; the trusted worker never sees user code |
| 18 | ~~No runtime safety layer~~ **done** | Per-project guardrail policies with flag/redact/block, enforced on ingest and exposed as a pre-flight check; audit events store a redacted preview |
| 19 | ~~No optimization loop~~ **done** | A durable `optimize` job measures a baseline and prompt candidates on a pinned snapshot and promotes the winner only when it improved |

---

## 7. What else a "full product" needs that Opik has and we don't

- **Annotation queues** (human review workflow with locking — see `AnnotationQueueItemLockService`). Ours is a thumbs
  button; review is a queue, a routing rule, and an audit trail.
- **Automation rules / online evaluators**: implemented in D14 — filters, sampling, automatic scores — and
  extended in D18 to conversation scope.
- **Alerts + monitors** with state (firing/resolved): implemented in D17 — windowed aggregate conditions,
  one event per episode, webhook and Slack delivery with the result recorded on the event. PagerDuty and
  email channels are not implemented.
- **Optimizations** (prompt/agent optimization loops) — implemented in D20 for prompt variants; no
  agent-graph or few-shot-demo search.
- **Attachments** (images, audio, JSON blobs) on spans — needs object storage.
- **Workspaces + RBAC + API keys** — the actual boundary that makes "multi-tenant" true. We have API keys
  with scopes and OAuth 2.1 for MCP clients, but no orgs, teams or a session login.
- **Retention policies** — implemented in D16.
- **SDKs** (Python/TS tracing) with batching, offline buffering, and idempotent flush. Our `sdk/sample_client.py` is a
  demo, not an SDK.

Copying all of that would take a team a year. The ordering above is the point: ingest correctness and reproducibility
first, breadth last.

---

## 8. Using this for the Go interview

The boilerplate in the brief maps onto the layout in §5 almost one-to-one. The phrases that make an AI (or a
reviewer) produce the right shape:

1. **Structure first:** "Create a Go service using the standard layout: `cmd/server`, `internal/{config,database,
   handler,service,repository,middleware,router}` with explicit constructor injection — no globals."
2. **Config:** "Load config once into a typed struct with defaults and a `Validate() error`; fail at startup on
   invalid values."
3. **Errors:** "Define a typed `HTTPError{Code,Message,Status,Err}`; handlers return it; one error middleware renders
   JSON and logs the cause with the request id."
4. **Data:** "Use `database/sql` with raw SQL and small repository interfaces; context on every method; no ORM."
5. **Middleware:** "Add request id, panic recovery, structured access logging, CORS, and auth, in that order, with a
   table test for the recovery path."
6. **Jobs:** "Dispatch work through a `jobs` table with claim/lease/expiry and a reaper ticker; make retries idempotent
   with a unique key."
7. **Tests:** "Write `httptest` table tests against a temp database; assert status codes, error bodies, and one
   end-to-end path per resource."
8. **Shutdown:** "On SIGINT/SIGTERM: stop accepting, drain with a bounded context, close the DB."

Each of those is one prompt, produces reviewable code in minutes, and is exactly what the reference boilerplate does.

