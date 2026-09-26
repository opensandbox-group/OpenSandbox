---
title: Client Pool Resize
description: Keep a Go sandbox pool's idle target on a local wall-clock schedule with a time-window resize policy.
---

# Client Pool Resize

Size a Go sandbox pool on a local wall-clock schedule. `Resize` changes the idle
target; deciding *when* to call it is a policy decision, so this example keeps the
policy outside the pool reconciler and applies it through the existing `Resize`
method.

Source: [`examples/client-pool-resize`](https://github.com/opensandbox-group/OpenSandbox/tree/main/examples/client-pool-resize)

## What it does

The example declares two windows and one default:

| Window | Local time | Idle target |
|--------|------------|-------------|
| `overnight` | 22:00 – 06:00 (wraps midnight) | 0 |
| `business-hours` | 09:00 – 18:00, Mon–Fri | 6 |
| default | everything else, including weekends | 2 |

`PoolResizeAdapter` re-evaluates the policy every minute and calls `Resize` with
the resulting target. The pool's own reconciler still performs warmup and
idle-only shrink, so lowering the target never kills a borrowed sandbox.

## Prerequisites

- A reachable OpenSandbox control plane
- Go 1.20 or newer

## Run

```bash
cd examples/client-pool-resize

export OPEN_SANDBOX_DOMAIN=api.opensandbox.io
export OPEN_SANDBOX_API_KEY=...

go run .
```

Optional environment variables:

| Variable | Default | Description |
|----------|---------|-------------|
| `OPEN_SANDBOX_DOMAIN` | — (required) | Control-plane domain |
| `OPEN_SANDBOX_API_KEY` | empty | API key, when the control plane requires one |
| `OPEN_SANDBOX_POOL_NAME` | `resize-demo` | Pool namespace |
| `OPEN_SANDBOX_OWNER_ID` | `resize-example` | Owner ID recorded in the state store |
| `OPEN_SANDBOX_TIMEZONE` | `UTC` | Timezone the windows are expressed in |

::: tip
Use a named timezone such as `America/Los_Angeles` in production. `UTC` is the
default only so the example is reproducible anywhere.
:::

The process logs the applied target and the current idle count every 30 seconds.
Press `Ctrl-C` to stop; the policy loop is cancelled and the pool is shut down
gracefully before exit.

## Notes

- Start the pool before calling `Apply` or `Run`; otherwise they return
  `ErrPoolResizePoolNotRunning`.
- The example uses an in-memory state store, so the schedule is owned by this one
  process. To share a namespace across processes, use a shared state store
  (Redis) and rotate the `PoolName` when the schedule itself changes.
- See the [Client Pool guide](/guides/client-pool#time-window-sizing-go) for
  timezone, overnight-window, and multi-process details.
