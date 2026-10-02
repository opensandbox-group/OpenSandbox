# BFF API 契约（对 SPA）

Base path：`/api`。除登录与健康检查外，需 cookie session。

## 认证

### `POST /api/auth/tenant`

租户研发登录。

```json
{ "apiKey": "78cd6c5a..." }
```

- 200：`{ "role": "tenant", "tenant": "geip", "namespace": "geip" }` + Set-Cookie
- 401：Key 无效

### `POST /api/auth/admin`

平台 Admin 登录。

```json
{ "adminToken": "..." }
```

- 200：`{ "role": "admin" }` + Set-Cookie
- 401

### `POST /api/auth/logout`

清除 session。

### `GET /api/auth/me`

当前会话：`{ "role": "tenant"|"admin", "tenant"?, "namespace"? }`

## 租户沙箱（role=tenant）

代理 Lifecycle，响应附加 `runtimeSummary`（BFF 计算）。

| 方法 | 路径 | 对应 Lifecycle |
|------|------|----------------|
| GET | `/api/sandboxes` | `GET /sandboxes` + query |
| GET | `/api/sandboxes/{id}` | `GET /sandboxes/{id}` |
| POST | `/api/sandboxes` | `POST /sandboxes` |
| DELETE | `/api/sandboxes/{id}` | `DELETE /sandboxes/{id}` |
| POST | `/api/sandboxes/{id}/renew-expiration` | 同名 |
| POST | `/api/sandboxes/{id}/pause` | 同名（202） |
| POST | `/api/sandboxes/{id}/resume` | 同名（202） |
| POST | `/api/sandboxes/{id}/snapshots` | 同名（202） |
| GET | `/api/sandboxes/{id}/diagnostics/logs` | 同名；query `scope` 必填（稳定 JSON） |
| GET | `/api/sandboxes/{id}/diagnostics/events` | 同上 |
| GET | `/api/sandboxes/{id}/logs/archive` | node-agent OSS 归档（可选；query `maxBytes`） |
| GET | `/api/sandboxes/{id}/endpoints/{port}` | 同名 |

### 快照（role=tenant）

| 方法 | 路径 | 对应 Lifecycle |
|------|------|----------------|
| GET | `/api/snapshots` | `GET /snapshots` + query |
| GET | `/api/snapshots/{id}` | `GET /snapshots/{id}` |
| DELETE | `/api/snapshots/{id}` | `DELETE /snapshots/{id}` |

### `runtimeSummary`（BFF 附加字段）

```json
{
  "wallClockSeconds": 3842,
  "remainingSeconds": 900,
  "asOf": "2026-09-28T08:00:00Z",
  "basis": "createdAt"
}
```

- 非终态：`wallClockSeconds = now - createdAt`
- 终态（Terminated/Failed/Stopping）：优先 `lastTransitionAt - createdAt`
- 无 `expiresAt` 时不返回 `remainingSeconds`

## Admin（role=admin）

### `GET /api/admin/sandboxes`

Query 与 Lifecycle list 相同（`state`, `page`, `pageSize`, …）。

响应：

```json
{
  "items": [
    {
      "tenant": "geip",
      "namespace": "geip",
      "...": "Sandbox 字段",
      "runtimeSummary": { }
    }
  ],
  "pagination": {
    "page": 1,
    "pageSize": 20,
    "totalItems": 42,
    "tenantErrors": [{ "tenant": "scm", "code": "UPSTREAM_ERROR", "message": "..." }]
  }
}
```

- 对每个 tenant 并行/串行 list；单 tenant 失败记入 `tenantErrors`，不拖垮整体。
- 可选 query：`tenant=geip` 只查一个。

### Admin 沙箱 CRUD（代发租户 Key）

Query **`tenant`** 必填：`GET/DELETE /api/admin/sandboxes/{id}`，`POST .../renew-expiration|pause|resume`，`GET .../endpoints/{port}`，`POST .../snapshots`。

### Admin 快照

`GET/DELETE /api/admin/snapshots[/{id}]?tenant=`

### Admin K8s 只读

`GET /api/admin/k8s/workloads?tenant=&namespace=&sandboxId=&limit=`  
`GET /api/admin/k8s/events?tenant=&sandboxId=&limit=`  
需 `BFF_K8S_PROBE_ENABLED` + ClusterRole（见 `k8s/console-bff-rbac.example.yaml`）。

### Admin 沙箱诊断（代发租户 Key）

| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/api/admin/sandboxes/{id}/diagnostics/logs` | query **`tenant`**（必填）、**`scope`**（必填） |
| GET | `/api/admin/sandboxes/{id}/diagnostics/events` | 同上 |
| GET | `/api/admin/sandboxes/{id}/logs/archive` | query **`tenant`**（必填）；可选 `maxBytes` |

Lifecycle 诊断 JSON：`content` / `contentUrl` / `truncated` / `warnings`。

归档 JSON 额外字段：`archive.objectKey`、`archive.markerStatus`（来自 node-agent finalized marker）。

### `GET /api/admin/stats/runtime`

聚合 KPI（基于当前 Admin list 或缓存）：

```json
{
  "runningCount": 10,
  "totalWallClockSeconds": 120000,
  "avgWallClockSeconds": 12000,
  "maxWallClockSeconds": 86400,
  "expiringWithin30mCount": 2,
  "asOf": "2026-09-28T08:00:00Z"
}
```

### Pool（role=admin）

代理 Lifecycle `/v1/pools`（**不按租户隔离**，BFF 用 `tenants.toml` 中第一个 tenant 的 API Key 代发）。

| 方法 | 路径 | 对应 Lifecycle |
|------|------|----------------|
| GET | `/api/pools` | `GET /pools` |
| POST | `/api/pools` | `POST /pools` |
| GET | `/api/pools/{name}` | `GET /pools/{name}` |
| PUT | `/api/pools/{name}` | `PUT /pools/{name}` |
| DELETE | `/api/pools/{name}` | `DELETE /pools/{name}` |

### `GET /api/admin/platform/summary`

平台概览：`server.health` / `server.version`（代理 Server `/health`、`/version`），以及 ingress / controller / console 的占位组件状态（Phase 2 未主动探测 K8s）。

## 健康

- `GET /health` → `{ "status": "ok" }`
- `GET /api/version` → 代理 Server `/version`（无 auth）

## 错误格式

```json
{ "code": "UNAUTHORIZED", "message": "..." }
```

Upstream Lifecycle 错误透传 `code` / `message`（若存在）。
