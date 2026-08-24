---
title: Kotlin/Java SDK
description: Kotlin SDK for creating, managing, and interacting with secure OpenSandbox environments.
---

# OpenSandbox SDK for Kotlin

A Kotlin SDK for low-level interaction with OpenSandbox. It provides capabilities to create, manage, and interact with secure sandbox environments, including executing shell commands, managing files, and monitoring resources.

## Installation

### Gradle (Kotlin DSL)

```kotlin
dependencies {
    implementation("com.alibaba.opensandbox:sandbox:{latest_version}")
}
```

### Maven

```xml
<dependency>
    <groupId>com.alibaba.opensandbox</groupId>
    <artifactId>sandbox</artifactId>
    <version>{latest_version}</version>
</dependency>
```

## Quick Start

The following example shows how to create a sandbox and execute a shell command.

::: tip
Before running this example, ensure the OpenSandbox service is running. See the [Getting Started](/getting-started/) guide for startup instructions.
:::

```java
import com.alibaba.opensandbox.sandbox.Sandbox;
import com.alibaba.opensandbox.sandbox.config.ConnectionConfig;
import com.alibaba.opensandbox.sandbox.domain.exceptions.SandboxException;
import com.alibaba.opensandbox.sandbox.domain.models.execd.executions.Execution;

public class QuickStart {
    public static void main(String[] args) {
        // 1. Configure connection
        ConnectionConfig config = ConnectionConfig.builder()
            .domain("api.opensandbox.io")
            .apiKey("your-api-key")
            .build();

        // 2. Create a Sandbox using try-with-resources
        try (Sandbox sandbox = Sandbox.builder()
                .connectionConfig(config)
                .image("ubuntu")
                .build()) {

            // 3. Execute a shell command
            Execution execution = sandbox
                    .commands()
                    .run("echo 'Hello Sandbox!'");

            // 4. Print output
            System.out.println(execution.getLogs().getStdout().get(0).getText());

            // 5. Cleanup (sandbox.close() called automatically)
            // Note: kill() must be called explicitly if you want to terminate the remote sandbox instance immediately
            sandbox.kill();
        } catch (SandboxException e) {
            // Handle Sandbox specific exceptions
            System.err.println("Sandbox Error: [" + e.getError().getCode() + "] " + e.getError().getMessage());
            System.err.println("Request ID: " + e.getRequestId());
        } catch (Exception e) {
            e.printStackTrace();
        }
    }
}
```

## Usage Examples

### 1. Lifecycle Management

Manage the sandbox lifecycle, including renewal, pausing, and resuming.

```java
// Renew the sandbox
// This resets the expiration time to (current time + duration)
sandbox.renew(Duration.ofMinutes(30));

// Pause execution (suspends all processes)
sandbox.pause();

// Resume execution
sandbox.resume();

// Get current status
SandboxInfo info = sandbox.getInfo();
System.out.println("State: " + info.getStatus().getState());
System.out.println("Expires: " + info.getExpiresAt()); // null when manual cleanup mode is used
```

Create a non-expiring sandbox by passing `timeout(null)`:

```java
Sandbox manual = Sandbox.builder()
    .connectionConfig(config)
    .image("ubuntu")
    .timeout(null)
    .build();
```

### 2. Custom Health Check

Define custom logic to determine if the sandbox is healthy. This overrides the default ping check.

```java
Sandbox sandbox = Sandbox.builder()
    .connectionConfig(config)
    .image("nginx:latest")
    // Custom check: Wait for port 80 to be accessible
    .healthCheck(sbx -> {
        try {
            // 1. Get the external mapped address for port 80
            SandboxEndpoint endpoint = sbx.getEndpoint(80);

            // 2. Perform your connection check (e.g. HTTP request, Socket connect)
            // return checkConnection(endpoint.getEndpoint());
            return true;
        } catch (Exception e) {
            return false;
        }
    })
    .build();
```

### 3. Command Execution & Streaming

Execute commands and handle output streams in real-time.

```java
// Create handlers for streaming output
ExecutionHandlers handlers = ExecutionHandlers.builder()
    .onStdout(msg -> System.out.println("STDOUT: " + msg.getText()))
    .onStderr(msg -> System.err.println("STDERR: " + msg.getText()))
    .onExecutionComplete(complete ->
        System.out.println("Command finished in " + complete.getExecutionTimeInMillis() + "ms")
    )
    .build();

// Execute command with handlers
RunCommandRequest request = RunCommandRequest.builder()
    .command("for i in {1..5}; do echo \"Count $i\"; sleep 0.5; done")
    .handlers(handlers)
    .build();

sandbox.commands().run(request);
```

### 4. Comprehensive File Operations

Manage files and directories, including read, write, list, delete, and search.

```java
// 1. Write file
sandbox.files().write(List.of(
    WriteEntry.builder()
        .path("/tmp/hello.txt")
        .data("Hello World")
        .mode(644)
        .build()
));

// 2. Read file
String content = sandbox.files().readFile("/tmp/hello.txt", "UTF-8", null);
System.out.println("Content: " + content);

// 3. List/Search files
List<EntryInfo> files = sandbox.files().search(
    SearchEntry.builder()
        .path("/tmp")
        .pattern("*.txt")
        .build()
);
files.forEach(f -> System.out.println("Found: " + f.getPath()));

// 4. Delete file
sandbox.files().deleteFiles(List.of("/tmp/hello.txt"));
```

### 5. Sandbox Management (Admin)

Use `SandboxManager` for administrative tasks and finding existing sandboxes.

```java
SandboxManager manager = SandboxManager.builder()
    .connectionConfig(config)
    .build();

import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SandboxState;

// ...

// List running sandboxes
PagedSandboxInfos sandboxes = manager.listSandboxInfos(
    SandboxFilter.builder()
        .states(SandboxState.RUNNING)
        .pageSize(10)
        .page(1)
        .build()
);

sandboxes.getSandboxInfos().forEach(info -> {
    System.out.println("Found sandbox: " + info.getId());
    // Perform admin actions
    manager.killSandbox(info.getId());
});

// Try-with-resources will automatically call manager.close()
// manager.close();
```

### 6. Sandbox Pool (Client-Side)

Use `SandboxPool` to keep an idle buffer of ready sandboxes and reduce acquire latency.

::: warning Experimental
`SandboxPool` is still evolving based on production feedback and may introduce breaking changes in future releases.
:::

```java
import com.alibaba.opensandbox.sandbox.pool.SandboxPool;
import com.alibaba.opensandbox.sandbox.pool.SandboxPoolManager;
import com.alibaba.opensandbox.sandbox.domain.pool.PoolCreationSpec;
import com.alibaba.opensandbox.sandbox.domain.pool.PoolDestroyOptions;
import com.alibaba.opensandbox.sandbox.domain.pool.AcquirePolicy;
import com.alibaba.opensandbox.sandbox.infrastructure.pool.InMemoryPoolStateStore;

SandboxPool pool = SandboxPool.builder()
    .poolName("demo-pool")
    .ownerId("worker-1")
    .maxIdle(3)
    .warmupCreateQps(10)
    .warmupConcurrency(128)
    .warmupReadyTimeout(Duration.ofSeconds(45))
    .warmupHealthCheckInitialDelay(Duration.ofSeconds(2))
    .stateStore(new InMemoryPoolStateStore()) // single-node store
    .connectionConfig(config)
    .creationSpec(
        PoolCreationSpec.builder()
            .image("ubuntu:22.04")
            .entrypoint(java.util.List.of("tail", "-f", "/dev/null"))
            .extension("storage.id", "dataset-001")
            .build()
    )
    .build();

pool.start();
Sandbox sb = pool.acquire(Duration.ofMinutes(10), AcquirePolicy.FAIL_FAST);
try {
    sb.commands().run("echo pool-ok");
} finally {
    sb.kill();
    sb.close();
}
pool.shutdown(true);
```

::: warning Staged warmup scheduling
Kotlin reconciles on a fixed one-second cadence; `reconcileInterval(...)` has been
removed. `warmupCreateQps(...)` (default `10`) caps new warmup creates admitted per
tick, while `warmupConcurrency(...)` (default `128`) independently limits concurrent
post-create health-check and prepare work. Built-in warmup creates make one HTTP attempt
and do not honor the normal transport retry policy or a special HTTP-429 throttle. A
custom `PooledSandboxCreator` must use `context.createConnectionConfig` and honor
`context.skipHealthCheck` to preserve those semantics. Direct creates made by
`acquire()` are unchanged.
:::

The post-create pipeline is staged:

1. Create a sandbox without the builder's inline readiness loop.
2. Wait `warmupHealthCheckInitialDelay` (default zero), then check readiness every
   `warmupHealthCheckPollingInterval` (default `500 ms`) until
   `warmupReadyTimeout` (default `30 s`). The deadline receives one final check.
3. Run `warmupSandboxPreparer` once. If `warmupPostPrepareHealthCheck` is configured,
   retry it at the same polling interval until
   `warmupPostPrepareHealthCheckTimeout` (default `30 s`) without rerunning the
   preparer.
4. Renew the sandbox TTL and commit its ID to the idle buffer.

`degradedThreshold` (default `3`) still controls the `HEALTHY → DEGRADED` diagnostic
state, but Kotlin no longer pauses replenish with exponential backoff;
`snapshot().backoffActive` is always `false`.

::: tip AcquirePolicy
`AcquirePolicy` controls what happens when the idle buffer is empty **or** the first idle candidate fails its readiness check:

| Policy | Retry across idles | Fallback on exhaustion |
|---|---|---|
| `FAIL_FAST` | no | throw `PoolEmptyException` / `PoolAcquireFailedException` |
| `DIRECT_CREATE` (default) | no | create a new sandbox via lifecycle API |
| `RETRY_NEXT_IDLE` | up to `maxAcquireRetries` idles | throw |
| `RETRY_NEXT_IDLE_THEN_CREATE` | up to `maxAcquireRetries` idles | create a new sandbox |

Use the `RETRY_NEXT_IDLE*` variants when the pool may contain a mix of healthy and stale idle sandboxes (e.g. custom templates with long cold-start; a network flap left a few unreachable idles). Each failed candidate still pays up to `acquireReadyTimeout`, so bound the retry with `maxAcquireRetries` (default `3`).
:::

Use `SandboxPoolManager` for release or operations workflows that need to destroy an old
pool namespace without constructing the old `SandboxPool` object:

```java
SandboxPoolManager poolManager = SandboxPoolManager.builder()
    .stateStore(redisStore)
    .connectionConfig(config)
    .ownerId("deploy-job-123")
    .build();

poolManager.destroy(
    "old-pool",
    new PoolDestroyOptions()
);
```

::: info Pool Lifecycle Semantics
- `acquire()` is only allowed when pool state is `RUNNING`.
- In `DRAINING` / `STOPPED`, `acquire()` throws `PoolNotRunningException`.
- When a pool namespace is being destroyed or has been destroyed, `acquire()` throws `PoolDestroyedException` and does not fall back to direct create.
- `maxIdle` is the target/cap for ready idle sandboxes. It is not a global limit on borrowed sandboxes or sandboxes created by `AcquirePolicy.DIRECT_CREATE`.
- `ownerId` is the lock owner identity (node/process id), not the pool identifier. If omitted, SDK auto-generates a UUID-based default.
- Use `warmupSandboxPreparer(...)` if you need to prepare a sandbox after warmup readiness succeeds and before it is put into the idle pool. Add `warmupPostPrepareHealthCheck(...)` when the prepared service needs a separate validation window; retries never rerun the preparer.
:::

::: tip Observing warmup performance
To trace the warmup path, enable `ConnectionConfig.builder().enableTracing(true)` and add an
OpenTelemetry SDK + exporter to your application. Each warmup becomes one trace
(`pool.warmup` root span plus `create` / `readiness_check` / `prepare` /
`post_prepare_check` / `renew` / `commit` phases) with
`trace_id` / `span_id` published to the SLF4J MDC, so you can look up a sandbox's
warmup by searching logs for its `sandbox_id`. See [SDK Tracing (Pool Warmup)](/guides/sdk-tracing).
:::

::: tip Distributed Deployment
For distributed deployment, use the optional `com.alibaba.opensandbox:sandbox-pool-redis` module or provide a custom `PoolStateStore` implementation. The Redis module accepts a caller-managed Jedis client, so your application keeps ownership of Redis connection configuration and lifecycle. Nodes sharing the same pool namespace must use the same sandbox creation and warmup definition; use a new `poolName` or namespace when changing that definition. Kotlin renews the primary lease independently of staged warmup work, at an interval no greater than one third of `primaryLockTtl`; a task is discarded if the lease epoch changes before commit.

In distributed mode, `resize(maxIdle)` can be called from any node. The call returns after the target is stored in the shared state store; the current primary applies replenish or shrink work during periodic reconcile. Use `resize(0)` and wait for `snapshot().idleCount == 0` when you need to drain the distributed idle buffer; `releaseAllIdle()` is only a best-effort cleanup pass.

`releaseAllIdle()` preserves serial cleanup. Use `releaseAllIdle(concurrency)` for bounded parallel cleanup. `concurrency` must be positive, and the overload waits for every drained ID to receive a best-effort kill attempt.

`SandboxPoolManager.destroy(poolName)` is a stronger administrative operation: it writes a `DESTROYING` fence, drains visible idle IDs, best-effort kills idle sandboxes, clears persistent pool state, and then writes a `DESTROYED` tombstone for the configured TTL to prevent old nodes from recreating the same pool namespace. If drain or persistent-state cleanup cannot complete, `destroy()` throws `PoolDestroyIncompleteException` and leaves the namespace fenced as `DESTROYING`; retry `destroy()` to finish cleanup.
:::

## Configuration

### 1. Connection Configuration

The `ConnectionConfig` class manages API server connection settings.

| Parameter        | Description                                | Default                      | Environment Variable   |
| ---------------- | ------------------------------------------ | ---------------------------- | ---------------------- |
| `apiKey`         | API Key for authentication                 | Required                     | `OPEN_SANDBOX_API_KEY` |
| `domain`         | The endpoint domain of the sandbox service | Required (or localhost:8080) | `OPEN_SANDBOX_DOMAIN`  |
| `protocol`       | HTTP protocol (http/https)                 | `http`                       | -                      |
| `requestTimeout` | Timeout for API requests                   | 30 seconds                   | -                      |
| `debug`          | Enable debug logging for HTTP requests     | `false`                      | -                      |
| `headers`        | Custom HTTP headers                        | Empty                        | -                      |
| `connectionPool` | Shared OKHttp ConnectionPool               | SDK-created per instance     | -                      |
| `retryPolicy`    | Automatic retry policy for non-streaming requests (see [Automatic retries](#_2-automatic-retries)) | Enabled (`RetryPolicy()`) | -                 |
| `useServerProxy` | Use sandbox server as proxy for execd/endpoint requests (e.g. when client cannot reach the sandbox directly) | `false` | -                      |
| `disableMetrics` | Disable SDK create-latency telemetry (see [SDK Telemetry](/guides/sdk-telemetry)) | `false` | `OPENSANDBOX_DISABLE_METRICS` |
| `enableTracing` | Enable OpenTelemetry tracing for pool warmup (see [SDK Tracing](/guides/sdk-tracing)) | `false` | - |

```java
// 1. Basic configuration
ConnectionConfig config = ConnectionConfig.builder()
    .apiKey("your-key")
    .domain("api.opensandbox.io")
    .requestTimeout(Duration.ofSeconds(60))
    .build();

// 2. Advanced: Shared Connection Pool
// If you create many Sandbox instances, sharing a connection pool is recommended to save resources.
// SDK default keep-alive is 30 seconds for its own pools.
ConnectionPool sharedPool = new ConnectionPool(50, 30, TimeUnit.SECONDS);

ConnectionConfig sharedConfig = ConnectionConfig.builder()
    .apiKey("your-key")
    .domain("api.opensandbox.io")
    .headers(Map.of(
        "X-Custom-Header", "value",
        "X-Request-ID", "trace-123"
    ))
    .connectionPool(sharedPool) // Inject shared pool
    .build();
```

::: tip SDK Telemetry
`Sandbox.builder()...build()` reports create latency to `POST /v1/metrics/events` by default. Call `ConnectionConfig.builder().disableMetrics(true)` or export `OPENSANDBOX_DISABLE_METRICS=1` to opt out. See [SDK Telemetry](/guides/sdk-telemetry).
:::

### 2. Automatic retries

The SDK retries transient failures automatically. `ConnectionConfig` installs a
`RetryInterceptor` (`com.alibaba.opensandbox.sandbox.transport.RetryPolicy`) on the
SDK's non-streaming HTTP clients.

Default behavior:

- **Enabled by default.** Idempotent methods (`GET/HEAD/PUT/DELETE/OPTIONS`) are
  retried on `429`, `502`, `503`, and on pre-send transport failures (DNS, TCP
  connect, TLS handshake).
- **`POST`/`PATCH` are never retried on a status code by default**, since the
  request may already have been applied server-side. Pre-send transport failures
  (before any byte is written) are still retried for these methods.
- Up to `3` retries with decorrelated-jitter exponential backoff, honoring a server
  `Retry-After` header (capped at 60s).
- **SSE / streaming requests bypass all automatic retry** because their bodies
  are not safely replayable. The SSE client also disables OkHttp's built-in
  connection recovery to prevent a streaming command POST from being replayed.

::: warning Behavior change
SDK-policy retries are on by default. This can increase the number of HTTP attempts
and tail latency compared to earlier SDK versions. To disable the new SDK-policy
retries, use `RetryPolicy.disabled()`; non-streaming requests then fall back to
OkHttp's pre-existing built-in connection recovery.
:::

```java
import com.alibaba.opensandbox.sandbox.transport.RetryPolicy;
import com.alibaba.opensandbox.sandbox.transport.StatusCode;
import java.time.Duration;
import java.util.Set;

// Disable SDK-policy retries and retain OkHttp's built-in connection recovery.
ConnectionConfig config = ConnectionConfig.builder()
    .apiKey("your-key")
    .domain("api.opensandbox.io")
    .retryPolicy(RetryPolicy.disabled())
    .build();

// Custom policy: more retries, an overall wall-clock deadline, and an opt-in to
// retry POST/PATCH on 503 (only safe if your endpoints are idempotent).
ConnectionConfig tuned = ConnectionConfig.builder()
    .apiKey("your-key")
    .domain("api.opensandbox.io")
    .retryPolicy(new RetryPolicy(
        /* maxRetries */ 5,
        /* initialBackoff */ Duration.ofMillis(500),
        /* maxBackoff */ Duration.ofSeconds(30),
        /* backoffMultiplier */ 2.0,
        /* jitter */ com.alibaba.opensandbox.sandbox.transport.JitterMode.DECORRELATED,
        /* retryableStatusCodesIdempotent */ RetryPolicy.DEFAULT_IDEMPOTENT_STATUS,
        /* retryableStatusCodesNonIdempotent */ Set.of(StatusCode.SERVICE_UNAVAILABLE),
        /* perAttemptTimeout */ null,
        /* overallDeadline */ Duration.ofSeconds(20),
        /* onRetry */ null))
    .build();
```

### 3. Sandbox Creation Configuration

The `Sandbox.builder()` allows configuring the sandbox environment.

| Parameter      | Description                              | Default                         |
| -------------- | ---------------------------------------- | ------------------------------- |
| `image`        | Docker image to use                      | Required                        |
| `timeout`      | Automatic termination timeout            | 10 minutes                      |
| `entrypoint`   | Container entrypoint command             | `["tail", "-f", "/dev/null"]`   |
| `resource`     | CPU and memory limits                    | `{"cpu": "1", "memory": "2Gi"}` |
| `env`          | Environment variables                    | Empty                           |
| `metadata`     | Custom metadata tags                     | Empty                           |
| `extensions`   | Opaque server-side extension parameters  | Empty                           |
| `networkPolicy` | Optional outbound network policy (egress) | -                             |
| `credentialProxy` | Optional Credential Vault proxy startup settings | -                     |
| `readyTimeout` | Max time to wait for sandbox to be ready | 30 seconds                      |

::: warning
Metadata keys under `opensandbox.io/` are reserved for system-managed labels and will be rejected by the server.
:::

```java
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.NetworkPolicy;
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.NetworkRule;

Sandbox sandbox = Sandbox.builder()
    .connectionConfig(config)
    .image("python:3.11")
    .timeout(Duration.ofMinutes(30))
    .resource(map -> {
        map.put("cpu", "2");
        map.put("memory", "4Gi");
    })
    .env("PYTHONPATH", "/app")
    .metadata("project", "demo")
    .extension("storage.id", "dataset-001")
    .networkPolicy(
        NetworkPolicy.builder()
            .defaultAction(NetworkPolicy.DefaultAction.DENY)
            .addEgress(
                NetworkRule.builder()
                    .action(NetworkRule.Action.ALLOW)
                    .target("pypi.org")
                    .build()
            )
            .build()
    )
    .build();
```

### 4. Runtime Egress Policy Updates

Runtime egress reads and patches go directly to the sandbox egress sidecar.
The SDK first resolves the sandbox endpoint on port `18080`, then calls the sidecar `/policy` API.

Patch uses merge semantics:
- Incoming rules take priority over existing rules with the same `target`.
- Existing rules for other targets remain unchanged.
- Within a single patch payload, the first rule for a `target` wins.
- The current `defaultAction` is preserved.

```java
NetworkPolicy policy = sandbox.getEgressPolicy();

sandbox.patchEgressRules(
    List.of(
        NetworkRule.builder().action(NetworkRule.Action.ALLOW).target("www.github.com").build(),
        NetworkRule.builder().action(NetworkRule.Action.DENY).target("pypi.org").build()
    )
);
```

### 5. Credential Vault

Credential Vault injects outbound credentials from the egress sidecar while
keeping real secrets out of sandbox environment variables, commands, files, and
logs. Create the sandbox with `credentialProxyEnabled(true)`, then write
credentials and bindings through `sandbox.credentialVault()`.

```java
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.Credential;
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.CredentialAuth;
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.CredentialBinding;
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.CredentialMatch;
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.CredentialVaultCreateRequest;
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.NetworkPolicy;
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.NetworkRule;
import java.util.List;

Sandbox sandbox = Sandbox.builder()
    .connectionConfig(config)
    .image("python:3.11")
    .networkPolicy(
        NetworkPolicy.builder()
            .defaultAction(NetworkPolicy.DefaultAction.DENY)
            .addEgress(
                NetworkRule.builder()
                    .action(NetworkRule.Action.ALLOW)
                    .target("api.example.com")
                    .build()
            )
            .build()
    )
    .credentialProxyEnabled(true)
    .build();

sandbox.credentialVault().create(
    CredentialVaultCreateRequest.builder()
        .credentials(
            List.of(
                Credential.builder()
                    .name("api-token")
                    .inlineSource("<token>")
                    .build()
            )
        )
        .bindings(
            List.of(
                CredentialBinding.builder()
                    .name("api-token")
                    .match(
                        CredentialMatch.builder()
                            .schemes(CredentialMatch.Scheme.HTTPS)
                            .hosts("api.example.com")
                            .paths("/v1/*")
                            .build()
                    )
                    .auth(CredentialAuth.apiKey("x-api-key", "api-token"))
                    .build()
            )
        )
        .build()
);
```

See [Credential Vault](/guides/credential-vault) for auth types, binding
guidance, and Git/curl examples.
