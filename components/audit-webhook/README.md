# OpenSandbox Audit Webhook

## Overview

A FastAPI service that receives sandbox access audit events from the
[OpenSandbox ingress](../ingress/) and records them to PostgreSQL.

The ingress posts one JSON event per routed request when started with
`--audit-enabled --audit-webhook-url http://<this-service>:8080/events`.

Two tables are maintained (auto-created on startup):

- `sandbox_access_log` — **请求详情表**: one row per request.
- `sandbox_access_latest` — **总表**: one row per sandbox id, holding the
  latest request and the total request count.

A built-in web UI displays the records on two pages (password protected
when `AUDIT_UI_PASSWORD` is set):

- `GET /` - **总表页**: per-sandbox latest requests; clicking a sandbox id
  navigates to its detail page.
- `GET /details?sandbox_id=<id>` - **请求详情页**: request details with
  sandbox filter, pagination, and optional auto-refresh.
- `GET /login` - password login page (session cookie, 7-day validity).

## Quick Start

```bash
pip install -r requirements.txt

export AUDIT_DATABASE_URL=postgresql://postgres:postgres@localhost:5432/opensandbox_audit
export AUDIT_UI_PASSWORD=secret   # optional; unset = no login required
python main.py
```

Endpoints: `POST /events` (audit events; `POST /` is an alias, so a
path-less webhook URL also works), `GET /` (summary page), `GET /details`
(detail page), `GET /login` / `POST /login` / `POST /logout` (UI auth),
`GET /api/sandboxes` and `GET /api/requests` (JSON queries),
`GET /status.ok` (health).

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

| Env | Default | Description |
|---|---|---|
| `AUDIT_DATABASE_URL` | `postgresql://postgres:postgres@localhost:5432/opensandbox_audit` | PostgreSQL connection string |
| `AUDIT_UI_PASSWORD` | (empty) | When set, the web UI and query APIs require password login |
| `AUDIT_HOST` | `0.0.0.0` | HTTP listen host |
| `AUDIT_PORT` | `8080` | HTTP listen port |
| `AUDIT_DB_POOL_MIN` / `AUDIT_DB_POOL_MAX` | `1` / `10` | Connection pool bounds |
| `AUDIT_LOG_LEVEL` | `INFO` | Log level |

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
count. Supports fuzzy search on sandbox id and sorting by request time /
request count (click the column headers to toggle ascending/descending).
Clicking a sandbox id navigates to
`GET /details?sandbox_id=<id>` showing that sandbox's request history.

### `GET /details` (Detail Page)

Every request, filterable by sandbox id (pre-filled from the URL query),
paginated (50 per page), with an optional 10s auto-refresh and a back
link to the summary page.

### Login (`AUDIT_UI_PASSWORD`)

When `AUDIT_UI_PASSWORD` is set, both pages and the query APIs require
login (`GET /login`). Login issues an HMAC-signed session cookie valid
for 7 days; `POST /logout` revokes it. Changing the password invalidates
all existing sessions. Event ingestion is unaffected.

### `GET /api/sandboxes`

List per-sandbox latest requests (summary table).

Query params:
- `search` - fuzzy-match sandbox ids (case-insensitive substring;
  `%`/`_` in the input are matched literally)
- `sort` - `request_time` or `request_count`; prefix with `-` for
  descending (default: `-request_time`, newest first)
- `limit` (1-500, default 50), `offset` (default 0)

```json
{"total": 2, "items": [{"sandbox_id": "my-sandbox", "uri": "/ws", "method": "GET",
  "target": "10.0.0.1:8080", "request_time": "2026-08-20T10:00:00+00:00",
  "request_count": 2}]}
```

### `GET /api/requests`

List access request details, newest first.

Query params: `sandbox_id` (optional filter), `limit` (1-500, default 50),
`offset` (default 0).

```json
{"total": 3, "items": [{"id": 3, "sandbox_id": "other", "uri": "/", "method": "POST",
  "target": "10.0.0.2:8080", "request_time": "2026-08-20T10:00:01+00:00",
  "received_at": "2026-08-20T01:38:55+00:00"}]}
```

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
    uri           TEXT        NOT NULL,
    method        TEXT        NOT NULL,
    target        TEXT        NOT NULL,
    request_time  TIMESTAMPTZ NOT NULL,  -- 最新一次请求时间
    request_count BIGINT      NOT NULL DEFAULT 1,  -- 累计请求数
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

## Docker

```bash
docker build -t opensandbox/audit-webhook:local .
docker run -p 8080:8080 \
  -e AUDIT_DATABASE_URL=postgresql://postgres:postgres@host:5432/opensandbox_audit \
  opensandbox/audit-webhook:local
```

## Development & Tests

```bash
pip install -r requirements.txt pytest httpx
pytest test_main.py
```

Key code:
- `main.py`: FastAPI app, routes, lifespan/pool management.
- `store.py`: schema DDL and transactional writes.
