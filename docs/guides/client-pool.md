---
title: Client Pool
description: How the SDK-side sandbox pool works, how to configure it, and a minimal example for each supported SDK.
---

# Client Pool

The OpenSandbox SDKs ship an experimental **client-side sandbox pool** that keeps a small
buffer of ready sandboxes warm on the server so that `acquire()` returns quickly instead
of paying the full sandbox creation latency on the hot path.

Available in the Python, Kotlin/Java, and Go sandbox SDKs. The JavaScript/TypeScript and
C# SDKs do not currently ship a client pool.

::: warning Experimental
The client pool API is marked experimental and may change between minor releases. Pin
your SDK version if you rely on it in production.
:::

## What it actually pools

The pool does **not** pool HTTP connections, and it does **not** pool SDK `Sandbox`
objects. It pools the **IDs of pre-warmed, ready sandboxes** running on the OpenSandbox
server.

![Client pool architecture](/images/client-pool-architecture.svg)

Two flows happen concurrently:

- **Warmup (leader-only).** A background reconcile loop runs on every node at
  `reconcile_interval`. Whichever node holds the primary lock in the state store fans
  new-sandbox creation out onto a small warmup worker pool (`warmup_concurrency`),
  optionally runs a readiness check, then publishes the ready sandbox ID into the idle
  buffer with a TTL of `idle_timeout`.
- **Acquire (any node).** `acquire()` pops an idle ID from the store, connects a
  `Sandbox` client to it, optionally runs a health check and a `renew()` to the
  caller-supplied timeout, and hands it to the caller. Non-leader nodes can acquire
  freely; only replenish and shrink are gated by the leader lock.

The store carries only sandbox IDs and their expiry — no HTTP state, no client-side
objects. That is what lets Redis-backed pools be truly distributed across processes
and pods.

The warmup path — the leader-only replenish flow above — is worth zooming in on
because it is the only part of the pool that is gated by a distributed lock:

![Warmup reconcile sequence](/images/client-pool-warmup-sequence.svg)

### Lifecycle model

Each pool instance moves through `NOT_STARTED → STARTING → RUNNING → DRAINING → STOPPED`.
Health is tracked separately as `HEALTHY | DEGRADED | DRAINING | STOPPED`; after
`degraded_threshold` consecutive create failures the pool enters `DEGRADED` and applies
exponential backoff before retrying warmup. Callers do not need to observe these states
directly — `snapshot()` exposes them for diagnostics.

![Client pool lifecycle state machine](/images/client-pool-lifecycle.svg)

### There is no `release()`

Sandboxes are ephemeral. Once you have called `acquire()`, the sandbox is yours until you
`destroy()` / `kill()` it. `max_idle` bounds the **warm buffer**, not the number of
sandboxes borrowed by application code and not the number of sandboxes produced by
`DIRECT_CREATE` fallback.

## Empty-buffer behavior: `AcquirePolicy`

`AcquirePolicy` controls what happens when the idle buffer is empty, or when the first
idle candidate fails its readiness check:

| Policy                    | Fallback on exhaustion                     |
| ------------------------- | ------------------------------------------ |
| `FAIL_FAST`               | raise `PoolEmptyException` / `PoolAcquireFailedException` |
| `DIRECT_CREATE` (default) | create a new sandbox via the lifecycle API |

Under both policies `acquire()` tries **one** idle candidate. If that candidate fails
its readiness check, `FAIL_FAST` raises and `DIRECT_CREATE` falls back to creating a
brand-new sandbox via the lifecycle API. A failed candidate still pays up to
`acquire_ready_timeout`.

![Acquire decision flow](/images/client-pool-acquire-decision.svg)

## Configuration

All three SDKs expose the same knobs with the same defaults. This table is the
canonical reference; refer to the per-language builder or constructor for exact naming.

| Parameter                             | Default                            | Meaning                                                                          |
| ------------------------------------- | ---------------------------------- | -------------------------------------------------------------------------------- |
| `pool_name`                           | required                           | Logical namespace shared by all nodes of one distributed pool                    |
| `owner_id`                            | auto (`pool-owner-<uuid/host/pid>`) | Identity of this process for primary-lock ownership; **must be unique per node** |
| `max_idle`                            | required (≥ 0)                     | Target size and cap of the idle buffer                                           |
| `state_store`                         | required (Go builder defaults to in-memory) | `InMemoryPoolStateStore` or Redis-backed store                          |
| `connection_config`                   | required                           | Used for lifecycle and execd calls                                               |
| `creation_spec`                       | required in Python / Kotlin; required in Go only when `sandbox_creator` is unset | Template for warmed sandboxes: `image`, `entrypoint`, `env`, `metadata`, `extensions`, `resource`, `network_policy`, `platform`, `volumes`, `secure_access` |
| `sandbox_creator`                     | `null`                             | Optional callback that overrides `creation_spec` at runtime. Python and Kotlin still require `creation_spec` to be supplied (it is unused when the creator is set); only Go allows a creator-only pool. Receives a `PooledSandboxCreateContext` whose `reason` field is `WARMUP` for warmup and `DIRECT_CREATE` (Python / Kotlin) or `CreateReasonAcquire` / `"ACQUIRE"` (Go) for the acquire-fallback path. |
| `warmup_concurrency`                  | `max(1, ceil(max_idle * 0.2))`     | Warmup worker pool size                                                          |
| `primary_lock_ttl`                    | `60 s`                             | Leader-lock TTL; must exceed `warmup_ready_timeout` + preparer time              |
| `reconcile_interval`                  | `30 s`                             | Interval between reconcile ticks                                                 |
| `degraded_threshold`                  | `3`                                | Consecutive create failures before entering `DEGRADED` with backoff              |
| `acquire_ready_timeout`               | `30 s`                             | Max wait for the returned sandbox to become ready                                |
| `acquire_health_check_polling_interval` | `200 ms`                         | Ready-poll interval during acquire                                               |
| `acquire_health_check`                | `null`                             | Custom readiness predicate for acquire                                           |
| `acquire_skip_health_check`           | `false`                            | Skip the readiness check on acquire                                              |
| `acquire_min_remaining_ttl`           | `min(60 s, idle_timeout / 2)`      | Discard idles closer to expiry than this on acquire                              |
| `warmup_ready_timeout`                | `30 s`                             | Max wait for a warmed sandbox to become ready                                    |
| `warmup_health_check_polling_interval` | `200 ms`                          | Ready-poll interval during warmup                                                |
| `warmup_health_check`                 | `null`                             | Custom warmup readiness predicate                                                |
| `warmup_sandbox_preparer`             | `null`                             | Runs after readiness and before publishing to the idle buffer                    |
| `warmup_skip_health_check`            | `false`                            | Skip readiness check during warmup                                               |
| `idle_timeout`                        | `24 h`                             | Server-side TTL for pool-created sandboxes                                       |
| `drain_timeout`                       | `30 s`                             | Max wait for in-flight ops during graceful shutdown                              |

### Choosing a state store

- **`InMemoryPoolStateStore`** — single process only. Suitable for development, tests,
  and single-instance workers. Not process-wide for gunicorn/uvicorn workers, Celery, or
  Kubernetes replicas.
- **Redis-backed store** (`RedisPoolStateStore`, `AsyncRedisPoolStateStore`,
  `sandbox-pool-redis` on the JVM, `poolredis` in Go) — required for multi-process or
  multi-pod deployments. All nodes in one logical pool must share the same `pool_name`
  and Redis `key_prefix`, and each process must use a **unique** `owner_id`.

![Single-node vs distributed pool topology](/images/client-pool-topology.svg)

### Rules that apply to every deployment

- `max_idle` bounds the warm buffer only. It does not cap borrowed sandboxes or
  `DIRECT_CREATE` fallbacks.
- All nodes sharing one pool must use the same creation and warmup definition. If that
  definition changes, roll out under a **new** `pool_name` (or Redis `key_prefix`) and
  retire the old one (see "Retiring an old pool namespace" below). Do not attempt to
  refill a changed template into the same `pool_name`: `release_all_idle()` does not
  fence other nodes, does not lower `max_idle`, and does not stop any current leader
  (which may still be running the old code) from immediately re-publishing
  old-template sandbox IDs into the shared buffer during a rolling deploy.
- `resize(max_idle)` and `release_all_idle()` can be called from any node.

## Minimal usage

### Python (sync)

```python
from datetime import timedelta

from opensandbox import (
    AcquirePolicy,
    InMemoryPoolStateStore,
    PoolCreationSpec,
    SandboxPoolSync,
)
from opensandbox.config import ConnectionConfigSync

pool = SandboxPoolSync(
    pool_name="demo-pool",
    owner_id="worker-1",
    max_idle=2,
    state_store=InMemoryPoolStateStore(),
    connection_config=ConnectionConfigSync(domain="api.opensandbox.io"),
    creation_spec=PoolCreationSpec(image="ubuntu:22.04"),
    reconcile_interval=timedelta(seconds=5),
)

pool.start()
try:
    sandbox = pool.acquire(
        sandbox_timeout=timedelta(minutes=30),
        policy=AcquirePolicy.FAIL_FAST,
    )
    try:
        result = sandbox.commands.run("echo pool-ok")
        print(result.logs.stdout[0].text)
    finally:
        sandbox.destroy()
finally:
    pool.shutdown(graceful=True)
```

### Python (asyncio)

`SandboxPoolAsync` has the same surface plus an `async with` context manager:

```python
from datetime import timedelta

from opensandbox import (
    AcquirePolicy,
    InMemoryAsyncPoolStateStore,
    PoolCreationSpec,
    SandboxPoolAsync,
)
from opensandbox.config import ConnectionConfig

async with SandboxPoolAsync(
    pool_name="demo-pool",
    owner_id="worker-1",
    max_idle=2,
    state_store=InMemoryAsyncPoolStateStore(),
    connection_config=ConnectionConfig(domain="api.opensandbox.io"),
    creation_spec=PoolCreationSpec(image="ubuntu:22.04"),
) as pool:
    sandbox = await pool.acquire(
        sandbox_timeout=timedelta(minutes=30),
        policy=AcquirePolicy.FAIL_FAST,
    )
    try:
        result = await sandbox.commands.run("echo pool-ok")
    finally:
        await sandbox.destroy()
```

### Kotlin / Java

```java
SandboxPool pool = SandboxPool.builder()
    .poolName("demo-pool")
    .ownerId("worker-1")
    .maxIdle(3)
    .stateStore(new InMemoryPoolStateStore())
    .connectionConfig(config)
    .creationSpec(PoolCreationSpec.builder()
        .image("ubuntu:22.04")
        .entrypoint(List.of("tail", "-f", "/dev/null"))
        .build())
    .warmupReadyTimeout(Duration.ofSeconds(45))
    .build();

pool.start();
try {
    Sandbox sb = pool.acquire(Duration.ofMinutes(10), AcquirePolicy.FAIL_FAST);
    try {
        sb.commands().run("echo pool-ok");
    } finally {
        sb.kill();
        sb.close();
    }
} finally {
    pool.shutdown(true);
}
```

### Go

```go
pool, err := opensandbox.NewSandboxPoolBuilder().
    PoolName("demo-pool").
    OwnerID("worker-1").
    MaxIdle(3).
    ConnectionConfig(opensandbox.ConnectionConfig{Domain: "api.opensandbox.io"}).
    CreationSpec(opensandbox.PoolCreationSpec{Image: "ubuntu:22.04"}).
    StateStore(opensandbox.NewInMemoryPoolStateStore()).
    Build()
if err != nil {
    log.Fatal(err)
}
if err := pool.Start(ctx); err != nil {
    log.Fatal(err)
}
defer pool.Shutdown(context.Background(), true)

failFast := opensandbox.AcquirePolicyFailFast
sb, err := pool.Acquire(ctx, opensandbox.AcquireOptions{
    SandboxTimeout: 10 * time.Minute,
    Policy:         &failFast,
})
if err != nil {
    log.Fatal(err)
}
defer sb.Kill(context.Background())

result, _ := sb.RunCommand(ctx, "echo pool-ok", nil)
_ = result
```

## Diagnostics

Every SDK exposes read-only accessors:

- `snapshot()` — pool phase, health, counters (idle size, in-flight warmups,
  consecutive failures, last error).
- `snapshot_idle_entries()` — the current idle sandbox IDs with expiry timestamps.
- `resize(max_idle)` — change the target buffer size at runtime.
- `release_all_idle()` — drain the currently visible idle buffer and best-effort kill
  each entry, without stopping the pool. Useful to force a fresh set of warmups after a
  transient upstream problem. It does **not** change `max_idle`, does **not** fence
  other nodes, and does **not** stop an active leader from immediately replenishing —
  so it is not a safe way to swap creation templates on the same `pool_name`. For that
  case, retire the whole namespace under a new `pool_name` (see below).

### Retiring an old pool namespace

The retirement procedure differs across SDKs because Go does not currently ship a
`SandboxPoolManager` or the destroy / tombstone primitives that Python and Kotlin
have.

**Python / Kotlin** — use `SandboxPoolManager.destroy(poolName, options)`. The manager
applies a full `DESTROYING → DESTROYED` protocol: write a `DESTROYING` fence into the
state store (so any still-running peer instance sees it), best-effort drain and kill
every idle sandbox up to `drain_timeout`, clear the persistent per-pool state, then
write a `DESTROYED` tombstone with `tombstone_ttl` (default 7 days) so future callers
cannot silently rebind to the same `pool_name`.

**Go** — the Go SDK has no equivalent API and no state-store primitives for
tombstones or fences. The closest safe approximation is an operator-driven, out-of-band
sequence:

1. Stop every process that instantiates a pool against the old `pool_name`. Call
   `pool.Shutdown(ctx, true)` on each. This releases each node's primary lock but
   leaves idle entries in the store.
2. From one still-alive pool instance (or a throwaway one bound to the same
   `PoolName` + `StateStore`), call `pool.ReleaseAllIdle(ctx)` to drain and
   best-effort kill every idle sandbox.
3. Set `store.SetMaxIdle(ctx, poolName, 0)` so any peer that races back in cannot
   warm up new sandboxes.
4. Move all future callers to a new `PoolName` (for example
   `orders-v2` → `orders-v3`). This is the Go substitute for the `DESTROYED`
   tombstone: without a shared marker, name rotation is the only way to guarantee no
   accidental reuse.
5. If you are using the Redis store and want to reclaim keys, delete them directly
   with `DEL` / `SCAN` against your Redis instance — the Go SDK does not expose a
   destroy helper for this.

Without a fence, steps 2 and 3 race with any surviving peer that has not yet been
stopped. If you cannot guarantee "all writers stopped" before step 2, the only correct
option is to rotate `PoolName` first (step 4) and let the old namespace's idle entries
expire naturally via `idle_timeout`.

## Further reading

- Python: [`/sdks/python`](/sdks/python) &mdash; `SandboxPoolSync`, `SandboxPoolAsync`, Redis store.
- Kotlin: [`/sdks/kotlin`](/sdks/kotlin) &mdash; `SandboxPool` builder, `sandbox-pool-redis` module.
- Go: [`/sdks/go`](/sdks/go) &mdash; `SandboxPool` interface, `RedisPoolStateStore`, distributed deployment notes.
