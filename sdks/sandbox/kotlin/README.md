# Alibaba Sandbox SDK for Kotlin

English | [中文](README_zh.md)

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

> **Note**: Before running this example, ensure the OpenSandbox service is running. See the root [README.md](../../../README.md) for startup instructions.

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

> ⚠ Experimental: `SandboxPool` is still evolving based on production feedback and may introduce breaking changes in future releases.

```java
import com.alibaba.opensandbox.sandbox.pool.SandboxPool;
import com.alibaba.opensandbox.sandbox.domain.pool.PoolCreationSpec;
import com.alibaba.opensandbox.sandbox.domain.pool.AcquirePolicy;
import com.alibaba.opensandbox.sandbox.infrastructure.pool.InMemoryPoolStateStore;

SandboxPool pool = SandboxPool.builder()
    .poolName("demo-pool")
    .ownerId("worker-1")
    .maxIdle(3)
    .warmupReadyTimeout(Duration.ofSeconds(45))
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

Pool lifecycle semantics:
- `acquire()` is only allowed when pool state is `RUNNING`.
- In `DRAINING` / `STOPPED`, `acquire()` throws `PoolNotRunningException`.
- `maxIdle` is the target/cap for ready idle sandboxes. It is not a global limit
  on borrowed sandboxes or sandboxes created by `AcquirePolicy.DIRECT_CREATE`.
- `ownerId` is the lock owner identity (node/process id), not the pool identifier.
  If omitted, SDK auto-generates a UUID-based default.
- Use `warmupSandboxPreparer(...)` if you need to prepare a sandbox after warmup readiness succeeds and before it is put into the idle pool.


> For distributed deployment, use the optional `com.alibaba.opensandbox:sandbox-pool-redis` module or provide a custom `PoolStateStore` implementation. The Redis module accepts a caller-managed Jedis client, so your application keeps ownership of Redis connection configuration and lifecycle. Nodes sharing the same pool namespace must use the same sandbox creation and warmup definition; use a new `poolName` or namespace when changing that definition. Configure `primaryLockTtl` greater than `warmupReadyTimeout` plus expected warmup preparer time and buffer, otherwise leadership may expire while a node is creating idle sandboxes.
> In distributed mode, `resize(maxIdle)` can be called from any node. The call returns after the target is stored in the shared state store; the current primary applies replenish or shrink work during periodic reconcile. Use `resize(0)` and wait for `snapshot().idleCount == 0` when you need to drain the distributed idle buffer; `releaseAllIdle()` is only a best-effort cleanup pass.

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
| `useServerProxy` | Use sandbox server as proxy for execd/endpoint requests (e.g. when client cannot reach the sandbox directly) | `false` | -                      |

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

### 2. Sandbox Creation Configuration

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
| `readyTimeout` | Max time to wait for sandbox to be ready | 30 seconds                      |

Note: metadata keys under `opensandbox.io/` are reserved for system-managed
labels and will be rejected by the server.

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

### 3. Runtime Egress Policy Updates

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
