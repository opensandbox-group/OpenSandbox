# OpenSandbox Server configuration reference

All TOML configuration options for the OpenSandbox lifecycle server (`opensandbox-server`). Schema: [`opensandbox_server/config.py`](opensandbox_server/config.py) (`AppConfig`).

- **Default path:** `~/.sandbox.toml`; override with `SANDBOX_CONFIG_PATH` or `--config`.

Example files: [`example.config.toml`](opensandbox_server/examples/example.config.toml) / [`example.config.zh.toml`](opensandbox_server/examples/example.config.zh.toml) (Docker), [`example.config.k8s.toml`](opensandbox_server/examples/example.config.k8s.toml) / [`example.config.k8s.zh.toml`](opensandbox_server/examples/example.config.k8s.zh.toml) (Kubernetes).

---

## Table of contents

1. [Top-level sections](#top-level-sections)
2. [`[server]`](#server--lifecycle-api)
3. [`[proxy]`](#proxy)
4. [`[log]`](#log)
5. [`[runtime]`](#runtime--required)
6. [`[docker]`](#docker--only-when-runtime--docker)
7. [`[kubernetes]`](#kubernetes--only-when-runtime--kubernetes)
8. [`[agent_sandbox]`](#agent_sandbox--only-with-kubernetes--agent-sandbox)
9. [`[ingress]`](#ingress)
10. [`[egress]`](#egress)
11. [`[storage]`](#storage)
12. [`[store]`](#store)
13. [`[secure_runtime]`](#secure_runtime)
14. [`[renew_intent]`](#renew_intent)
15. [`[automation]`](#automation)
16. [`[otel]`](#otel)
17. [Environment variables (outside TOML)](#environment-variables-outside-toml)
18. [Cross-field validation rules](#cross-field-validation-rules)

---

## Top-level sections

| Section | Required | When |
|---------|----------|------|
| `[server]` | No | Always |
| `[proxy]` | No | Always; server-side reverse-proxy target |
| `[log]` | No | Always |
| `[runtime]` | **Yes** | Always |
| `[docker]` | No | `runtime.type = "docker"` |
| `[kubernetes]` | No | `runtime.type = "kubernetes"` (defaults applied if missing) |
| `[agent_sandbox]` | No | Only with `kubernetes.workload_provider = "agent-sandbox"` |
| `[ingress]` | No | Ingress exposure mode |
| `[egress]` | No | Needed when clients create sandboxes with `networkPolicy` |
| `[storage]` | No | Host bind allowlist / OSSFS mount root |
| `[store]` | No | Server-managed persistence backend |
| `[secure_runtime]` | No | gVisor / Kata / Firecracker |
| `[renew_intent]` | No | Auto-renew on access |
| `[automation]` | No | Lifecycle automation umbrella (idle auto-pause today) |
| `[otel]` | No | OTLP export for Server HTTP and ingested SDK metrics |

---

## `[server]` — Lifecycle API

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `host` | string | `"0.0.0.0"` | HTTP API bind address. |
| `port` | integer | `8080` | Listen port (1–65535). |
| `api_key` | string \| omitted | `null` | If set, requests must send header `OPEN-SANDBOX-API-KEY` (public routes like `/health`, `/docs` excluded; in single-tenant mode the sandbox proxy routes are excluded too unless `[proxy] require_api_key` is set). If unset, API key checks are skipped, and startup requires explicit risk acknowledgment: interactive TTY `YES` or `OPENSANDBOX_INSECURE_SERVER=YES`. |
| `eip` | string \| omitted | `null` | Public IP/hostname used as the host part of returned sandbox endpoint URLs (mainly Docker runtime). |
| `max_sandbox_timeout_seconds` | integer \| omitted | `null` | Cap on the `timeout` field of **create** requests (≥ 60). Does not apply to renew-expiration or total lifetime. Omit to disable. |
| `timeout_keep_alive` | integer | `30` | Idle keep-alive timeout (s) passed to uvicorn. |
| `timeout_graceful_shutdown` | integer | `5` | Seconds uvicorn waits for in-flight requests before forcing shutdown. |
| `limit_concurrency` | integer | `1024` | Max concurrent connections before 503 backpressure. `0` disables. |
| `backlog` | integer | `2048` | Socket listen backlog passed to uvicorn. |
| `thread_pool_size` | integer | `200` | Anyio threadpool size for sync route handlers. |
| `loop` | `"auto"` \| `"uvloop"` \| `"asyncio"` | `"auto"` | Event loop; `auto` prefers uvloop. |
| `http` | `"auto"` \| `"httptools"` \| `"h11"` | `"auto"` | HTTP parser; `auto` prefers httptools. |

---

## `[proxy]`

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `resolve_internal` | boolean | `true` | `true`: reverse-proxy targets the sandbox internal IP (Docker bridge / provider internal endpoint). `false`: targets the server-local host-mapped port — required when the server cannot route to sandbox bridge IPs (e.g. server container on a Compose network while sandboxes run on the default bridge; macOS launchd/systemd session). |
| `require_api_key` | boolean | `false` | `true`: the sandbox proxy routes (`/sandboxes/{id}/proxy/{port}/...`, HTTP and WebSocket) also require `OPEN-SANDBOX-API-KEY` in single-tenant mode. `false` keeps them open so a browser can reach sandbox ports directly; any process that can reach the server can then use them without a key. Enable it when only a trusted gateway talks to the server. Requires `server.api_key` (rejected at load otherwise). Multi-tenant mode always authenticates proxy routes. |

---

## `[log]`

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `level` | string | `"INFO"` | Server log level (e.g. `"DEBUG"`, `"WARNING"`). |
| `file_enabled` | boolean | `false` | Write logs to rotating files instead of stdout. |
| `file_path` | string \| omitted | `null` | Main log file; default `~/logs/opensandbox/server.log`. |
| `access_file_path` | string \| omitted | `null` | Access log file; default `~/logs/opensandbox/access.log`. |
| `file_max_bytes` | integer | `104857600` | Max bytes per file before rotation. |
| `file_backup_count` | integer | `5` | Rotated files to retain. |

---

## `[runtime]` — **required**

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `type` | string | — | **`docker`** or **`kubernetes`**. |
| `execd_image` | string | — | OCI image containing the **execd** binary (in-sandbox bootstrap). |
| `execd_run_as_init` | boolean | `false` | Run execd as sandbox init (OSEP-0018): execd becomes PID 1 (child reaping, container lifecycle, hardening floor). |

---

## `[docker]` — only when `runtime.type = "docker"`

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `network_mode` | string | `"host"` | Sandbox network: **`host`**, **`bridge`**, **`none`**, **`pasta`**, **`slirp4netns`**, or a custom network name. Egress sidecar + `networkPolicy` require **`bridge`**. |
| `api_timeout` | integer \| omitted | `null` | Docker API timeout (s); default 180 when unset. |
| `host_ip` | string \| omitted | `null` | Host/IP used when rewriting bridge-mode endpoint URLs (e.g. `host.docker.internal`). |
| `drop_capabilities` | list of strings | See `config.py` | Linux capabilities dropped from sandbox containers. |
| `apparmor_profile` | string \| omitted | `null` | AppArmor profile name; empty uses Docker default. Also applied to the execd cache container. |
| `no_new_privileges` | boolean | `true` | Block privilege escalation. |
| `seccomp_profile` | string \| omitted | `null` | Seccomp profile name or absolute path; empty uses Docker default. Also applied to the execd cache container (e.g. set `unconfined` on hosts whose default profile blocks execd). |
| `pids_limit` | integer \| null | `4096` | Max PIDs per sandbox container; `null` disables. |
| `sandbox_env` | table | `{}` | Env vars injected into every sandbox; create-request keys override same-named ones. |
| `sandbox_binds` | string[] | `[]` | Bind mounts for every sandbox (`host:container[:mode]`); prepended to request-derived binds. |
| `port_range_min` / `port_range_max` | integer | `40000` / `60000` | Host port range for bridge-mode allocation. Range must span ≥ 100 ports; each sandbox needs 2–3 ports (3 with egress). |
| `publish_host` | string | `"0.0.0.0"` | Address Docker publishes sandbox ports on. Use `127.0.0.1` (server on host) or the bridge gateway IP (server in container) to keep ports off public interfaces. Must be an IPv4 address. |

---

## `[kubernetes]` — only when `runtime.type = "kubernetes"`

Missing table → defaults from `KubernetesRuntimeConfig`.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `kubeconfig_path` | string \| omitted | `null` | Kubeconfig path; often unset in-cluster. |
| `insecure_skip_tls_verify` | boolean | `false` | Skip apiserver TLS verification (temporary workaround only). |
| `namespace` | string \| omitted | `null` | Namespace for sandbox workloads. |
| `workload_provider` | string \| omitted | `null` | **`batchsandbox`** (default) or **`agent-sandbox`**. |
| `batchsandbox_template_file` | string \| omitted | `null` | BatchSandbox CR YAML template path (batchsandbox only). |
| `image_pull_policy` | string \| omitted | `"IfNotPresent"` | BatchSandbox main container pull policy (`Always`/`IfNotPresent`/`Never`). |
| `sandbox_create_timeout_seconds` | integer | `60` | Max wait for a new sandbox to become ready. |
| `pool_acquisition_timeout_seconds` | integer | `30` | Max wait blocked by Pool capacity (does not extend create timeout). |
| `sandbox_create_poll_interval_seconds` | float | `1.0` | Fallback status-check interval without informer events. |
| `informer_resync_seconds` | integer | `300` | **[Beta]** Informer full resync period. |
| `informer_watch_timeout_seconds` | integer | `60` | **[Beta]** Watch stream restart interval. |
| `read_qps` / `read_burst` | float / integer | `0` / `0` | K8s get/list rate limit; `0` = unlimited (burst `0` uses QPS). |
| `write_qps` / `write_burst` | float / integer | `0` / `0` | K8s write rate limit; same semantics. |
| `execd_init_resources` | table \| omitted | `null` | Resource requests/limits for the execd init container: `limits` / `requests` maps, e.g. `{ cpu = "100m", memory = "128Mi" }`. |

### BatchSandbox vs agent-sandbox

Workloads are created by a **workload provider**; there is no `[batchsandbox]` table. BatchSandbox is configured entirely under `[kubernetes]`.

| | BatchSandbox (default) | agent-sandbox |
|--|------------------------|---------------|
| Provider value | `"batchsandbox"` or omit | `"agent-sandbox"` |
| Template file | `kubernetes.batchsandbox_template_file` | `[agent_sandbox].template_file` |
| Image pull policy | `kubernetes.image_pull_policy` | Not used |
| Extra TOML table | None | `[agent_sandbox]` required |

All other `[kubernetes]` keys apply to both providers.

### fsb (fast-sandbox) settings under `[kubernetes]`

The kubernetes runtime also serves fsb (`fsb-`) sandboxes side by side; these fields are always available.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `fastpath_endpoint` | string | `"fast-sandbox-fastpath.opensandbox.svc:9090"` | Fast-Path Server gRPC endpoint. |
| `fastpath_timeout_seconds` | number | `30.0` | Per-RPC gRPC deadline. |
| `fastpath_wait_ready_seconds` | number | `30.0` | Bounded readiness wait for DataPlaneReady after Create. |
| `fastpath_resource_pool` | string | `"default-pool"` | SandboxPool when `extensions.poolRef` is unset. |
| `template_s3_publish_secret` | string | `"sandbox-oss-credentials"` | Object-store credentials secret for server-created SandboxTemplates (`format: native`). |
| `template_oci_publish_secret` | string \| omitted | `null` | Name of a `kubernetes.io/dockerconfigjson` Secret in each template's namespace, used as `output.publishSecretRef` for `format: overlaybd` SandboxTemplate builds (fast-sandbox OCI publication). Required when creating overlaybd templates; native templates keep using `template_s3_publish_secret`. |
| `template_mitm_ca_secret` | string \| omitted | `null` | Secret holding the pinned cluster MITM CA certificate (OSEP-0022). When set, server-created SandboxTemplates bake `spec.egressMitmCertificate` from it (certificate entry only, never the private key); unset keeps templates CA-free. The Secret must exist in each template's namespace (ops-runbook distribution); a missed copy fails the build observably. |
| `template_mitm_ca_secret_key` | string | `"ca.crt"` | Secret entry holding the CA certificate when `template_mitm_ca_secret` is set. |

---

## `[agent_sandbox]` — only with `kubernetes.workload_provider = "agent-sandbox"`

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `template_file` | string \| omitted | `null` | Sandbox CR YAML template path. |
| `shutdown_policy` | string | `"Delete"` | **`Delete`** or **`Retain`** when the sandbox expires. |
| `ingress_enabled` | boolean | `true` | Whether ingress routing to agent-sandbox pods is expected. |

---

## `[ingress]`

Ingress exposure for sandbox endpoints. With `runtime.type = "docker"`, only `mode = "direct"` is allowed. `secure_access` is currently Kubernetes-only and requires `mode = "gateway"`.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `mode` | string | `"direct"` | **`direct`** — clients reach sandboxes without an L7 gateway. **`gateway`** — use `[ingress.gateway]`. |
| `gateway.address` | string | — | Gateway host, **no URL scheme**. Wildcard domain (e.g. `*.example.com`) required for `route.mode = "wildcard"`; forbidden otherwise. |
| `gateway.route.mode` | string | — | **`wildcard`** (host-based) / **`uri`** (path-prefix) / **`header`** routing. |
| `secure_access` | table \| omitted | `null` | OSEP-0011 signing config: signed route tokens + static SecureAccessTokens. Also injectable via `OPENSANDBOX_SECURE_ACCESS_KEYS` / `OPENSANDBOX_SECURE_ACCESS_ACTIVE_KEY`. |

---

## `[egress]`

Egress sidecar image and enforcement mode. The sidecar is only attached to sandboxes created **with** a `networkPolicy`.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `image` | string \| omitted | `null` | Egress sidecar OCI image. **Required** when clients send `networkPolicy`. |
| `mode` | string | `"dns"` | **`dns`** — DNS-proxy enforcement (CIDR/IP rules not enforced); **`dns+nft`** — adds nftables for CIDR/IP enforcement where available. |
| `disable_ipv6` | bool | `true` | IPv6 egress is incomplete (esp. Kubernetes); set `false` only deliberately. |
| `otlp_endpoint` | string \| omitted | `null` | OTLP/**HTTP** endpoint for sidecar metrics (both runtimes). Infrastructure config — not settable per request. |
| `readiness_timeout_seconds` | float | `30.0` | Docker only: max wait for sidecar health endpoint (> 0). |
| `requests` / `limits` | map \| omitted | `null` | Kubernetes only: sidecar resources. Omit independently; invalid quantities fail config load. |
| `upstream_proxy` | table \| omitted | `null` | Chain mitmproxy egress through an upstream HTTP(S) CONNECT proxy. Requires `mode = "dns+nft"`. See below. |

```toml
[egress]
image = "opensandbox/egress:v1.1.7"
mode = "dns+nft"

[egress.upstream_proxy]
url = "http://proxy.example.com:3128"
# authorization = "Basic <base64>"
# ca_cert_path = "/etc/ssl/private-ca/upstream-proxy-ca.pem"   # Docker only
# ca_secret_name = "corp-proxy-ca"                            # Kubernetes only
```

**`otlp_endpoint` notes:** must be `http(s)://` (OTLP over HTTP/protobuf only; gRPC silently fails); use a fully qualified name or IP — the sidecar allow rule matches the configured host exactly; sidecar exports **delta** temporality (Prometheus/GMP needs `deltatocumulative`).

**`upstream_proxy` notes:**

- Requires `mode = "dns+nft"`; applies only to sandboxes created with `networkPolicy`.
- Each such create must enable transparent MITM (`credentialProxy.enabled=true` or env `OPENSANDBOX_EGRESS_MITMPROXY_TRANSPARENT=true`), else `400`; `OPENSANDBOX_EGRESS_MITMPROXY_SSL_INSECURE` in request env is rejected.
- Admin config only: the same env var names in request `env` are rejected. Fast sandboxes reject `networkPolicy` on create/patch while configured (pool mode already rejects it).
- `url`: `http(s)://host[:port]` only; credentials, query, fragment, non-root paths, control/outside-charset characters rejected. Put credentials in `authorization` (injected as literal env var — visible in `docker inspect`/Pod spec; protect the config file).
- CA fields are runtime-specific and validated at load (`ca_cert_path`: absolute path on the Docker daemon host; `ca_secret_name`: Secret key `ca.crt`, mounted read-only into the sidecar only). They **augment** the system trust store and apply to all mitmproxy upstream TLS verification. Missing/invalid CA fails closed — traffic is never chained with reduced verification.
- Config changes affect newly created sandboxes only.
- Data-plane details: [`components/egress/docs/mitmproxy-transparent.md`](../components/egress/docs/mitmproxy-transparent.md#6-chain-through-an-upstream-proxy-corporateforward-egress); operations: [`docs/guides/egress-upstream-proxy.md`](../docs/guides/egress-upstream-proxy.md).

**Runtime notes:** Docker — `networkPolicy` requires `docker.network_mode = "bridge"`; raise `readiness_timeout_seconds` if the sidecar starts slowly. Kubernetes — set `requests`/`limits` when namespace `LimitRange` defaults are too large. Sidecar behavior: [`components/egress/README.md`](../components/egress/README.md).

---

## `[storage]`

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `allowed_host_paths` | list of strings | `[]` | Absolute path prefixes allowed for host bind mounts; **empty rejects all** (secure-by-default). |
| `ossfs_mount_root` | string | `"/mnt/ossfs"` | Host directory under which OSSFS mounts resolve (`<root>/<bucket>/...`). |
| `volume_default_size` | string | `"1Gi"` | Default size for auto-created Kubernetes PVCs without an explicit size. |

Sandbox volume models (`host`, `pvc`, `ossfs`) are documented in the OpenAPI specs and OSEPs.

---

## `[store]`

Persistence backend for server-managed resources (snapshot metadata today; future resources reuse it).

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `type` | string | `"sqlite"` | `sqlite` or `postgresql`. |
| `path` | string | `"~/.opensandbox/opensandbox.db"` | SQLite file path; parents created automatically. |
| `postgresql.dsn` | string | unset | Required when `type = "postgresql"`; prefer the `OPENSANDBOX_STORE_POSTGRESQL_DSN` env var in production. |
| `postgresql.min_pool_size` / `max_pool_size` | integer | `1` / `10` | Connection pool bounds per server process. |
| `postgresql.connect_timeout_seconds` | integer | `5` | Max time to establish initial connections. |
| `postgresql.pool_timeout_seconds` | number | `5` | Max wait for a pooled connection. |
| `postgresql.snapshot_recovery_interval_seconds` | number | `15` | Unfinished-snapshot recovery scan interval (takeover latency, not correctness). |

Notes:

- PostgreSQL + Kubernetes supports multiple active server processes for snapshot create/recovery/delete (coordination via deterministic name + state CAS; Kubernetes admits one CR, one terminal DB transition wins). Errors/timeouts leave records in `Creating` for a later scan.
- SQLite/Docker keep single-process recovery — do not treat this as a general multi-active guarantee.
- Switching backends does **not** migrate existing metadata; start empty or migrate separately. `memory` is intentionally not supported as default (state must survive restarts).

```toml
[store]
type = "postgresql"
```

```bash
export OPENSANDBOX_STORE_POSTGRESQL_DSN='postgresql://opensandbox:password@postgres:5432/opensandbox?sslmode=require'
```

Kubernetes setup: [Kubernetes Deployment](../docs/deployment/index.md#use-postgresql-for-server-persistence).

---

## `[secure_runtime]`

Optional strong isolation runtimes.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `type` | string | `""` | `""` (runc), **`gvisor`**, **`kata`**, **`firecracker`** (Kubernetes-only). |
| `docker_runtime` | string \| omitted | `null` | Docker OCI runtime name (e.g. `runsc`, `kata-runtime`). |
| `k8s_runtime_class` | string \| omitted | `null` | Kubernetes RuntimeClass name (e.g. `gvisor`, `kata-qemu`). |

Validation: empty `type` → both runtime fields must be omitted; `firecracker` → `k8s_runtime_class` required; `gvisor`/`kata` → at least one required.

One server process has one `[secure_runtime]` profile; API requests cannot select a different runtime class. Multiple profiles in one cluster require separately addressed server releases — see [`docs/guides/secure-container.md`](../docs/guides/secure-container.md).

---

## `[renew_intent]`

Auto-renew sandbox expiration on observed access (lifecycle proxy and/or Redis queue). Off by default. Design: [OSEP-0009](../oseps/0009-auto-renew-sandbox-on-ingress-access.md). Redis uses dotted keys under the same table.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `enabled` | boolean | `false` | Master switch. |
| `min_interval_seconds` | integer | `60` | Per-sandbox renewal cooldown (≥ 1). |
| `redis.enabled` | boolean | `false` | Enable Redis list consumer for gateway renew intents. |
| `redis.dsn` | string \| omitted | `null` | Redis URL. **Required** when `redis.enabled = true`. |
| `redis.queue_key` | string | `"opensandbox:renew:intent"` | Redis list key. |
| `redis.consumer_concurrency` | integer | `8` | Concurrent BRPOP workers (≥ 1). |

Per-sandbox enablement uses `lifecycle.renewalPolicy.extendSeconds` at create time (OSEP-0009). The deprecated `extensions["access.renew.extend.seconds"]` key remains supported throughout 1.x and is removed in 2.0; when both are set, their parsed values must match or creation fails with HTTP 400. See the [Renew-on-Access guide](../docs/guides/renew-on-access.md).

---

## `[automation]`

Umbrella for lifecycle automation of fast sandboxes (kubernetes runtime only). Only sandboxes declaring `lifecycle.idlePolicy` at create time are affected; absent that field everything below is a no-op. Design: [OSEP-0024](../oseps/0024-auto-pause-and-wake-on-access-for-fast-sandbox.md).

### `[automation.idle_pause]` — idle auto-pause / auto-wake

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `enabled` | boolean | `false` | Master switch: enables the idle sweeper. |
| `sweep_interval_seconds` | integer | `15` | Period between sweeps per replica (≥ 1). |
| `activity_ttl_seconds` | integer | `1800` | TTL of per-sandbox activity keys (≥ 30). Must stay >= the largest accepted `idlePolicy.idleTimeoutSeconds` (30–86400). Keep the ingress `--activity-ttl-seconds` equal. |
| `redis.dsn` | string \| omitted | `null` | Redis for activity keys and pause locks. **Required** when `enabled = true` (the sweeper cannot run without it). Must match the ingress `--activity-redis-dsn`. |
| `redis.lock_ttl_seconds` | integer | `30` | TTL of the per-sandbox pause admission lock (SET NX EX, ≥ 1). |

---

## `[otel]`

Optional OTLP metrics export for Server HTTP requests and SDK-reported create latency. Off by default; middleware and ingestion stay active but record as noops.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `enabled` | boolean | `false` | Enable OTLP metrics export. |
| `endpoint` | string \| omitted | `null` | OTLP HTTP metrics endpoint; when omitted, uses `OTEL_EXPORTER_OTLP_ENDPOINT` / `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT`. |
| `service_name` | string | `"opensandbox-server"` | `service.name` resource attribute. |
| `export_interval_millis` | integer | `60000` | Export interval (≥ 1000). |

| Metric | Type | Attributes | Description |
|--------|------|------------|-------------|
| `server.http.request.duration` | Histogram (`ms`) | `http_method`, `http_route`, `http_status_code` | HTTP latency. Matched route template; unmatched/auth-failed requests use `http_route=unknown`; extension methods use `http_method=OTHER`. Never includes IDs, keys, bodies, or other unbounded data. |
| `opensandbox.sandbox.create.duration` | Histogram (`ms`) | `sdk.language`, `sdk.version`, `success` | SDK-reported create latency until ready/failure. |

---

## Environment variables (outside TOML)

| Variable | Where used | Description |
|----------|------------|-------------|
| `SANDBOX_CONFIG_PATH` | `config.py`, CLI | TOML file path; overrides default `~/.sandbox.toml`. |
| `OPENSANDBOX_SERVER_API_KEY` | `config.py` | Overrides `server.api_key`. |
| `OPENSANDBOX_INSECURE_SERVER` | `config.py` | `YES` acknowledges the risk of running without an API key. |
| `OPENSANDBOX_STORE_POSTGRESQL_DSN` | `config.py` | Overrides `store.postgresql.dsn`. |
| `OPENSANDBOX_SECURE_ACCESS_KEYS` / `OPENSANDBOX_SECURE_ACCESS_ACTIVE_KEY` | `config.py` | Inject `ingress.secure_access`. |
| `DOCKER_HOST` | Docker service | Docker daemon address. |
| `OTEL_EXPORTER_OTLP_ENDPOINT` / `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT` | OTEL exporter | Default endpoint when `[otel].endpoint` is omitted. |

---

## Cross-field validation rules

Enforced when `AppConfig` is parsed (`AppConfig.validate_runtime_blocks` in `config.py`):

1. **`runtime.type = "docker"`** — no `[kubernetes]` / `[agent_sandbox]`; `ingress.mode` must be `"direct"`; `secure_runtime.type = "firecracker"` forbidden.
2. **`runtime.type = "kubernetes"`** — `[kubernetes]` defaulted if missing; `[agent_sandbox]` only when `workload_provider = "agent-sandbox"` (case-insensitive).
3. **`ingress.mode = "gateway"`** — `[ingress.gateway]` required; `address`/`route.mode` validated (wildcard domain for `wildcard`, no scheme, etc.); `secure_access` requires this mode.
4. **`[automation.idle_pause]`** — `redis.dsn` required (non-empty) when `enabled = true`.

---

## Source of truth

If this document and the running server disagree, prefer:

1. **`opensandbox_server/config.py`** — authoritative Pydantic schema and defaults.
2. **Example TOML files** in `server/` — reviewed snapshots.
3. **Release notes** — experimental flags and breaking changes.

For API request fields (`networkPolicy`, volumes, etc.), see the OpenAPI specs under [`specs/`](../specs/) and the [Server README](README.md).
