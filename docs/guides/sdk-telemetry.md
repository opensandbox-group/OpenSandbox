---
title: SDK Telemetry
description: What sandbox create latency metrics the SDKs report, when they fire, and how to disable them.
---

# SDK Telemetry

OpenSandbox SDKs optionally report sandbox creation latency to the lifecycle server. Reporting is best-effort: failures never affect `Sandbox.create`, and the payload contains no user content.

## Requirements

The `POST /v1/metrics/events` endpoint and the SDK reporters described below require the following minimum versions. Older SDKs simply do not emit events; older servers reject unknown routes with `404`, which the SDK swallows silently (see [Version skew](#version-skew) below).

| Component | Minimum version |
|-----------|-----------------|
| Server (`opensandbox-server`) | `0.2.2` |
| Python SDK (`opensandbox`) | `0.1.15` |
| JavaScript / TypeScript SDK (`@alibaba-group/opensandbox`) | `0.1.11` |
| Go SDK (`github.com/alibaba/OpenSandbox/sdks/sandbox/go`) | `1.0.5` |
| C# SDK (`Alibaba.OpenSandbox`) | `0.1.5` |
| Kotlin / Java SDK (`com.alibaba.opensandbox:sandbox`) | `1.0.17` |

### Version skew

Reporting is fire-and-forget in every SDK: the POST runs on a background task/thread, and any exception or non-2xx response is caught and logged at debug level. This means you can upgrade the SDK and the server independently:

- **New SDK, old server (`< 0.2.2`)**: the server returns `404` for `/v1/metrics/events`. The SDK ignores the response. `Sandbox.create` behavior is unchanged and no user-visible error is raised. The only side effect is one debug-level log line per create call.
- **Old SDK, new server**: the SDK does not emit events. The server histogram simply records nothing for that client.
- **Network errors, TLS failures, timeouts**: same behavior as the `404` case — swallowed, `Sandbox.create` unaffected.

## What is sent

After create succeeds or fails, the SDK fire-and-forget posts to `POST /v1/metrics/events`:

```json
{
  "eventType": "sandbox.create",
  "sandboxId": "sbx_...",
  "image": "python:3.12",
  "createDurationMs": 1842,
  "success": true
}
```

- `sandboxId` / `image` may be omitted when create fails early.
- SDK language and version come from the HTTP `User-Agent` header (for example `OpenSandbox-Python-SDK/0.1.15`), not from body fields.

The server accepts the event with `204` and, when `[otel]` is enabled, records an OTEL histogram. See [server configuration](https://github.com/opensandbox-group/OpenSandbox/blob/main/server/configuration.md#otel).

## When it runs

| SDK | Trigger |
|-----|---------|
| Python (async + sync) | After `Sandbox.create` / sync create completes or raises |
| JavaScript / TypeScript | After `Sandbox.create` completes or fails |
| Go | After `CreateSandbox` completes or fails |
| C# | After `Sandbox.CreateAsync` completes or fails |
| Kotlin | After `Sandbox.builder()...build()` completes or fails |

## How to disable

Default is on. Opt out with either:

1. Environment variable (all SDKs):

```bash
export OPENSANDBOX_DISABLE_METRICS=1
```

2. Connection config field:

::: code-group

```python [Python]
from opensandbox import ConnectionConfig, Sandbox

config = ConnectionConfig(disable_metrics=True)
sandbox = await Sandbox.create("python:3.12", connection_config=config)
```

```typescript [JavaScript]
import { ConnectionConfig, Sandbox } from "@alibaba-group/opensandbox";

const connectionConfig = new ConnectionConfig({ disableMetrics: true });
const sandbox = await Sandbox.create({
  image: "python:3.12",
  connectionConfig,
});
```

```go [Go]
cfg := opensandbox.ConnectionConfig{DisableMetrics: true}
sandbox, err := opensandbox.CreateSandbox(ctx, cfg, opensandbox.SandboxCreateOptions{
    Image: "python:3.12",
})
```

```csharp [C#]
using OpenSandbox;
using OpenSandbox.Config;

var connectionConfig = new ConnectionConfig(new ConnectionConfigOptions
{
    DisableMetrics = true,
});
var sandbox = await Sandbox.CreateAsync(new SandboxCreateOptions
{
    Image = "python:3.12",
    ConnectionConfig = connectionConfig,
});
```

```kotlin [Kotlin]
import com.alibaba.opensandbox.sandbox.Sandbox
import com.alibaba.opensandbox.sandbox.config.ConnectionConfig

val connectionConfig = ConnectionConfig.builder()
    .disableMetrics(true)
    .build()
val sandbox = Sandbox.builder()
    .image("python:3.12")
    .connectionConfig(connectionConfig)
    .build()
```

:::

Use opt-out for air-gapped / on-prem environments that must not emit extra HTTP traffic, or when corporate egress logging should not include telemetry requests.
