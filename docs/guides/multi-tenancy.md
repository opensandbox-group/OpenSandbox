---
title: Multi-Tenancy
description: Isolate sandbox workloads across Kubernetes namespaces with per-tenant API keys and automatic namespace routing.
---

# Multi-Tenancy

Multi-tenancy enables a single OpenSandbox Server deployment to serve multiple independent consumers, each isolated in its own Kubernetes namespace. Each tenant gets dedicated API key(s), and all sandbox lifecycle operations (create, list, get, delete) are automatically scoped to the tenant's namespace.

## Requirements

- `opensandbox-server` >= 0.2.2
- Kubernetes runtime (`runtime.type = "kubernetes"` in `server.toml`)
- One Kubernetes namespace per tenant, **pre-created by a cluster admin**
- Default Helm chart RBAC (already includes ClusterRole with cross-namespace access)

::: tip Proxy routes in multi-tenant mode
Unlike single-tenant mode, proxy routes require `OPEN-SANDBOX-API-KEY` when multi-tenancy is enabled. Ensure clients include the header for proxy requests.
:::

::: warning Docker not supported
Multi-tenancy requires Kubernetes namespace isolation. If `runtime.type = "docker"` is configured and tenants are enabled, the server refuses to start with a fatal error.
:::

## How It Works

```
Request with OPEN-SANDBOX-API-KEY header
       │
       ├── Tenant provider configured?
       │       ├── YES → lookup key in tenant provider
       │       │       ├── found  → inject tenant context, route to tenant namespace
       │       │       └── not found → 401 Unauthorized
       │       └── NO  → validate against server.api_key (single-tenant legacy)
```

When multi-tenancy is active:

1. Auth middleware resolves the API key to a `TenantEntry` via the configured provider.
2. The tenant's namespace is injected into a request-scoped `ContextVar`.
3. Sandbox lifecycle operations (create/list/get/delete) use the resolved namespace.
4. List operations only return sandboxes within the authenticated tenant's namespace.
5. Proxy routes (`/sandboxes/{id}/proxy/...`) also require `OPEN-SANDBOX-API-KEY` in multi-tenant mode.

## Configuration

Multi-tenancy is enabled by adding a `[tenants]` section to `server.toml`. Two provider backends are available: **file** (local TOML) and **http** (remote endpoint).

### File Provider (default)

Add to `server.toml`:

```toml
[tenants]
provider = "file"
```

Then create `tenants.toml` at one of these paths (checked in order):

1. `$SANDBOX_TENANTS_CONFIG_PATH` environment variable
2. `~/.opensandbox/tenants.toml`

Example `tenants.toml`:

```toml
[[tenants]]
name = "team-alpha"
namespace = "sandbox-alpha"
api_keys = ["sk-alpha-key-1", "sk-alpha-key-2"]

[[tenants]]
name = "team-beta"
namespace = "sandbox-beta"
api_keys = ["sk-beta-key-1"]
```

Each tenant entry requires:

| Field | Description |
|-------|-------------|
| `name` | Unique human-readable tenant identifier |
| `namespace` | Target Kubernetes namespace (must exist at server startup) |
| `api_keys` | One or more API keys for this tenant (supports key rotation) |

Constraints:
- API keys must be unique across all tenants (duplicate → startup error).
- Every tenant must have at least one key.
- `server.api_key` in `server.toml` must be removed when using `[tenants]`.

### HTTP Provider

For environments where tenants are managed by an external IAM or tenant management system:

```toml
[tenants]
provider = "http"
endpoint = "https://iam.internal/opensandbox/tenant-lookup"
max_stale_seconds = 300.0
timeout_seconds = 5.0
auth_header = "X-Internal-Auth"
auth_token = "bearer-token-here"
```

| Field | Default | Description |
|-------|---------|-------------|
| `provider` | `"file"` | `"file"` or `"http"` |
| `endpoint` | — | Required for HTTP. URL the server GETs to resolve a key. |
| `max_stale_seconds` | `300.0` | Max seconds to serve stale cache when endpoint is unreachable. |
| `timeout_seconds` | `5.0` | HTTP request timeout. |
| `auth_header` | — | Optional header name for provider-level auth. |
| `auth_token` | — | Optional token value for that header. |

**HTTP endpoint contract:**

Request:
```
GET {endpoint}
Header: OPEN-SANDBOX-API-KEY: <api_key>
```

Response (200):
```json
{
  "namespace": "sandbox-alpha",
  "ttl": 60
}
```

Response (401):
```json
{
  "code": "UNAUTHORIZED",
  "message": "Invalid API key"
}
```

The HTTP provider caches results per key using the server-suggested `ttl`. On TTL expiry it re-fetches synchronously. If the endpoint is unreachable, stale entries are served up to `max_stale_seconds`, after which requests fail with 503.

## Namespace Setup

Before onboarding a tenant, the cluster admin must prepare the target namespace.

### 1. Create namespace

```bash
kubectl create namespace sandbox-alpha
```

### 2. ResourceQuota (recommended)

```yaml
apiVersion: v1
kind: ResourceQuota
metadata:
  name: tenant-quota
  namespace: sandbox-alpha
spec:
  hard:
    requests.cpu: "8"
    requests.memory: 16Gi
    limits.cpu: "16"
    limits.memory: 32Gi
    pods: "20"
```

### 3. LimitRange (recommended)

```yaml
apiVersion: v1
kind: LimitRange
metadata:
  name: tenant-limits
  namespace: sandbox-alpha
spec:
  limits:
  - default:
      cpu: "1"
      memory: 2Gi
    defaultRequest:
      cpu: 250m
      memory: 512Mi
    type: Container
```

### 5. Server RBAC

The default Helm chart (`opensandbox-server`) already deploys a ClusterRole + ClusterRoleBinding with cross-namespace access. No additional RBAC changes are needed for multi-tenancy — the server ServiceAccount can already operate in any namespace.

## Hot Reload

### File provider

The file provider watches `tenants.toml` for changes (polls every 2 seconds). Changes take effect without server restart:

- **New key added** → immediately valid on next request.
- **Key removed** → immediately invalid (401).
- **Parse error during reload** → previous config retained, warning logged.
- **File deleted** → all tenant keys invalidated.

The watcher monitors the parent directory to handle Kubernetes ConfigMap atomic symlink swaps.

### HTTP provider

No explicit reload needed — cache entries expire per their `ttl` and are re-fetched on next lookup.

## Key Rotation

Multi-tenancy supports multiple API keys per tenant specifically for zero-downtime key rotation:

1. Add the new key to `tenants.toml` (or issue it via HTTP provider).
2. Wait for hot-reload (file) or TTL expiry (HTTP).
3. Update clients to use the new key.
4. Remove the old key from config.

Both old and new keys are valid simultaneously during the transition window.

## Migration from Single-Tenant

1. Create target namespace(s) for your tenant(s).
2. Write `tenants.toml` with your existing API key as a tenant entry, pointing to the existing namespace:
   ```toml
   [[tenants]]
   name = "default"
   namespace = "your-existing-namespace"
   api_keys = ["your-existing-api-key"]
   ```
3. Add `[tenants]` section to `server.toml`:
   ```toml
   [tenants]
   provider = "file"
   ```
4. Remove `api_key` from `[server]` in `server.toml`.
5. Deploy. The existing key continues working, now as a tenant key.
6. Add more tenants as needed.

**Rollback:** Remove the `[tenants]` section from `server.toml`, restore `server.api_key`, restart.

## Isolation Model

The server itself does not enforce resource quotas or network policies. Isolation is delegated to Kubernetes:

| Dimension | Kubernetes Mechanism | Scope |
|-----------|---------------------|-------|
| Resource quota | `ResourceQuota` | Per-namespace CPU, memory, storage |
| Default limits | `LimitRange` | Per-namespace default container resources |
| Network isolation | Egress sidecar | Per-sandbox outbound policy via egress proxy |
| Sandbox count | `ResourceQuota` (pod count) | Per-namespace pod limit |
| RBAC | `RoleBinding` | Per-namespace API access |

::: info Pool APIs are not tenant-scoped
Pool management routes (`/pools`) currently operate in the server's configured default namespace, not the authenticated tenant's namespace. Pools are shared resources. If you need per-tenant pool isolation, create separate server deployments or wait for future per-tenant pool support.
:::

## Troubleshooting

| Symptom | Cause | Fix |
|---------|-------|-----|
| Server exits at startup: "Multi-tenancy requires Kubernetes namespaces" | `runtime.type = "docker"` with `[tenants]` configured | Use Kubernetes runtime or remove `[tenants]` |
| Server exits: "Remove server.api_key from server.toml" | Both `server.api_key` and `[tenants]` present | Remove `api_key` from `[server]` section |
| Server exits: namespace not found | Tenant namespace doesn't exist | `kubectl create namespace <ns>` before starting server |
| 401 on valid key after config change | Hot-reload hasn't picked up change yet | Wait 2s (file) or TTL seconds (HTTP) |
| 503 TENANT_PROVIDER_UNAVAILABLE | HTTP endpoint unreachable and cache expired past `max_stale_seconds` | Fix endpoint connectivity; increase `max_stale_seconds` |
| Duplicate api_key error at startup | Same key assigned to multiple tenants | Ensure each key is unique across all tenants |
