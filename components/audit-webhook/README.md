# OpenSandbox Audit Webhook

## Overview

A FastAPI service that receives sandbox access audit events from the
[OpenSandbox ingress](../ingress/) and records them to PostgreSQL.

The ingress posts one JSON event per routed request when started with
`--audit-enabled --audit-webhook-url http://<this-service>:8080/events`.

Two tables are maintained (auto-created on startup):

- `sandbox_access_log` — **请求详情表**: one row per request.
- `sandbox_access_latest` - **总表**: one row per sandbox id, holding the
  latest request and the total request count. BatchSandbox resources
  whose sandbox id has no row yet are inserted with `accessed = FALSE`
  (request fields NULL) and shown in the UI as `未访问`.

A built-in web UI (React + Ant Design; source in `frontend/`, built
output in `static/`) displays the records on two pages (password
protected when `server.ui_password` is set):

- `GET /` - **总表页**: per-sandbox latest requests; fuzzy search on
  sandbox id, a date-range filter on the latest request time, sortable
  columns (request time / request count / accessed status), and
  auto-refresh; sandboxes that exist in the cluster but were never
  accessed are listed with an `未访问` marker; clicking a sandbox id
  navigates to its detail page.
- `GET /details?sandbox_id=<id>` - **请求详情页**: request details with
  sandbox filter, pagination, and optional auto-refresh.
- `GET /login` - password login page (session cookie, 7-day validity).

## Quick Start

```bash
pip install -r requirements.txt

cp audit.toml.example audit.toml   # then edit settings
python main.py
```

Configuration is read from `audit.toml` (next to `main.py`); point
`AUDIT_CONFIG_PATH` at another location to use a different file. A missing
file means "use defaults"; a malformed file fails startup.

Endpoints: `POST /events` (audit events; `POST /` is an alias, so a
path-less webhook URL also works), `GET /` (summary page), `GET /details`
(detail page), `GET /login` / `POST /login` / `POST /logout` (UI auth),
`GET /api/sandboxes` and `GET /api/requests` (JSON queries),
`POST /api/sync-deleted` (mark deleted sandboxes), `GET /status.ok`
(health).

Event ingestion (`POST /events`) is never password protected - the ingress
must deliver events without a session.

Then start the ingress pointing at this service:

```bash
go run main.go \
  --namespace opensandbox \
  --audit-enabled \
  --audit-webhook-url http://audit-webhook:8080/events
```

## Configuration

All settings live in `audit.toml` (see `audit.toml.example`); the file
location can be overridden with the `AUDIT_CONFIG_PATH` env var. Every key
is optional - defaults are shown below.

| Key | Default | Description |
|---|---|---|
| `server.host` | `0.0.0.0` | HTTP listen host |
| `server.port` | `8080` | HTTP listen port |
| `server.ui_password` | (empty) | When set, the web UI and query APIs require password login |
| `database.url` | `postgresql://postgres:postgres@localhost:5432/opensandbox_audit` | PostgreSQL connection string |
| `database.pool_min` / `database.pool_max` | `1` / `10` | Connection pool bounds |
| `kubernetes.kubeconfig` | (empty) | Kubeconfig file path for the deleted-sync (empty = default kubeconfig, falling back to in-cluster credentials) |
| `kubernetes.namespace` | (empty) | Namespace whose `batchsandboxes.sandbox.opensandbox.io` resources mark live sandbox ids (the resource name is the sandbox id; required by the sync) |
| `kubernetes.sync_interval` | `0` | When > 0 (seconds), sync against the cluster periodically in the background (discover unaccessed sandboxes + reconcile the `deleted` flags); `0` = manual sync via `POST /api/sync-deleted` only |
| `log.level` | `INFO` | Log level |

## API

### `POST /events`

Accepts a single event or a batch (JSON array). Payload (matches the
ingress `AuditEvent`):

```json
{
  "sandbox_id": "my-sandbox",
  "uri": "/api/users",
  "method": "GET",
  "target": "10.0.0.1:8080",
  "request_time": "2026-08-20T09:24:12.252+08:00"
}
```

Responses:
- `200 {"accepted": <n>}` — recorded
- `422` — invalid payload (rejected, ingress will log a delivery error)
- `502` — database write failed

### `GET /` (Summary Page)

Web page listing the latest request per sandbox id with the total request
count. Supports fuzzy search on sandbox id, sorting by request time /
request count (click the column headers to toggle ascending/descending),
and a date-range filter on the latest request time (two date pickers -
start/end, both inclusive, interpreted in the browser's local timezone).
Clicking a sandbox id navigates to
`GET /details?sandbox_id=<id>` showing that sandbox's request history.

### `GET /details` (Detail Page)

Every request, filterable by sandbox id (pre-filled from the URL query),
paginated (50 per page), with an optional 10s auto-refresh and a back
link to the summary page.

### Login (`server.ui_password`)

When `server.ui_password` is set, both pages and the query APIs require
login (`GET /login`). Login issues an HMAC-signed session cookie valid
for 7 days; `POST /logout` revokes it. Changing the password invalidates
all existing sessions. Event ingestion is unaffected.

### `GET /api/sandboxes`

List per-sandbox latest requests (summary table).

Query params:
- `search` - fuzzy-match sandbox ids (case-insensitive substring;
  `%`/`_` in the input are matched literally)
- `sort` - `request_time`, `request_count` or `accessed`; prefix with
  `-` for descending (default: `-request_time`, newest first;
  `accessed` ascending puts never-accessed sandboxes first)
- `time_from` / `time_to` - ISO 8601 bounds on the latest request time
  (inclusive; naive values are assumed to be UTC); the summary page's
  date pickers send local start-of-day / end-of-day here
- `limit` (1-500, default 50), `offset` (default 0)

```json
{"total": 2, "items": [{"sandbox_id": "my-sandbox", "uri": "/ws", "method": "GET",
  "target": "10.0.0.1:8080", "request_time": "2026-08-20T10:00:00+00:00",
  "request_count": 2, "accessed": true}]}
```

`accessed` is `false` (and the request fields `null`, `request_count` `0`)
on rows inserted by the cluster discovery for sandboxes that have not
been accessed yet; they sort last under the default
`-request_time` order.

### `GET /api/requests`

List access request details, newest first.

Query params: `sandbox_id` (optional filter), `limit` (1-500, default 50),
`offset` (default 0).

```json
{"total": 3, "items": [{"id": 3, "sandbox_id": "other", "uri": "/", "method": "POST",
  "target": "10.0.0.2:8080", "request_time": "2026-08-20T10:00:01+00:00",
  "received_at": "2026-08-20T01:38:55+00:00"}]}
```

### `POST /api/sync-deleted`

Sync the summary table against the cluster (auth required like the other
query APIs), accessing Kubernetes through the kubeconfig at
`kubernetes.kubeconfig` (empty = default kubeconfig / in-cluster):

1. BatchSandbox resources in the namespace configured via
   `kubernetes.namespace` (the resource name is the sandbox id) whose
   id has no summary row yet are inserted as never-accessed rows
   (`accessed = FALSE`, NULL request fields, `request_count` 0,
   `created_at` = the resource's creationTimestamp) and shown in the
   UI with an `未访问` marker. Existing rows whose `created_at` is
   still NULL get it backfilled. The first audit event for such a
   sandbox flips the row to `accessed = TRUE`.
2. The `deleted` flags are reconciled against the
   `batchsandboxes.sandbox.opensandbox.io` resource names (the resource
   name is the sandbox id): summary rows whose sandbox id is not among
   them are marked `deleted` (hidden from the UI and `/api/sandboxes`);
   previously deleted ids that reappear are restored.

Responses:
- `200 {"namespace": "...", "live": <n>, "discovered": <n>, "backfilled": <n>, "deleted": <n>, "restored": <n>}`
- `400` - `kubernetes.namespace` is not configured
- `502` - the Kubernetes API or the database write failed

Set `kubernetes.sync_interval` (seconds) to run the same sync periodically
in the background instead of calling the endpoint manually.

## Table Schemas

```sql
-- 请求详情表：每次请求一行
CREATE TABLE sandbox_access_log (
    id           BIGSERIAL PRIMARY KEY,
    sandbox_id   TEXT        NOT NULL,
    uri          TEXT        NOT NULL,
    method       TEXT        NOT NULL,
    target       TEXT        NOT NULL,
    request_time TIMESTAMPTZ NOT NULL,   -- 请求到达 ingress 的时间
    received_at  TIMESTAMPTZ NOT NULL DEFAULT now()  -- webhook 收到的时间
);
CREATE INDEX idx_sandbox_access_log_sandbox_time
    ON sandbox_access_log (sandbox_id, request_time DESC);

-- 总表：每个沙箱 id 一行，记录最新一次请求
CREATE TABLE sandbox_access_latest (
    sandbox_id    TEXT        PRIMARY KEY,
    uri           TEXT,                 -- 最新一次请求（未访问沙箱为 NULL）
    method        TEXT,
    target        TEXT,
    request_time  TIMESTAMPTZ,          -- 最新一次请求时间（未访问沙箱为 NULL）
    request_count BIGINT      NOT NULL DEFAULT 1,  -- 累计请求数
    deleted       BOOLEAN     NOT NULL DEFAULT FALSE,  -- 沙箱资源已不存在（前端不展示）
    accessed      BOOLEAN     NOT NULL DEFAULT TRUE,   -- 集群中发现但从未访问
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

Behavior notes:
- Detail insert and summary upsert run in one transaction per request.
- The summary upsert only overwrites when the incoming event is newer or
  equal (`WHERE EXCLUDED.request_time >= sandbox_access_latest.request_time`),
  so out-of-order deliveries never regress the latest-request row.
- Naive timestamps (no timezone) are assumed to be UTC.
- Requests whose URI ends with "ping" (case-insensitive, e.g.
  `/<sandbox-id>/<port>/ping` health checks) are dropped at ingestion and
  never written to the database.
- All stored timestamps are truncated to whole seconds (no sub-second
  precision); the UI displays them as `YYYY-MM-DD HH:MM:SS` in the
  browser's local timezone.
- Summary rows flagged `deleted = TRUE` (sandbox resource gone, see
  `POST /api/sync-deleted`) are excluded from `/api/sandboxes` and the
  summary page; existing tables are migrated with
  `ALTER TABLE ... ADD COLUMN IF NOT EXISTS`.
- Sandbox ids present on BatchSandbox resources but absent from the
  database are inserted as never-accessed rows (`accessed = FALSE`,
  NULL request fields, `created_at` = the resource's
  creationTimestamp); existing rows missing `created_at` get it
  backfilled; a first request flips them to `accessed = TRUE` even
  when it is older than the (NULL) stored request time.

## Docker

```bash
docker build -t opensandbox/audit-webhook:local .
docker run -p 8080:8080 \
  -v $(pwd)/audit.toml:/app/audit.toml:ro \
  opensandbox/audit-webhook:local
```

## Development & Tests

```bash
pip install -r requirements.txt pytest httpx
pytest test_main.py test_k8s.py test_config.py
```

Key code:
- `main.py`: FastAPI app, routes, lifespan/pool management.
- `store.py`: schema DDL and transactional writes.
- `k8s.py`: kubeconfig-based BatchSandbox listing for the cluster sync.
- `config.py`: TOML config loading (`audit.toml`, see `audit.toml.example`).
- `frontend/`: React + Vite + Ant Design SPA sources (three page entries).

## Frontend

The UI is a React app in `frontend/` (Vite, TypeScript, Ant Design). It
is a multi-page build: `index.html` (summary), `details.html`, and
`login.html` map 1:1 to the FastAPI routes that serve them, so no
client-side router is needed. `npm run build` writes the output into
`../static` (which FastAPI serves at `/static`).

```bash
cd frontend
npm install
npm run dev     # dev server at :5173, proxying /api and /login to :8080
npm run build   # regenerate ../static (commit the result with your change)
```

After changing anything under `frontend/`, re-run `npm run build` and
commit the regenerated `static/` output - the Docker image builds it
itself, but `pytest test_main.py` serves the committed `static/` files.
