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

The pool does **not** pool SDK `Sandbox` objects. It pools the **IDs of
pre-warmed, ready sandboxes** running on the OpenSandbox server.

The **Kotlin/Java** SDK additionally gives each `SandboxPool` a pool-wide
shared HTTP connection pool. When the pool's `ConnectionConfig` carries no
custom `connectionPool`, the pool creates one sized by `warmup_concurrency`
(5-minute keep-alive) and uses it for every sandbox it creates — warmup,
direct create, and idle connect — so concurrent warmups reuse TCP connections
instead of each opening fresh ones. At high `warmup_concurrency`, per-sandbox
connection churn otherwise causes intermittent connection resets and retry
amplification. The pool evicts its shared pool on shutdown; a user-provided
pool is never touched. Python and Go pools do not share HTTP connections
across sandboxes today.

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

The existing cleanup methods retain their original execution behavior. For opt-in
bounded parallel cleanup, use Python's
`release_all_idle_parallel(max_workers=50)`, Kotlin's
`releaseAllIdle(concurrency)`, or Go's concrete
`(*DefaultSandboxPool).ReleaseAllIdleParallel(ctx, maxWorkers)`. These methods
validate a positive concurrency value and wait for every drained ID to receive a
best-effort kill attempt. The Go method is intentionally outside the
`SandboxPool` interface to preserve compatibility with third-party implementors.

### Tracing warmups (Kotlin)

The Kotlin SDK can emit an OpenTelemetry trace per warmup task (`pool.warmup`
root span plus `create` / `prepare` / `renew` / `commit` phases) when
`ConnectionConfig.enableTracing(true)` is set and an OpenTelemetry SDK +
exporter is on the classpath. `trace_id` / `span_id` are published to the
SLF4J MDC, so search your logs for a `sandbox_id` to find the warmup trace and
drill into phase durations. See [SDK Tracing (Pool Warmup)](/guides/sdk-tracing).

### Retiring an old pool namespace

Every SDK exposes a `SandboxPoolManager` with a `destroy` operation that applies the
same `DESTROYING → DESTROYED` protocol:

1. Write a `DESTROYING` fence into the state store, so any still-running peer instance
   sees it and stops replenishing instead of racing the retirement.
2. Best-effort drain and kill every idle sandbox, bounded by the drain timeout.
3. Clear the persistent per-pool state.
4. Write a `DESTROYED` tombstone with the tombstone TTL (default 7 days) so future
   callers cannot silently rebind to the same `pool_name`.

Destroy is idempotent: calling it on an already-tombstoned namespace reports
`DESTROYED` without draining or killing anything. If the drain or the cleanup cannot
finish, the namespace stays `DESTROYING` and the call reports the destroy as
incomplete; retrying is safe and picks up where it left off.

**Python / Kotlin** — `SandboxPoolManager.destroy(poolName, options)`, configured
through `PoolDestroyOptions` (`strategy`, `drain_timeout`, `tombstone_ttl`).

**Go** — `(*SandboxPoolManager).Destroy(ctx, poolName, options)`:

```go
manager, err := opensandbox.NewSandboxPoolManagerBuilder().
    StateStore(store).
    ConnectionConfig(connCfg).
    Build()
if err != nil {
    return err
}

result, err := manager.Destroy(ctx, "orders-v2", opensandbox.PoolDestroyOptions{})
if err != nil {
    return err
}
log.Printf("retired %s: drained=%d killed=%d",
    result.PoolName, result.DrainedIdleCount, result.KilledIdleCount)
```

`PoolDestroyOptions` mirrors the other SDKs. `Strategy` selects the algorithm and only
`PoolDestroyForce` is implemented. `DrainTimeout` and `TombstoneTTL` are `*time.Duration`:
leave them nil for the defaults (30s and 7 days), or set an explicit zero to drain
without a deadline and to write a tombstone that never expires.

The fence is what makes retirement safe without stopping every writer first, and it
is enforced on two levels. The state store refuses `PutIdle`, `SetMaxIdle` and
`SetIdleEntryTTL` with a `*PoolDestroyedError` and hands out no primary lock, which
stops replenishment. The pool itself also checks the fence when it starts, before every
acquire, again once an acquire holds a live sandbox, and on each reconcile tick: a
surviving peer stops outright on its next tick, an in-flight acquire fails rather
than minting a fresh sandbox into the retired namespace through the direct-create
fallthrough, and a sandbox obtained just before the fence landed is killed instead
of handed out. The post-acquire check matters because the idle take is deliberately
left unfenced so `destroy` can drain: once an ID has been taken, `destroy` can no
longer reach it, so the acquire has to dispose of it itself.
Starting a fresh pool against a tombstoned `PoolName` fails for the same reason, so
rebinding the name requires either waiting out the tombstone TTL or rotating to a new
`PoolName`.

One deliberate exception: if the state store itself is unreachable, the destroy state
is unknowable, so policies that already fall through to direct create on a store
outage (`DIRECT_CREATE`, `RETRY_NEXT_IDLE_THEN_CREATE`) assume `ACTIVE` and proceed,
matching the existing `try_take_idle` outage behavior in the OSEP-0005 error-code
matrix. `FAIL_FAST` and `RETRY_NEXT_IDLE` surface the outage instead. That relaxation
stops at a sandbox already taken from the idle buffer: there the check is fail-closed
and an unreachable store means the sandbox is killed, because nothing else is tracking
it any more.

## Further reading

- Python: [`/sdks/python`](/sdks/python) &mdash; `SandboxPoolSync`, `SandboxPoolAsync`, Redis store.
- Kotlin: [`/sdks/kotlin`](/sdks/kotlin) &mdash; `SandboxPool` builder, `sandbox-pool-redis` module.
- Go: [`/sdks/go`](/sdks/go) &mdash; `SandboxPool` interface, `RedisPoolStateStore`, distributed deployment notes.
