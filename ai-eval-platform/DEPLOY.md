# Deploying EvalForge

Three commands, one machine, no cloud account:

```bash
cp .env.example .env      # add OPENROUTER_API_KEY (optional, see below)
docker compose up --build -d
docker compose run --rm seed
open http://localhost:8080
```

That starts a PostgreSQL control plane, the Go API with its Python evaluation worker, and the web
console behind nginx. Data lives in named volumes, so `docker compose down` keeps it and
`docker compose down -v` wipes it.

---

## What gets started

| Service | Image | What it is | Port |
|---|---|---|---|
| `postgres` | `postgres:16-alpine` | control plane: projects, datasets, prompts, rules, policies, jobs, credentials | internal |
| `server` | built from `deploy/Dockerfile.server` | Go API **and** the Python worker it spawns, plus the docker CLI for the sandbox | `8000` |
| `web` | built from `deploy/Dockerfile.web` | the React console, served by nginx with the API proxied | `8080` |
| `seed` | same as `server` | one-shot: loads the demo project (profile `tools`) | – |
| `create-key` | same as `server` | one-shot: prints the first API key (profile `tools`) | – |
| `pull-sandbox` | `docker:cli` | one-shot: pre-pulls the metric interpreter (profile `tools`) | – |

The API and the worker are one container on purpose: the server spawns `worker.py` as a subprocess, so
splitting them would mean inventing an RPC between two halves of the same program.

```
browser ──► nginx :8080 ──┬── /            the SPA
                          ├── /api/        the API
                          ├── /v1/traces   OTLP ingest (point an exporter here)
                          ├── /otel/       OTLP ingest (alternate spelling)
                          ├── /mcp         MCP over Streamable HTTP
                          └── /oauth/, /.well-known/   OAuth 2.1 discovery
                                   │
                                   ▼
                             server :8000 ──► postgres
                                   │      └─► /data/telemetry.duckdb
                                   ├── spawns worker.py (scoring, optimizer)
                                   └── docker run --network none … (custom metrics)
```

Only `web` needs a public URL in a real deployment. `PUBLIC_BASE_URL` must be the URL a **browser**
reaches, because it is published in OAuth discovery documents.

---

## Prerequisites

- Docker Desktop (or Docker Engine + the Compose v2 plugin).
- ~4 GB of disk for images (Go toolchain + Postgres + Python + nginx).
- About 3 minutes for the first build; ~10 seconds afterwards.
- `OPENROUTER_API_KEY` in `.env` is **optional**. Without it, LLM judges fall back to labelled
  deterministic heuristics, and every optimizer candidate scores the same — everything else, including
  guardrails, monitors, retention, ingest and the sandbox, works identically.

---

## First run, step by step

```bash
# 1. configure (only if you have a provider key)
cp .env.example .env

# 2. build and start
docker compose up --build -d

# 3. wait for health (the web service waits for the API itself)
docker compose ps

# 4. demo data: projects, traces, conversations, datasets, prompts, rules
docker compose run --rm seed

# 5. pre-pull the metric interpreter so the first custom metric is not a cold pull
docker compose run --rm pull-sandbox

# 6. open it
#    http://localhost:8080
```

On Windows PowerShell the same commands work unchanged.

### Turning on authentication

```bash
# 1. switch the mode
#    in .env:  AUTH_MODE=required
docker compose up -d server

# 2. mint a key; it is printed once, to stdout
docker compose run --rm create-key
#    efk_…
```

`create-key` talks only to PostgreSQL, so it works **while the server is running** — it does not open the
telemetry file, which DuckDB allows only one writer to hold. Adding a second key later is the same
command.

With `AUTH_MODE=required`, every `/api/v1` route needs `Authorization: Bearer efk_…`, and the UI has no
login screen — it is a token product, and the console reads the key from the browser's local storage.
For a hackathon demo, leave `AUTH_MODE=off`, which is the default.

Verified against this stack: no key → `401`, a minted key → `200`.

---

## Demo script (the order that tells the story)

The product is one loop, and every feature is stage of it. Six minutes:

1. **Observe** — Dashboard, then *Logs & Traces*. Open a trace: span tree, tokens, cost, status.
2. **Conversations** — *Conversations*, open one, hit **Evaluate**. Five thread scorers run through the
   queue; scores appear per scorer. This is the multi-turn signal a single trace cannot give you.
3. **Turn a failure into a test** — in a trace, add feedback and **promote to dataset**.
4. **Evaluate** — *Datasets* → run an experiment with a pinned prompt. *Experiments* → compare against
   the baseline.
5. **Online evaluation** — *Online evaluation*: create a rule (scope *one trace*), then ingest a trace
   and watch it score itself with no human. Create a second rule with scope *whole conversation*; close
   a conversation and watch the conversation get scored.
6. **Monitoring** — *Monitors & policies*: create `error_rate > 0.5` over 15 minutes, click
   **Evaluate now**. It fires once and resolves when it recovers. Add a webhook URL to see the payload.
7. **Retention** — same page: add a 30-day policy, **Apply now**; the numbers it reports are the rows it
   expired.
8. **Guardrails** — same page: add a policy (`redact`, check `PII`), then use **Try a check** with
   `email me at jane@example.com`. It comes back masked. Switch the policy to `block` and POST a span
   with a real-looking card number to `/api/v1/projects/{id}/spans:ingest` — `422`, nothing stored.
9. **Custom metric in a sandbox** — register a metric, press **Test**. It runs inside a container with
   no network, not on the host. Then point an online rule at it.
10. **Optimize** — *Optimize*: pick the dataset, pick a baseline prompt, start a run. It measures the
    baseline and five prompt candidates on the same snapshot and promotes the winner **only if it
    improved**. With no provider key the candidates tie, nothing is promoted, and the UI says so.

Steps 1–4 work with zero API key. Step 10 is the one that most benefits from one.

---

## Verifying the deployment

```bash
# API is up and its two stores are reachable
curl -s localhost:8000/health

# the console can reach the API through the proxy
curl -s localhost:8080/health

# ingest a trace with the native endpoint
curl -s -X POST localhost:8000/api/v1/projects \
  -H 'content-type: application/json' -d '{"name":"smoke"}'
curl -s -X POST localhost:8000/api/v1/projects/<id>/spans:ingest \
  -H 'content-type: application/json' \
  -d '{"events":[{"id":"t1","name":"agent","type":"llm","model":"gpt-4o-mini","input":"q","output":"a"}]}'

# OTLP: point the Python SDK at http://localhost:8000  (it appends /v1/traces)
```

The repository's own suites can be pointed at the running stack:

```bash
python scripts/smoke_api.py --base http://localhost:8000     # 69 checks
python scripts/auth_test.py                                   # needs AUTH_MODE=required
python scripts/mcp_test.py
```

---

## Operating it

```bash
docker compose ps                     # health of each service
docker compose logs -f server         # JSON logs, one line per request
docker compose restart server         # queued jobs are requeued on boot
docker compose down                   # stop, keep data
docker compose down -v                # stop and wipe everything
docker compose up -d --build server   # rebuild after a code change
```

Background work you should expect to see in the logs: `closed idle threads`, `monitor state changed`,
`retention applied`, `job done`. The durable queue means a restart mid-evaluation requeues it — that is
the point of `jobs` having a lease.

### Backing up

Two things hold state, and they are deliberately separate:

```bash
docker compose exec postgres pg_dump -U evalforge evalforge > control.sql   # metadata
docker run --rm -v evalforge_telemetry-data:/data -v "$PWD:/out" alpine \
  tar czf /out/telemetry.tgz -C /data .                                     # telemetry
```

Different engines, different backup and retention policies for each — that is why the planes are split.

---

## Verified against this stack

Everything below was run against the Compose deployment, not just locally:

| Check | Result |
|---|---|
| `docker compose build` (server image: cgo + go-duckdb, and the web image) | both built |
| `docker compose up -d` | postgres, server and web all `healthy` |
| `docker compose run --rm seed` | 36 traces, 8 conversations, 2 datasets, 2 prompts |
| `GET :8080/health` through the nginx proxy | `{"ok":true,…}` |
| SPA and its JS bundle through :8080 | `200` |
| Custom metric `tone_check` via `docker run` inside the stack | `{"ok":true,"score":0.9}` |
| Metric that calls `http://example.com` | `score: 0.25` — the network is genuinely off |
| Guardrail `redact` check through :8080 | `mail me at [EMAIL_ADDRESS], card [CREDIT_CARD]` |
| Optimization run (provider key set) | `completed`, baseline and candidate measured, tie → not promoted |
| `create-key` while the server is running | printed a key; no DuckDB lock error |
| `AUTH_MODE=required`: no key vs minted key | `401` vs `200` |
| `POST :8080/mcp` initialize | JSON-RPC result with server capabilities |

---

## Deploying to Render

`render.yaml` is a blueprint: a managed Postgres, the API (with a 5 GB disk for telemetry), and the
console. Render builds both Dockerfiles from this repository, so there is nothing to pre-build.

### Checklist before you push

1. **Push the repository.** This project lives in the `ai-eval-platform/` folder of
   `archief2910/EvalForger`, so `render.yaml` sets `dockerContext` to `ai-eval-platform` and both
   `dockerfilePath` values to `ai-eval-platform/deploy/Dockerfile.*` (Render resolves those from the
   repository root).
2. **Commit the deployment files.** `docker-compose.yml`, `deploy/`, `render.yaml`, `.env.example` and
   the regenerated `frontend/package-lock.json` (`npm ci` in the web image needs a lockfile that matches
   `package.json`).
3. **Do not commit `.env`.** It is git-ignored, and the blueprint injects secrets from the dashboard.

### Deploy

1. Render dashboard → **New → Blueprint** → select `archief2910/EvalForger`. In the **Blueprint Path**
   field enter `ai-eval-platform/render.yaml`. Render shows three resources: `evalforge-db`,
   `evalforge-api`, `evalforge-web`.
2. It asks for the two `sync: false` values on `evalforge-api`:
   - `OPENROUTER_API_KEY` — optional; without it judges use labelled heuristics and the optimizer only
     ever finds ties.
   - `PUBLIC_BASE_URL` — leave blank for now, then set it to `https://<your-web-service>.onrender.com`
     after the first deploy and redeploy the API. It is published in OAuth discovery.
3. **Apply.** The first build is the slow one (~4–6 minutes, it compiles cgo and DuckDB).
4. When both services are live, confirm:
   ```bash
   curl https://<api>.onrender.com/health
   curl https://<web>.onrender.com/health     # proxied through nginx
   open https://<web>.onrender.com
   ```
5. Seed the demo data once, from anywhere with Python:
   ```bash
   python worker-py/seed.py --base https://<api>.onrender.com
   ```

### Zero-cost demo (no card)

Render's **Free** compute plan does not require a payment method. Use
`render-free.yaml` instead of `render.yaml` when you want a demo without a
card:

1. Render dashboard → **New → Blueprint** → `archief2910/EvalForger`.
2. **Blueprint Path:** `ai-eval-platform/render-free.yaml`.
3. Enter `OPENROUTER_API_KEY`; leave `PUBLIC_BASE_URL` blank for now.
4. Apply, then set `PUBLIC_BASE_URL` to the `evalforge-free-web` URL and
   redeploy the API.

The Free plan is a demo tier, not production:

- Services spin down after 15 idle minutes; the next request takes about a
  minute to wake them.
- There is no disk, so the DuckDB telemetry file lives in `/tmp` and is lost
  on every restart, redeploy, or spin-down. Re-run
  `python worker-py/seed.py --base https://<api>.onrender.com` after a cold
  start.
- The free Postgres database expires 30 days after creation.
- The workspace gets 750 free instance hours per month, shared by both
  services. Idle spin-down keeps a demo well under this.
- No Shell access and no persistent disks. The custom-metric sandbox still
  returns 503 because Render has no Docker daemon.

### What is different in the cloud

| Thing | On Render |
|---|---|
| Postgres | managed, injected as `CONTROL_DB_URL`; migrations run at boot |
| Telemetry | DuckDB file on the service disk at `/data`; **the disk is what makes restarts safe** |
| Port | the API binds `$PORT` (Render assigns it), the console's nginx listens on `$PORT` too |
| Console → API | nginx proxies to `API_HOST` (the API's public host) with `API_SCHEME=https` |
| Worker | same container as the API; no second service needed |
| **Custom metrics** | **unavailable** — no Docker daemon, so the sandbox reports `sandbox_available: false` and running a metric returns `503`. Everything else works. |

The sandbox is the one feature that does not survive the move to a PaaS. That is not a bug in the
deployment: isolation here means "start a container", and a container platform does not give you a
container runtime. If you need custom metrics in production, the honest options are a small VM with
Docker, or a dedicated sandbox service — not removing the boundary.

### Other hosts

The same two images work anywhere that runs containers:

- **Fly.io / Cloud Run / ECS:** same split (API with a volume for `/data`, console with `API_HOST`
  pointing at the API). Set `PORT` if the platform requires it; the API reads it automatically.
- **A single VM with Docker:** that is exactly `docker compose up -d`, and it is the only one of these
  where the sandbox works, because the VM has a daemon.
- **Anything with a managed Postgres:** the control plane is portable; only the telemetry file is
  local to the API instance.

---

## Security notes

Read this before exposing the stack to anything but your laptop.

- **`AUTH_MODE=off` means open.** Anyone who reaches the port can read and write. It is the right
  default for a demo and the wrong default for a network you do not control.
- **The docker socket is the most privileged thing in this stack.** `server` mounts
  `/var/run/docker.sock` so a custom metric can run in its own container (no network, read-only root,
  non-root, caps dropped, memory/PID/CPU caps, timeout). Anything that can talk to that socket can start
  a privileged container, so the socket is a real capability, not a detail. If you do not need custom
  metrics, delete that line and the sandbox reports `sandbox_available: false` — every other feature
  keeps working. If you do need them in production, give the sandbox its own host or a rootless daemon
  instead of the host socket.
- **Guardrails protect the telemetry path.** They run before spans are stored and through
  `POST /guardrails/check` for a pre-flight decision. They are not a network filter: they do not sit
  between your application and the model provider unless your application calls them.
- **The guardrail audit log stores a redacted preview**, never the matched value — otherwise the log
  would be the leak.
- **Postgres credentials** default to `evalforge/evalforge`. Change `POSTGRES_PASSWORD` in `.env` for
  anything shared.
- **`.env` is git-ignored.** Keep the provider key out of the image; compose passes it as an environment
  variable, and the image never bakes it in.

---

## Troubleshooting

| Symptom | Cause | Fix |
|---|---|---|
| `docker compose up` fails at `FROM golang:1.27-bookworm` | your registry does not have that tag | set `GO_IMAGE=golang:1.26-bookworm` in `.env`; the builder downloads the toolchain go.mod asks for |
| `server` restarts in a loop, logs `database is unreachable` | Postgres not ready yet | it retries; check `docker compose logs postgres`. If it persists, the volume may be from an older Postgres major version |
| `web` shows a blank page, `/health` on 8080 works | stale SPA build | `docker compose up -d --build web` |
| Custom metric test returns `503 sandbox is unavailable` | daemon not reachable from the container | check `docker compose exec server docker version`; confirm the socket mount and that Docker Desktop is running |
| Custom metric fails with `no such file or directory: /sandbox/payload.json` | the staging path is not shared with the daemon | it must be the same absolute path on both sides; compose already does this via `SANDBOX_WORK_DIR` and the matching bind mount |
| First custom metric takes ~20 s | pulling `python:3.12-slim` | `docker compose run --rm pull-sandbox` |
| Optimization run finishes with `promoted: false` every time | no provider key, so all candidates tie | set `OPENROUTER_API_KEY`, or expect this — the UI reports a tie rather than inventing a win |
| Ports already in use | something else has 8000/8080 | set `API_PORT` / `WEB_PORT` in `.env` |
| Windows: `docker compose` is not a command | old CLI | Docker Desktop ships Compose v2; update it, or use `docker-compose` |

---

## Deploying somewhere real

The Compose stack is a complete single-host deployment. Two things change when it is not your laptop:

1. **Put TLS in front.** Terminate HTTPS at the `web` service (or a load balancer) and set
   `PUBLIC_BASE_URL=https://your-host`, because that value is published in OAuth discovery.
2. **Separate the concerns the single host is hiding.** The two that matter:
   - **Telemetry storage.** DuckDB is one file, one writer. A second API replica needs its own file or a
     different telemetry backend behind the same plane boundary — that is what the plane split buys you,
     and the SQL shape does not change when it does.
   - **The queue.** `jobs` is claimed and reaped inside one process. Running two API replicas against one
     database works but wants the lease clock tuned; a second failure domain (Redis, a real broker) is
     the next step if you need horizontal scale.
