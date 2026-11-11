---
title: SDKs
description: Choose an OpenSandbox SDK and compare lifecycle, Client Pool, tracing, and diagnostics support.
---

# SDKs

OpenSandbox provides five sandbox SDKs for lifecycle management, command execution, file operations, and resource metrics. This site follows the repository's default branch; a feature merged here may require a newer package than the one you have installed. Check the [release index](https://github.com/opensandbox-group/OpenSandbox/releases) when upgrading.

## Installation

| Language | Package | Install |
| --- | --- | --- |
| [Python](/sdks/python) | `opensandbox` | `pip install opensandbox` |
| [JavaScript/TypeScript](/sdks/javascript) | `@alibaba-group/opensandbox` | `npm install @alibaba-group/opensandbox` |
| [Kotlin/Java](/sdks/kotlin) | `com.alibaba.opensandbox:sandbox` | Gradle/Maven |
| [Go](/sdks/go) | `github.com/alibaba/OpenSandbox/sdks/sandbox/go` | `go get github.com/alibaba/OpenSandbox/sdks/sandbox/go` |
| [C#/.NET](/sdks/csharp) | `Alibaba.OpenSandbox` | `dotnet add package Alibaba.OpenSandbox` |

## Capability coverage

“Yes” means a public SDK entry point exists. Availability also depends on the server runtime and the execd version installed in the sandbox.

| Capability | Python async/sync | JavaScript/TypeScript | Kotlin/Java | Go | C#/.NET |
| --- | --- | --- | --- | --- | --- |
| Create, connect, renew, pause/resume, kill | Yes | Yes | Yes | Yes | Yes |
| Snapshots and template management | Yes | Yes | Yes | Yes | Yes |
| Resource requests, volumes, lifecycle hooks | Yes | Yes | Yes | Yes | Yes |
| Commands, files, resource metrics | Yes | Yes | Yes | Yes | Yes |
| Egress policy and Credential Vault | Yes | Yes | Yes | Yes | Yes |
| Isolated sessions | Yes | Yes | Yes | Yes | Yes |
| [Client Pool](/guides/client-pool), including Redis | Yes | Yes | Yes | Yes | No |
| [Pool warmup tracing](/sdks/observability#pool-warmup-tracing) | Yes | Yes, fewer attributes | Yes | No | No |
| [Diagnostic context, logs/events](/api/#diagnostics) | Yes | Yes | Yes | Yes | Yes |
| [Create-latency telemetry](/sdks/observability#creation-metrics) | Yes | Yes | Yes | Yes | Yes |

### Differences that affect application code

- **Pools:** Go exposes different warmup controls; the fields accepted by the default pool creator also differ across languages. See the [pool configuration matrix](/guides/client-pool#configuration).
- **Go command control:** interrupt, command status, and accumulated command logs are exposed by `ExecdClient`, rather than the high-level `Sandbox` wrapper. Neither `Sandbox.CreateSession` nor `ExecdClient.CreateSession` accepts an initial working directory. Set `Cwd` when running a command in the session instead.
- **Go connect/resume readiness:** pass `ReadyOptions` to request readiness checks. Without it, connecting resolves the endpoint without checking sandbox health.
- **Metrics streaming:** Go exposes `ExecdClient.WatchMetrics`; the other SDKs' stable metrics services provide point-in-time reads. The [CLI](/cli/) also has a metrics stream via `osb sandbox metrics --watch`.
- **Timeout units:** use Python `timedelta` and JVM `Duration`; JavaScript and C# command timeouts use seconds. Go `RunCommandRequest.Timeout` uses **milliseconds**.

Template-backed sandboxes require an explicit TTL and inherit their workload configuration from the published template. Their egress policy is managed through the lifecycle API; they do not have a sandbox-side Credential Vault.

## Feature guides

- [Client Pool](/guides/client-pool): keep a ready buffer, select an acquire policy, share state through Redis, and retire a pool namespace.
- [Observability](/sdks/observability): configure pool warmup traces and creation metrics, understand the default settings, and locate slow startup phases.

## Diagnostics

All sandbox SDKs expose diagnostic context, logs, and events on both `Sandbox`
and `SandboxManager`. Use a manager when execd is not ready: these requests go
to the lifecycle server without connecting to the sandbox.

| Language | Context | Logs / events |
| --- | --- | --- |
| Python async/sync | `get_diagnostic_context` | `get_diagnostic_logs` / `get_diagnostic_events` |
| Kotlin/Java, JavaScript | `getDiagnosticContext` | `getDiagnosticLogs` / `getDiagnosticEvents` |
| Go | `GetDiagnosticContext` | `GetDiagnosticLogs` / `GetDiagnosticEvents` |
| C# | `GetDiagnosticContextAsync` | `GetDiagnosticLogsAsync` / `GetDiagnosticEventsAsync` |

Manager methods take a sandbox ID; sandbox methods use the current instance.
Context returns the backend type, backend-defined identifiers, and lifecycle state
when available. Log/event methods also require a scope such as `container` or
`runtime`. Availability depends on the server backend. URL-delivered content is
not downloaded automatically; inspect `truncated` and `warnings`.
C# `SdkDiagnosticsOptions` separately controls local SDK logging.

## Lifecycle and cleanup

High-level image/snapshot creation defaults to a 10-minute TTL. Configure the TTL explicitly for your workload. To disable expiration, use Python `timeout=None`, JavaScript `timeoutSeconds: null`, Kotlin `timeout(null)`, or the Go/C# `ManualCleanup` option. Template creation requires a TTL.

`close()`, `DisposeAsync()`, and context-manager exit release local client resources; they do not kill the remote sandbox. Kill it in a `finally`/`defer` block, or use Python's `destroy()` helper. Pool acquisitions are consumed once and are not returned to the idle buffer.

## CLI

Use the [CLI](/cli/) to manage sandboxes, run commands, and work with files from a terminal.

```bash
uv tool install opensandbox-cli
```

## MCP server

The [MCP server](/sdks/mcp) exposes sandbox operations to MCP-capable clients:

```bash
pip install opensandbox-mcp
```
