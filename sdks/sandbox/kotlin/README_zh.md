# Alibaba Sandbox SDK for Kotlin

中文 | [English](README.md)

用于与 OpenSandbox 进行底层交互的 Kotlin SDK。它提供了创建、管理和与安全沙箱环境交互的能力，包括执行 Shell 命令、管理文件和监控资源。

## 安装指南

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

## 快速开始

以下示例展示了如何创建一个沙箱并执行 Shell 命令。

> **注意**: 在运行此示例之前，请确保 OpenSandbox 服务已启动。服务启动请参考根目录的 [README_zh.md](../../../docs/README_zh.md)。

```java
import com.alibaba.opensandbox.sandbox.Sandbox;
import com.alibaba.opensandbox.sandbox.config.ConnectionConfig;
import com.alibaba.opensandbox.sandbox.domain.exceptions.SandboxException;
import com.alibaba.opensandbox.sandbox.domain.models.execd.executions.Execution;
import com.alibaba.opensandbox.sandbox.domain.models.execd.executions.RunCommandRequest;

public class QuickStart {
    public static void main(String[] args) {
        // 1. 配置连接信息
        ConnectionConfig config = ConnectionConfig.builder()
            .domain("api.opensandbox.io")
            .apiKey("your-api-key")
            .build();

        // 2. 使用 try-with-resources 创建 Sandbox
        try (Sandbox sandbox = Sandbox.builder()
                .connectionConfig(config)
                .image("ubuntu")
                .build()) {

            // 3. 执行 Shell 命令
            Execution execution = sandbox
                    .commands()
                    .run("echo 'Hello Sandbox!'");

            // 4. 打印输出
            System.out.println(execution.getLogs().getStdout().get(0).getText());

            // 5. 清理资源 (自动调用 sandbox.close())
            // 注意: 如果希望立即终止远程沙箱实例，仍需显式调用 kill()
            sandbox.kill();
        } catch (SandboxException e) {
            // 处理 Sandbox 特定异常
            System.err.println("沙箱错误: [" + e.getError().getCode() + "] " + e.getError().getMessage());
            System.err.println("Request ID: " + e.getRequestId());
        } catch (Exception e) {
            e.printStackTrace();
        }
    }
}
```

## 核心功能示例

### 1. 生命周期管理

管理沙箱的生命周期，包括续期、暂停、恢复和状态查询。

```java
// 续期沙箱
// 此操作将沙箱的过期时间重置为 (当前时间 + duration)
sandbox.renew(Duration.ofMinutes(30));

// 暂停执行 (挂起所有进程)
sandbox.pause();

// 恢复执行
sandbox.resume();

// 获取当前状态
SandboxInfo info = sandbox.getInfo();
System.out.println("当前状态: " + info.getStatus().getState());
System.out.println("过期时间: " + info.getExpiresAt()); // 使用手动清理模式时为 null
```

通过传入 `timeout(null)` 创建一个不会自动过期的沙箱：

```java
Sandbox manual = Sandbox.builder()
    .connectionConfig(config)
    .image("ubuntu")
    .timeout(null)
    .build();
```

### 2. 自定义健康检查

定义自定义逻辑来判断沙箱是否健康。这可以覆盖默认的 Ping 检查。

```java
Sandbox sandbox = Sandbox.builder()
    .connectionConfig(config)
    .image("nginx:latest")
    // 自定义检查：等待 80 端口可访问
    .healthCheck(sb -> {
        try {
            // 1. 获取沙箱 80 端口映射的外部访问地址
            SandboxEndpoint endpoint = sb.getEndpoint(80);

            // 2. 执行你的连接检查逻辑 (例如 HTTP 请求, Socket 连接等)
            // return checkConnection(endpoint.getEndpoint());
            return true;
        } catch (Exception e) {
            return false;
        }
    })
    .build();
```

### 3. 命令执行与流式响应

执行命令并实时处理输出流。

```java
// 创建流式输出处理器
ExecutionHandlers handlers = ExecutionHandlers.builder()
    .onStdout(msg -> System.out.println("STDOUT: " + msg.getText()))
    .onStderr(msg -> System.err.println("STDERR: " + msg.getText()))
    .onExecutionComplete(complete ->
        System.out.println("命令执行耗时: " + complete.getExecutionTimeInMillis() + "ms")
    )
    .build();

// 带处理器的命令执行
RunCommandRequest request = RunCommandRequest.builder()
    .command("for i in {1..5}; do echo \"Count $i\"; sleep 0.5; done")
    .handlers(handlers)
    .build();

sandbox.commands().run(request);
```

### 4. 全面的文件操作

管理文件和目录，包括读写、列表、删除和搜索。

```java
// 1. 写入文件
sandbox.files().write(List.of(
    WriteEntry.builder()
        .path("/tmp/hello.txt")
        .data("Hello World")
        .mode(644)
        .build()
));

// 2. 读取文件
String content = sandbox.files().readFile("/tmp/hello.txt", "UTF-8", null);
System.out.println("文件内容: " + content);

// 3. 搜索/列表文件
List<EntryInfo> files = sandbox.files().search(
    SearchEntry.builder()
        .path("/tmp")
        .pattern("*.txt")
        .build()
);
files.forEach(f -> System.out.println("找到文件: " + f.getPath()));

// 4. 删除文件
sandbox.files().deleteFiles(List.of("/tmp/hello.txt"));
```

### 5. 沙箱管理 (Sandbox Manager)

使用 `SandboxManager` 进行管理操作，如查询现有沙箱列表。

```java
SandboxManager manager = SandboxManager.builder()
    .connectionConfig(config)
    .build();

import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SandboxState;

// ...

// 列出运行中的沙箱
PagedSandboxInfos sandboxes = manager.listSandboxInfos(
    SandboxFilter.builder()
        .states(SandboxState.RUNNING)
        .pageSize(10)
        .page(1)
        .build()
);

sandboxes.getSandboxInfos().forEach(info -> {
    System.out.println("Found sandbox: " + info.getId());
    // 执行管理操作
    manager.killSandbox(info.getId());
});

// Try-with-resources 会自动调用 manager.close()
// manager.close();
```

### 6. Sandbox Pool（客户端池化）

你可以使用 `SandboxPool` 维护就绪沙箱的空闲缓冲区，以降低 `acquire` 延迟。

> ⚠ 实验性能力：`SandboxPool` 仍在根据真实生产场景持续演进，后续版本可能存在 breaking changes。

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
    .stateStore(new InMemoryPoolStateStore()) // 单机实现
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

池状态语义：
- `acquire()` 仅允许在 `RUNNING` 状态调用。
- 在 `DRAINING` / `STOPPED` 状态，`acquire()` 会抛出 `PoolNotRunningException`。
- `ownerId` 表示主锁持有者身份（节点/进程标识），不是池子标识；
  如果不传，SDK 会自动生成基于 UUID 的默认值。
- 如果你需要在 warmup 成功后、`putIdle` 之前对沙箱做预处理，可以使用 `warmupSandboxPreparer(...)`。


> 在分布式部署场景下，可以使用可选模块 `com.alibaba.opensandbox:sandbox-pool-redis`，也可以自行提供 `PoolStateStore` 实现。Redis 模块接收业务方自己创建和管理的 Jedis client，因此 Redis 连接配置和生命周期仍由业务方负责。共享同一池命名空间的节点必须使用相同的 sandbox 创建与 warmup 定义；如果定义发生变化，建议使用新的 `poolName` 或命名空间。建议将 `primaryLockTtl` 配置为大于 `warmupReadyTimeout` 加上预期 warmup preparer 耗时和缓冲时间，否则节点可能在创建 idle sandbox 过程中失去主锁。
> 分布式模式下，`resize(maxIdle)` 可以在任意节点调用；调用返回只表示目标值已写入共享状态存储，实际补池或缩容由当前主节点在周期性 reconcile 中执行。如果需要清空分布式 idle buffer，建议调用 `resize(0)` 并等待 `snapshot().idleCount == 0`；`releaseAllIdle()` 只适合作为 best-effort 的兜底清理。

## 配置说明

### 1. 连接配置 (Connection Configuration)

`ConnectionConfig` 类管理与 API 服务器的连接设置。

| 参数             | 描述                         | 默认值                   | 环境变量               |
| ---------------- | ---------------------------- | ------------------------ | ---------------------- |
| `apiKey`         | 用于认证的 API Key           | 必填                     | `OPEN_SANDBOX_API_KEY` |
| `domain`         | 沙箱服务的端点域名           | 必填 (或 localhost:8080) | `OPEN_SANDBOX_DOMAIN`  |
| `protocol`       | HTTP 协议 (http/https)       | `http`                   | -                      |
| `requestTimeout` | API 请求超时时间             | 30 秒                    | -                      |
| `debug`          | 是否开启 HTTP 请求的调试日志 | `false`                  | -                      |
| `headers`        | 自定义 HTTP 请求头           | 空                       | -                      |
| `connectionPool` | 共享 OKHttp 连接池           | SDK 每实例创建            | -                      |
| `useServerProxy` | 是否通过沙箱服务代理访问 execd/endpoint（适用于客户端无法直连沙箱的场景） | `false` | -                      |

```java
// 1. 基础配置
ConnectionConfig config = ConnectionConfig.builder()
    .apiKey("your-key")
    .domain("api.opensandbox.io")
    .requestTimeout(Duration.ofSeconds(60))
    .build();

// 2. 进阶配置：共享连接池 (Shared Connection Pool)
// 如果你需要创建大量 Sandbox 实例，建议共享连接池以节省资源。
// SDK 默认连接保活时间为 30 秒。
ConnectionPool sharedPool = new ConnectionPool(50, 30, TimeUnit.SECONDS);

ConnectionConfig sharedConfig = ConnectionConfig.builder()
    .apiKey("your-key")
    .domain("api.opensandbox.io")
    .headers(Map.of(
        "X-Custom-Header", "value",
        "X-Request-ID", "trace-123"
    ))
    .connectionPool(sharedPool) // 注入共享连接池
    .build();
```

### 2. 沙箱创建配置 (Sandbox Creation Configuration)

`Sandbox.builder()` 用于配置沙箱环境。

| 参数           | 描述                   | 默认值                          |
| -------------- | ---------------------- | ------------------------------- |
| `image`        | 使用的 Docker 镜像     | 必填                            |
| `timeout`      | 自动终止的超时时间     | 10 分钟                         |
| `entrypoint`   | 容器启动入口命令       | `["tail", "-f", "/dev/null"]`   |
| `resource`     | CPU 和内存限制         | `{"cpu": "1", "memory": "2Gi"}` |
| `env`          | 环境变量               | 空                              |
| `metadata`     | 自定义元数据标签       | 空                              |
| `networkPolicy` | 可选的出站网络策略（egress） | -                         |
| `readyTimeout` | 等待沙箱就绪的最大时间 | 30 秒                           |

注意：`opensandbox.io/` 前缀下的 metadata key 属于系统保留标签，服务端会拒绝用户传入。

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

### 3. 运行时 Egress 策略更新

运行时的 egress 查询和 patch 会直接访问沙箱内的 egress sidecar。
SDK 会先解析 `18080` 端口对应的 sandbox endpoint，再调用 sidecar 的 `/policy` API。

```java
NetworkPolicy policy = sandbox.getEgressPolicy();

sandbox.patchEgressRules(
    List.of(
        NetworkRule.builder().action(NetworkRule.Action.ALLOW).target("www.github.com").build(),
        NetworkRule.builder().action(NetworkRule.Action.DENY).target("pypi.org").build()
    )
);
```
