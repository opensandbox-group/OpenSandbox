# OpenSandbox Kotlin Redis Pool Store

Redis-backed `PoolStateStore` implementation for distributed `SandboxPool` coordination.

## Installation

Use this module only when multiple SDK processes need to share one sandbox pool through Redis.

```kotlin
dependencies {
    implementation("com.alibaba.opensandbox:sandbox:{latest_version}")
    implementation("com.alibaba.opensandbox:sandbox-pool-redis:{latest_version}")
}
```

## Usage

Create and configure the Jedis client yourself, then pass it to `RedisPoolStateStore`.
The store does not create, configure, or close the Redis client.
The client must be safe for concurrent use because pool operations may call Redis from multiple
threads. `JedisPooled` is recommended; do not share a single non-pooled Jedis connection across
pool threads.

```java
import com.alibaba.opensandbox.sandbox.pool.SandboxPool;
import com.alibaba.opensandbox.sandbox.domain.pool.PoolCreationSpec;
import com.alibaba.opensandbox.sandbox.infrastructure.pool.RedisPoolStateStore;
import redis.clients.jedis.JedisPooled;

JedisPooled redis = new JedisPooled("redis://user:password@redis.example.com:6379/0");

RedisPoolStateStore store = new RedisPoolStateStore(
    redis,
    "opensandbox:pool:prod"
);

SandboxPool pool = SandboxPool.builder()
    .poolName("demo-pool")
    .ownerId("worker-1")
    .maxIdle(10)
    .stateStore(store)
    .connectionConfig(config)
    .creationSpec(
        PoolCreationSpec.builder()
            .image("ubuntu:22.04")
            .build()
    )
    .build();

try {
    pool.start();
    // acquire and use sandboxes
} finally {
    pool.shutdown(true);
    redis.close();
}
```

## Notes

- `RedisPoolStateStore` supports standalone Redis or Redis-compatible proxy endpoints.
- Redis Cluster and Redis Sentinel clients are not supported by this store.
- All nodes in the same logical pool must use the same `keyPrefix` and `poolName`.
- All nodes sharing the same `keyPrefix` and `poolName` must use the same pool definition, including
  sandbox creation spec, warmup behavior, idle timeout, and OpenSandbox service endpoint. If a deployment
  changes the sandbox definition, use a new `poolName` or `keyPrefix` and drain the old pool.
- Each process must use a unique `ownerId`.
- `maxIdle` is the target/cap for ready idle sandboxes. It is not a global limit
  on borrowed sandboxes or sandboxes created by `AcquirePolicy.DIRECT_CREATE`.
- Configure Redis connection details, TLS, ACL, timeout, pooling, and monitoring through Jedis.
- Pass a thread-safe Jedis client. `JedisPooled` is recommended for production use.
- `resize(maxIdle)` can be called from any node. The call returns after the target is written to
  Redis, and the current primary applies replenish or shrink work during periodic reconcile.
  `resize(0)` is the recommended way
  to drain the distributed idle buffer; wait until `snapshot().idleCount` converges to zero.
- `releaseAllIdle()` is best-effort in distributed mode. It drains idle IDs visible during the call,
  but a concurrent primary may put new idle sandboxes unless the shared `maxIdle` target has been
  reduced first.
- Configure `primaryLockTtl` greater than `warmupReadyTimeout` plus the expected
  `warmupSandboxPreparer` duration and operational buffer. If warmup takes longer than
  the primary lock TTL, another node may take leadership while the old leader discards
  the sandboxes it created.
- Redis outages are surfaced as `PoolStateStoreUnavailableException`; the pool does not silently bypass shared state.

TODO: If production deployments need stronger protection against accidental mixed pool definitions,
add an optional pool definition version or fingerprint check that fails fast when nodes sharing the
same Redis namespace disagree.
