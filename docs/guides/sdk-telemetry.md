---
title: SDK Telemetry
description: What sandbox create latency metrics the SDKs report, when they fire, and how to disable them.
---

# SDK Telemetry

OpenSandbox SDKs optionally report sandbox creation latency to the lifecycle server. Reporting is best-effort: failures never affect `Sandbox.create`, and the payload contains no user content.

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
- SDK language and version come from the HTTP `User-Agent` header (for example `OpenSandbox-Python-SDK/0.1.14`), not from body fields.

The server accepts the event with `204` and, when `[otel]` is enabled, records an OTEL histogram. See [server configuration](https://github.com/opensandbox-group/OpenSandbox/blob/main/server/configuration.md#otel).

## When it runs

| SDK | Trigger |
|-----|---------|
| Python (async + sync) | After `Sandbox.create` / sync create completes or raises |
| JavaScript / TypeScript | After `Sandbox.create` completes or fails |
| Go | After `CreateSandbox` completes or fails |

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

:::

Use opt-out for air-gapped / on-prem environments that must not emit extra HTTP traffic, or when corporate egress logging should not include telemetry requests.
