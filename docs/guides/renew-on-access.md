---
title: Renew-on-Access
description: Keep busy sandboxes alive by extending their expiration automatically on observed access — opt-in via lifecycle.renewalPolicy, driven by server-proxy hits or ingress gateway renew-intents.
---

# Renew-on-Access Guide

This guide shows how to opt sandboxes into **renew-on-access** (OSEP-0009): when the server proxy or the ingress gateway observes traffic for a sandbox, the lifecycle server silently moves the sandbox's expiration forward — a busy sandbox stays alive, an idle one still expires on schedule, and clients never call the renew API themselves.

For the design rationale, see [OSEP-0009](https://github.com/opensandbox-group/OpenSandbox/blob/main/oseps/0009-auto-renew-sandbox-on-ingress-access.md). For expiring *idle* fast sandboxes instead of busy ones, see [Auto-Pause & Wake-on-Access](/guides/auto-pause-wake).

## How it works in one minute

```text
access    a request hits /sandboxes/{id}/proxy/{port} (server proxy mode)
          and/or is proxied by the ingress gateway (Redis queue mode)
intent    the hit becomes a renew intent, throttled per sandbox
renew     the server checks the gates and moves the expiration to
          max(now + extendSeconds, current expiresAt)
```

Nothing happens unless all three layers agree:

1. **The server operator** enables the renew-intent integration (`[renew_intent]`).
2. **The trigger path** exists: server-proxy hits (built into the server) and/or the ingress gateway publishing intents to Redis.
3. **The sandbox opts in** at creation with `lifecycle.renewalPolicy`.

A sandbox that never declares a `renewalPolicy` is never auto-renewed — omission is exactly today's behavior.

## Step 1 — Enable the integration on the server

In the server's `config.toml`:

```toml
[renew_intent]
enabled = true                  # master switch
min_interval_seconds = 60       # per-sandbox renewal cooldown

[renew_intent.redis]
enabled = true                  # consume ingress renew-intents (queue mode)
dsn = "redis://127.0.0.1:6379/0"
# queue_key = "opensandbox:renew:intent"
# consumer_concurrency = 8
```

`[renew_intent] redis` is only needed for ingress-gateway mode. Server-proxy mode (SDK `useServerProxy`) works with `enabled = true` alone. See [`[renew_intent]` in the server configuration reference](https://github.com/opensandbox-group/OpenSandbox/blob/main/server/configuration.md) for all keys.

## Step 2 — Enable the ingress gateway (queue mode)

```text
--renew-intent-enabled=true
--renew-intent-redis-dsn=redis://127.0.0.1:6379/0
```

| Flag | Default | Purpose |
|---|---|---|
| `--renew-intent-enabled` | `false` | Publish renew intents while proxying |
| `--renew-intent-redis-dsn` | `redis://127.0.0.1:6379/0` | Redis for the intent queue |
| `--renew-intent-queue-key` | `opensandbox:renew:intent` | Redis list key |
| `--renew-intent-queue-max-len` | `0` (uncapped) | Queue length cap |
| `--renew-intent-min-interval` | `60s` | Per-sandbox client-side publish throttle |

Publishing is async and best-effort: it never adds latency to proxied requests, and dropped intents only mean a missed renewal.

## Step 3 — Declare the policy on the sandbox

Pass `lifecycle.renewalPolicy.extendSeconds` (300–86400) in the create request.

::: code-group

```python Python
from datetime import timedelta

from opensandbox import Sandbox
from opensandbox.models import RenewalPolicy, SandboxLifecycle

sandbox = await Sandbox.create(
    "ubuntu:24.04",
    connection_config=config,
    timeout=timedelta(seconds=3600),
    lifecycle=SandboxLifecycle(
        renewal_policy=RenewalPolicy(extend_seconds=3600),
    ),
)
```

```javascript JavaScript
import { Sandbox } from "@alibaba-group/opensandbox";

const sandbox = await Sandbox.create({
  connectionConfig: { domain: "http://127.0.0.1:8080" },
  image: "ubuntu:24.04",
  timeoutSeconds: 3600,
  lifecycle: {
    renewalPolicy: { extendSeconds: 3600 },
  },
});
```

```go Go
ttl := 3600
info, err := opensandbox.CreateSandbox(ctx, cfg, opensandbox.SandboxCreateOptions{
    Image:           "ubuntu:24.04",
    TimeoutSeconds:  &ttl,
    Lifecycle: &opensandbox.SandboxLifecycle{
        RenewalPolicy: &opensandbox.RenewalPolicy{ExtendSeconds: 3600},
    },
})
```

```csharp C#
var sandbox = await OpenSandbox.Sandbox.CreateAsync(new SandboxCreateOptions
{
    Image = "ubuntu:24.04",
    TimeoutSeconds = 3600,
    Lifecycle = new SandboxLifecycle
    {
        RenewalPolicy = new RenewalPolicy { ExtendSeconds = 3600 },
    },
});
```

```kotlin Kotlin
val sandbox = Sandbox.builder()
    .image("ubuntu:24.04")
    .timeout(Duration.ofSeconds(3600))
    .lifecycle(
        SandboxLifecycle.builder()
            .renewalPolicy { extendSeconds(3600) }
            .build()
    )
    .build()
```

:::

Each renewal sets the expiration to `max(now + extendSeconds, current expiresAt)` — renewals never shorten a sandbox's life, and the expiration remains visible via the normal sandbox info / `expiresAt` response field. Manual `POST /sandboxes/{sandboxId}/renew-expiration` keeps working and composes with the policy.

## Backend support

| Backend | `lifecycle.renewalPolicy` | Renewal actually fires |
|---|---|---|
| Docker | accepted (persisted as a container label) | yes |
| Kubernetes (pod) | accepted (persisted as an annotation) | yes, including `extensions.poolRef` pool mode |
| Fast Sandbox (fsb) | **rejected with 400** | no — fsb does not support renew-on-access yet |

The field is control-plane state: it never reaches the guest and may accompany `extensions.poolRef`.

## Deprecated extensions key

Before this field existed, opt-in used the opaque creation key `extensions["access.renew.extend.seconds"]` (a decimal string with the same 300–86400 bounds).

::: warning Deprecation timeline
The extensions key is **deprecated** in favor of `lifecycle.renewalPolicy`. It remains supported throughout the entire **1.x** series and is **removed in 2.0**. New code should use `lifecycle.renewalPolicy`; existing SDKs, scripts, and integrations that still send the extensions key keep working on 1.x.
:::

Transition rules while both are accepted:

- Either input on its own works; both inputs with the same parsed value work.
- Both inputs with **conflicting values are rejected with HTTP 400** — values are compared after parsing, so `"3600"` and `" 3600 "` are equivalent.
- Both inputs normalize to the same persisted metadata (container label / workload annotation), so sandboxes created before a server upgrade keep renewing after the upgrade.

A single request can opt out of renew-intent publishing with the `OpenSandbox-Access-Renew: skip` request header through the ingress gateway.
