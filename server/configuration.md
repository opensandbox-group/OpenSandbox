# OpenSandbox Server configuration reference

This document describes **all TOML configuration options** accepted by the OpenSandbox lifecycle server (`opensandbox-server`). The schema is defined in [`opensandbox_server/config.py`](opensandbox_server/config.py) (`AppConfig` and nested models).

- **Default config path:** `~/.sandbox.toml`
- **Override path:** set environment variable `SANDBOX_CONFIG_PATH` to an absolute or user-expandable path.
- **CLI:** `opensandbox-server --config /path/to/sandbox.toml` also sets `SANDBOX_CONFIG_PATH` for that process.

Example files in this repository:

| File | Purpose |
|------|---------|
| [`example.config.toml`](opensandbox_server/examples/example.config.toml) | Docker runtime (English) |
| [`example.config.zh.toml`](opensandbox_server/examples/example.config.zh.toml) | Docker runtime (中文) |
| [`example.config.k8s.toml`](opensandbox_server/examples/example.config.k8s.toml) | Kubernetes runtime (English) |
| [`example.config.k8s.zh.toml`](opensandbox_server/examples/example.config.k8s.zh.toml) | Kubernetes runtime (中文) |

---

## Table of contents

1. [Top-level sections](#top-level-sections)
2. [`[server]`](#server--lifecycle-api)
3. [`[log]`](#log)
4. [`[runtime]`](#runtime--required)
5. [`[docker]`](#docker--only-when-runtime--docker)
6. [`[kubernetes]`](#kubernetes--only-when-runtime--kubernetes)
7. [`[agent_sandbox]`](#agent_sandbox--only-with-kubernetes--agent-sandbox)
8. [`[ingress]`](#ingress)
9. [`[egress]`](#egress)
10. [`[storage]`](#storage)
11. [`[store]`](#store)
12. [`[secure_runtime]`](#secure_runtime)
13. [`[renew_intent]`](#renew_intent--experimental)
14. [Environment variables (outside TOML)](#environment-variables-outside-toml)
15. [Cross-field validation rules](#cross-field-validation-rules)

---

## Top-level sections

| Section | Required | When |
|---------|----------|------|
| `[server]` | No | Always (defaults apply if omitted) |
| `[log]` | No | Always (defaults apply if omitted) |
| `[runtime]` | **Yes** | Always |
| `[docker]` | No | `runtime.type = "docker"` |
| `[kubernetes]` | No | `runtime.type = "kubernetes"` (defaults are applied if missing) |
| `[agent_sandbox]` | No | Only when `kubernetes.workload_provider = "agent-sandbox"` |
| `[ingress]` | No | Optional; see [Ingress](#ingress) |
| `[egress]` | No | Required values when clients use `networkPolicy` on create |
| `[storage]` | No | Host bind mounts / OSSFS mount root |
| `[store]` | No | Server-managed persistent metadata backend |
| `[secure_runtime]` | No | gVisor / Kata / Firecracker |
| `[renew_intent]` | No | Experimental auto-renew on access |

---

## `[server]` — Lifecycle API

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `host` | string | `"0.0.0.0"` | Bind address for the HTTP API. |
| `port` | integer | `8080` | Listen port (1–65535). |
| `api_key` | string \| omitted | `null` | If set to a non-empty string, requests must send header `OPEN-SANDBOX-API-KEY` with this value (except documented public routes such as `/health`, `/docs`, `/redoc`). If omitted or empty, API key checks are skipped, but startup now requires explicit risk acknowledgment: interactive TTY confirmation (`YES`) or `OPENSANDBOX_INSECURE_SERVER=YES`. |
| `eip` | string \| omitted | `null` | Public IP or hostname used as the **host part** when the server returns sandbox endpoint URLs (notably Docker runtime). |
| `max_sandbox_timeout_seconds` | integer \| omitted | `null` | Upper bound on sandbox TTL in seconds for **create** requests that specify `timeout`. Must be ≥ **60** if set. Omit to disable the server-side cap. |
| `timeout_keep_alive` | integer | `30` | Idle keep-alive timeout (seconds) passed to uvicorn. |
| `timeout_graceful_shutdown` | integer | `5` | Seconds uvicorn waits for in-flight requests to finish before forcing shutdown. Ensures Ctrl+C terminates promptly even when a long-running operation (e.g. image pull) is in progress. |
| `limit_concurrency` | integer | `1024` | Maximum concurrent connections before returning 503. Provides backpressure protection under burst load. Set to `0` to disable the cap (TOML cannot express `null`). |
| `backlog` | integer | `2048` | Socket listen backlog passed to uvicorn. |
| `thread_pool_size` | integer | `200` | Maximum size of the anyio default threadpool used by FastAPI to run sync route handlers. The anyio default of 40 throttles bursts of blocking sandbox list/get/delete operations under high concurrency. |
| `loop` | `"auto"` \| `"uvloop"` \| `"asyncio"` | `"auto"` | Event loop implementation. `auto` prefers uvloop and falls back to asyncio. |
| `http` | `"auto"` \| `"httptools"` \| `"h11"` | `"auto"` | HTTP protocol parser. `auto` prefers httptools and falls back to h11. |

---

## `[log]`

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `level` | string | `"INFO"` | Python logging level for the server process (e.g. `"DEBUG"`, `"INFO"`, `"WARNING"`). |
| `file_enabled` | boolean | `false` | When `true`, logs are written to rotating files instead of stdout. |
| `file_path` | string \| omitted | `null` | Override path for the main log file. Defaults to `~/logs/opensandbox/server.log` when `file_enabled = true`. |
| `access_file_path` | string \| omitted | `null` | Override path for the HTTP access log file. Defaults to `~/logs/opensandbox/access.log` when `file_enabled = true`. |
| `file_max_bytes` | integer | `104857600` (100 MB) | Max bytes per log file before rotation. |
| `file_backup_count` | integer | `5` | Number of rotated log files to retain. |

---

## `[runtime]` — **required**

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `type` | string | — | **`docker`** or **`kubernetes`**. Selects which runtime implementation loads. |
| `execd_image` | string | — | OCI image containing the **execd** binary used to bootstrap command/file access inside the sandbox. |

---

## `[docker]` — only when `runtime.type = "docker"`

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `network_mode` | string | `"host"` | Docker network attachment for sandbox containers: **`host`**, **`bridge`**, or a **custom user-defined network name**. Egress sidecar + `networkPolicy` require **`bridge`** (see [Egress](#egress)). |
| `api_timeout` | integer \| omitted | `null` | Docker API timeout in **seconds**. If unset, the code uses default **180** s where applicable. |
| `host_ip` | string \| omitted | `null` | Hostname or IP used when **rewriting** bridge-mode endpoint URLs (e.g. server runs in Docker and clients need a host-reachable address). Often `host.docker.internal` or the host LAN IP on Linux. |
| `drop_capabilities` | list of strings | See `config.py` | Linux capabilities **dropped** from sandbox containers (security hardening). |
| `apparmor_profile` | string \| omitted | `null` | Optional AppArmor profile name (e.g. `"docker-default"`). Empty/unset lets Docker use its default. |
| `no_new_privileges` | boolean | `true` | Sets `no-new-privileges` to block privilege escalation. |
| `seccomp_profile` | string \| omitted | `null` | Seccomp profile name or **absolute path**; empty uses Docker default seccomp. |
| `pids_limit` | integer \| null | `4096` | Max PIDs per sandbox container; set to **`null`** to disable the limit. |
| `port_range_min` | integer | `40000` | Lower bound of the host port range used by bridge-mode sandbox port allocation. Must be less than `port_range_max`. Each sandbox needs 2–3 host ports (2 without egress, 3 with egress sidecar). Narrow this range to match your firewall policy — e.g., 100 concurrent sandboxes ≈ 300 ports. |
| `port_range_max` | integer | `60000` | Upper bound of the host port range. Range must span ≥ 100 ports for reliable allocation. |

---

## `[kubernetes]` — only when `runtime.type = "kubernetes"`

If `runtime.type = "kubernetes"` and the `[kubernetes]` table is absent, the server instantiates defaults from `KubernetesRuntimeConfig`.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `kubeconfig_path` | string \| omitted | `null` | Path to kubeconfig (expandable, e.g. `~/.kube/config`). In-cluster configs often leave this unset and rely on in-cluster credentials. |
| `namespace` | string \| omitted | `null` | Namespace for sandbox workloads. |
| `workload_provider` | string \| omitted | `null` | One of: **`batchsandbox`**, **`agent-sandbox`**. If omitted, the **first registered** provider is used (currently **`batchsandbox`**). |
| `batchsandbox_template_file` | string \| omitted | `null` | Path to **BatchSandbox** CR YAML template when `workload_provider = "batchsandbox"`. |
| `image_pull_policy` | string \| omitted | `"IfNotPresent"` | Image pull policy for the BatchSandbox main container. Values: **`Always`**, **`IfNotPresent`**, **`Never`**. |
| `sandbox_create_timeout_seconds` | integer | `60` | Max time to wait for a new sandbox to become ready (e.g. IP assigned), in seconds. |
| `sandbox_create_poll_interval_seconds` | float | `1.0` | Poll interval while waiting for readiness. |
| `snapshot_create_timeout_seconds` | integer | `900` | Max time to wait for a Kubernetes public snapshot to become ready, in seconds. Set this greater than the controller snapshot `commitJobTimeout` / `--commit-job-timeout`. |
| `informer_enabled` | boolean | `true` | **[Beta]** Use informer/watch cache for reads to reduce API load. |
| `informer_resync_seconds` | integer | `300` | **[Beta]** Full resync period for the informer cache. |
| `informer_watch_timeout_seconds` | integer | `60` | **[Beta]** Watch stream restart interval. |
| `read_qps` | float | `0` | K8s API **get/list** rate limit (QPS). **0** = unlimited. |
| `read_burst` | integer | `0` | Burst for read limiter; **0** means use `read_qps` as burst (minimum 1 internally). |
| `write_qps` | float | `0` | K8s API **write** rate limit (QPS). **0** = unlimited. |
| `write_burst` | integer | `0` | Burst for write limiter. |
| `execd_init_resources` | table \| omitted | `null` | Optional resource requests/limits for the **execd init** container. |

### BatchSandbox vs agent-sandbox

Kubernetes workloads are created by a **workload provider**. There is **no** `[batchsandbox]` section in TOML — BatchSandbox is configured entirely under **`[kubernetes]`**, plus shared sections like `[egress]`, `[ingress]`, `[storage]`, `[secure_runtime]`.

| | **BatchSandbox** (default provider) | **agent-sandbox** ([kubernetes-sigs/agent-sandbox](https://github.com/kubernetes-sigs/agent-sandbox)) |
|--|--------------------------------------|--------------------------------------------------------------------------------------------------------|
| `kubernetes.workload_provider` | `"batchsandbox"` or **omit** (factory default is `batchsandbox`) | `"agent-sandbox"` |
| Template file | **`kubernetes.batchsandbox_template_file`** — path to **BatchSandbox** CR YAML | **`agent_sandbox.template_file`** in [`[agent_sandbox]`](#agent_sandbox--only-with-kubernetes--agent-sandbox) |
| Image pull policy | **`kubernetes.image_pull_policy`** — writes `imagePullPolicy` into the BatchSandbox pod template main container | Not currently used |
| Extra TOML table | None | **`[agent_sandbox]`** is required (see below) |

**BatchSandbox-only config keys in `config.py`:** `batchsandbox_template_file` and `image_pull_policy` on `KubernetesRuntimeConfig`. Everything else in the `[kubernetes]` table (namespace, kubeconfig, informer, API QPS, `sandbox_create_*`, `execd_init_resources`, …) applies to **whichever** provider you select.

### `kubernetes.execd_init_resources`

| Key | Type | Description |
|-----|------|-------------|
| `limits` | map string → string | e.g. `{ cpu = "100m", memory = "128Mi" }` |
| `requests` | map string → string | e.g. `{ cpu = "50m", memory = "64Mi" }` |

---

## `[agent_sandbox]` — only with `kubernetes.workload_provider = "agent-sandbox"`

Used with the **kubernetes-sigs/agent-sandbox** Sandbox CRD provider.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `template_file` | string \| omitted | `null` | Path to **Sandbox CR** YAML template. |
| `shutdown_policy` | string | `"Delete"` | **`Delete`** or **`Retain`** when the sandbox expires. |
| `ingress_enabled` | boolean | `true` | Whether ingress routing to agent-sandbox pods is expected. |

---

## `[ingress]`

Controls how **ingress exposure** is described for sandbox endpoints (especially behind gateways). **When `runtime.type = "docker"`, only `mode = "direct"` is allowed.**
`secureAccess` is currently supported only for **Kubernetes** sandboxes when **`ingress.mode = "gateway"`**.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `mode` | string | `"direct"` | **`direct`** — clients reach sandboxes without an L7 gateway configured here. **`gateway`** — use `[ingress.gateway]` for address and routing mode (Kubernetes-oriented deployments). |

### When `mode = "gateway"`

You must set **`[ingress.gateway]`** and omit gateway when `mode = "direct"`.

| Key | Type | Description |
|-----|------|-------------|
| `address` | string | Gateway host (**no `http://` or `https://`**). For `route.mode = "wildcard"`, must be a **wildcard domain** (e.g. `*.example.com`). Otherwise a normal domain, IP, or `IP:port`. |
| `route.mode` | string | **`wildcard`** — host-based routing; **`uri`** — path-prefix routing; **`header`** — header-based routing. |

Response URL shapes depend on `route.mode` (see server README / ingress component docs).

---

## `[egress]`

Configures the **egress sidecar** image and enforcement mode. The server only attaches the sidecar when a sandbox is created **with** a `networkPolicy` in the API request.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `image` | string \| omitted | `null` | OCI image for the egress sidecar. **Required in config** when clients send **`networkPolicy`** (create request). |
| `mode` | string | `"dns"` | Passed to the sidecar as `OPENSANDBOX_EGRESS_MODE`. Values: **`dns`** — DNS-proxy-based enforcement (CIDR/static IP rules **not** enforced); **`dns+nft`** — adds nftables where available so **CIDR/IP** rules can be enforced. |
| `disable_ipv6` | bool | `true` | IPv6 egress is incomplete (especially on Kubernetes). **Default on**; set `false` only when you want IPv6 left up in the netns. Details in [IPv6 and egress](#ipv6-and-egress) below. |

### IPv6 and egress

OpenSandbox egress does **not** treat IPv6 as a first-class, fully covered path—gaps show up most often under **`runtime.type = "kubernetes"`** (pod networking, CNI). The default **`disable_ipv6 = true`** matches the usual need on **dual-stack** CNI: do not rely on incomplete IPv6 egress. Set **`false`** when the cluster is effectively **IPv4-only** and you deliberately want IPv6 enabled in the sandbox network namespace, or when you accept those gaps for experiments.

**Docker notes:**

- `egress.image` must be set when using `networkPolicy`.
- Outbound policy requires **`docker.network_mode = "bridge"`**; `networkPolicy` is rejected for incompatible network modes.

**Kubernetes notes:**

- When `networkPolicy` is set, the workload includes an egress sidecar built from `egress.image`.

See [`components/egress/README.md`](../components/egress/README.md) for sidecar behavior and limits.

---

## `[storage]`

Host-side storage related to **volume mounts** (host bind allowlist and OSSFS mount layout).

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `allowed_host_paths` | list of strings | `[]` | Absolute path **prefixes** allowed for **host** bind mounts. If **empty**, all host bind mounts are rejected (secure-by-default). |
| `ossfs_mount_root` | string | `"/mnt/ossfs"` | Host directory under which OSSFS-backed mounts are resolved (`<root>/<bucket>/...`). |
| `volume_default_size` | string | `"1Gi"` | Default storage size for auto-created Kubernetes PVCs when the caller does not specify a size in the PVC provisioning hints. |

Sandbox **volume** models (`host`, `pvc`, `ossfs`) in API requests are documented in the OpenAPI specs and OSEPs; this table only covers **server** storage settings.

---

## `[store]`

Configures the persistence backend for **server-managed resources**. This is a
server-wide store, not a snapshot-specific backend. Snapshot metadata is the
first resource persisted here; future persistent server resources should reuse
the same backend.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `type` | string | `"sqlite"` | Server persistence backend type. Currently only **`sqlite`** is supported. |
| `path` | string | `"~/.opensandbox/opensandbox.db"` | Filesystem path to the SQLite database file used for server-managed metadata. Parent directories are created automatically when needed. |

**Notes**

- The default SQLite backend gives local and single-node deployments persistent
  metadata without requiring an external database service.
- `memory` is intentionally **not** the default because server-managed snapshot
  resources must survive process restarts.
- Higher-level components should depend on repository abstractions rather than
  importing `sqlite3` directly.

---

## `[secure_runtime]`

Optional **strong isolation** runtimes (gVisor, Kata, Firecracker).

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `type` | string | `""` | **`""`** — default OCI runtime (runc). **`gvisor`**, **`kata`**, **`firecracker`**. **`firecracker`** is **Kubernetes-only**. |
| `docker_runtime` | string \| omitted | `null` | Docker **OCI runtime name** (e.g. `runsc` for gVisor, `kata-runtime` for Kata). |
| `k8s_runtime_class` | string \| omitted | `null` | Kubernetes **RuntimeClass** name (e.g. `gvisor`, `kata-qemu`, `kata-fc`). |

**Validation (summary):**

- If `type` is empty, **`docker_runtime`** and **`k8s_runtime_class`** must be omitted.
- If `type` is **`firecracker`**, **`k8s_runtime_class`** is **required** (`docker` runtime cannot use Firecracker).
- If `type` is **`gvisor`** or **`kata`**, at least one of **`docker_runtime`** or **`k8s_runtime_class`** must be set.

See [`docs/guides/secure-container.md`](../docs/guides/secure-container.md) for installation and node requirements.

---

## `[renew_intent]` — **experimental**

**🧪 Experimental:** auto-renew sandbox expiration when access is observed (lifecycle proxy and/or Redis queue). Off by default. Full design: [OSEP-0009](../oseps/0009-auto-renew-sandbox-on-ingress-access.md).

Use **dotted keys** under the same table for Redis (valid in TOML):

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `enabled` | boolean | `false` | Master switch for renew-on-access. |
| `min_interval_seconds` | integer | `60` | Minimum seconds between renewals for the same sandbox (cooldown). ≥ 1. |
| `redis.enabled` | boolean | `false` | Enable Redis list consumer for ingress-gateway renew intents. |
| `redis.dsn` | string \| omitted | `null` | Redis URL, e.g. `redis://127.0.0.1:6379/0`. **Required** when `redis.enabled = true`. |
| `redis.queue_key` | string | `"opensandbox:renew:intent"` | Redis list key for renew-intent payloads. |
| `redis.consumer_concurrency` | integer | `8` | Concurrent BRPOP workers (≥ 1). |

Per-sandbox enablement uses create request extensions (see OSEP-0009 and `example.config.toml` comments).

---

## Environment variables (outside TOML)

These are read by the server or runtime code in addition to the TOML file:

| Variable | Where used | Description |
|----------|------------|-------------|
| `SANDBOX_CONFIG_PATH` | `config.py`, CLI | Path to the TOML file. Overrides the default `~/.sandbox.toml` when set. |
| `OPENSANDBOX_SERVER_API_KEY` | `config.py` | Overrides the API key from the TOML file. |
| `DOCKER_HOST` | Docker service | Standard Docker daemon address (e.g. `unix:///var/run/docker.sock`). |

---

## Cross-field validation rules

Rules enforced when the full `AppConfig` is parsed (see `AppConfig.validate_runtime_blocks` in `config.py`):

1. **`runtime.type = "docker"`**  
   - Must **not** include `[kubernetes]` or `[agent_sandbox]`.  
   - If `[ingress]` is present, **`ingress.mode` must be `"direct"`**.  
   - **`secure_runtime.type = "firecracker"`** is not allowed.

2. **`runtime.type = "kubernetes"`**  
   - `[kubernetes]` is created with defaults if missing.  
   - `[agent_sandbox]` is **only** allowed when **`kubernetes.workload_provider`** (case-insensitive) is **`agent-sandbox`**.

3. **`ingress.mode = "gateway"`**  
   - `[ingress.gateway]` is **required**; address and `route.mode` must satisfy the validators (wildcard domain for `wildcard` route mode, no URL scheme in `address`, etc.).

4. **`secure_runtime`**  
   - See [Secure runtime](#secure_runtime) above.

---

## Source of truth

If this document and the running server disagree, prefer:

1. **`opensandbox_server/config.py`** — authoritative Pydantic schema and defaults.  
2. **Example TOML files** in the `server/` directory — reviewed snapshots for Docker/K8s.  
3. **Release notes** — for experimental flags and breaking changes.

For API request fields (create sandbox, `networkPolicy`, volumes, etc.), see the OpenAPI specs under [`specs/`](../specs/) and the main [Server README](README.md).
