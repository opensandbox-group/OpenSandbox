# OpenSandbox SDK for C#

[English](README.md) | 中文

一个用于与 OpenSandbox 进行低级交互的 C# SDK。它提供了创建、管理和与安全沙箱环境交互的能力，包括执行 shell 命令、管理文件和读取资源指标。

## 安装

### NuGet

```bash
dotnet add package Alibaba.OpenSandbox
```

### Package Manager

```powershell
Install-Package Alibaba.OpenSandbox
```

## 快速开始

以下示例展示如何创建沙箱并执行 shell 命令。

> **注意**：运行此示例之前，请确保 OpenSandbox 服务正在运行。有关启动说明，请参阅根目录的 [README_zh.md](../../../docs/README_zh.md)。

```csharp
using OpenSandbox;
using OpenSandbox.Config;
using OpenSandbox.Core;

var config = new ConnectionConfig(new ConnectionConfigOptions
{
    Domain = "api.opensandbox.io",
    ApiKey = "your-api-key",
    // Protocol = ConnectionProtocol.Https,
    // RequestTimeoutSeconds = 60,
});

try
{
    await using var sandbox = await Sandbox.CreateAsync(new SandboxCreateOptions
    {
        ConnectionConfig = config,
        Image = "ubuntu",
        TimeoutSeconds = 10 * 60,
    });

    var execution = await sandbox.Commands.RunAsync("echo 'Hello Sandbox!'");
    Console.WriteLine(execution.Logs.Stdout.FirstOrDefault()?.Text);

    // 可选但推荐：完成后终止远程实例
    await sandbox.KillAsync();
}
catch (SandboxException ex)
{
    Console.Error.WriteLine($"沙箱错误: [{ex.Error.Code}] {ex.Error.Message}");
    Console.Error.WriteLine($"Request ID: {ex.RequestId}");
}
```

## 使用示例

### 1. 生命周期管理

管理沙箱生命周期，包括续期、暂停和恢复。

```csharp
var info = await sandbox.GetInfoAsync();
Console.WriteLine($"状态: {info.Status.State}");
Console.WriteLine($"创建时间: {info.CreatedAt}");
Console.WriteLine($"过期时间: {info.ExpiresAt}"); // 使用手动清理模式时为 null

await sandbox.PauseAsync();

// Resume 返回一个新的、已连接的 Sandbox 实例
var resumed = await sandbox.ResumeAsync();

// 续期: expiresAt = now + timeoutSeconds
await resumed.RenewAsync(30 * 60);
```

通过设置 `ManualCleanup = true` 创建一个不会自动过期的沙箱：

```csharp
var manual = await Sandbox.CreateAsync(new SandboxCreateOptions
{
    ConnectionConfig = config,
    Image = "ubuntu",
    ManualCleanup = true,
});
```

注意：与 Python、JavaScript、Kotlin SDK 不同，C# SDK 使用显式的
`ManualCleanup` 开关，而不是 `TimeoutSeconds = null`。这是有意的设计，
因为在当前的 options 模型里，`int?` 不能稳定地区分“未设置，沿用默认 TTL”
和“显式请求手动清理”。

### 2. 自定义健康检查

定义自定义逻辑来确定沙箱是否就绪/健康。

```csharp
var sandbox = await Sandbox.CreateAsync(new SandboxCreateOptions
{
    ConnectionConfig = config,
    Image = "nginx:latest",
    HealthCheck = async (sbx) =>
    {
        // 示例：当端口 80 端点可用时认为沙箱健康
        var ep = await sbx.GetEndpointAsync(80);
        return !string.IsNullOrEmpty(ep.EndpointAddress);
    },
});
```

### 3. 命令执行和流式处理

执行命令并实时处理输出流。

```csharp
using OpenSandbox.Models;

var handlers = new ExecutionHandlers
{
    OnStdout = msg => { Console.WriteLine($"STDOUT: {msg.Text}"); return Task.CompletedTask; },
    OnStderr = msg => { Console.Error.WriteLine($"STDERR: {msg.Text}"); return Task.CompletedTask; },
    OnExecutionComplete = c => { Console.WriteLine($"完成，耗时 {c.ExecutionTimeMs}ms"); return Task.CompletedTask; },
};

await sandbox.Commands.RunAsync(
    "for i in 1 2 3; do echo \"Count $i\"; sleep 0.2; done",
    handlers: handlers
);
```

对于后台命令，可以轮询状态和增量日志：

```csharp
var execution = await sandbox.Commands.RunAsync(
    "python /app/server.py",
    options: new RunCommandOptions
    {
        Background = true,
        TimeoutSeconds = 120,
    });

var status = await sandbox.Commands.GetCommandStatusAsync(execution.Id!);
var logs = await sandbox.Commands.GetBackgroundCommandLogsAsync(execution.Id!, cursor: 0);
Console.WriteLine($"running={status.Running}, cursor={logs.Cursor}");
```

### 4. 全面的文件操作

管理文件和目录，包括读取、写入、列出/搜索和删除。

```csharp
await sandbox.Files.CreateDirectoriesAsync(new[]
{
    new CreateDirectoryEntry { Path = "/tmp/demo", Mode = 755 }
});

await sandbox.Files.WriteFilesAsync(new[]
{
    new WriteEntry { Path = "/tmp/demo/hello.txt", Data = "Hello World", Mode = 644 }
});

var content = await sandbox.Files.ReadFileAsync("/tmp/demo/hello.txt");
Console.WriteLine($"内容: {content}");

var files = await sandbox.Files.SearchAsync(new SearchEntry { Path = "/tmp/demo", Pattern = "*.txt" });
foreach (var file in files)
{
    Console.WriteLine(file.Path);
}

await sandbox.Files.DeleteDirectoriesAsync(new[] { "/tmp/demo" });
```

### 5. 端点

`GetEndpointAsync()` 返回**不带协议**的端点（例如 `"localhost:44772"`）。如果需要可直接使用的绝对 URL，请使用 `GetEndpointUrlAsync()`。

```csharp
var endpoint = await sandbox.GetEndpointAsync(44772);
Console.WriteLine(endpoint.EndpointAddress);

var url = await sandbox.GetEndpointUrlAsync(44772);
Console.WriteLine(url); // 例如 "http://localhost:44772"
```

### 6. 沙箱管理（管理员）

使用 `SandboxManager` 进行管理任务和查找现有沙箱。

```csharp
await using var manager = SandboxManager.Create(new SandboxManagerOptions
{
    ConnectionConfig = config
});

var list = await manager.ListSandboxInfosAsync(new SandboxFilter
{
    States = new[] { "Running" },
    PageSize = 10
});

foreach (var s in list.Items)
{
    Console.WriteLine(s.Id);
}
```

## 配置

### 1. 连接配置

`ConnectionConfig` 类管理 API 服务器连接设置。

| 参数 | 描述 | 默认值 | 环境变量 |
| --- | --- | --- | --- |
| `ApiKey` | 用于身份验证的 API 密钥 | 可选 | `OPEN_SANDBOX_API_KEY` |
| `Domain` | 沙箱服务域名 (`host[:port]`) | `localhost:8080` | `OPEN_SANDBOX_DOMAIN` |
| `Protocol` | HTTP 协议 (`Http`/`Https`) | `Http` | - |
| `RequestTimeoutSeconds` | 应用于 SDK HTTP 调用的请求超时 | `30` | - |
| `UseServerProxy` | 是否请求服务端代理的沙箱访问端点 URL | `false` | - |
| `Headers` | 应用于每个请求的额外头部 | `{}` | - |

```csharp
using OpenSandbox.Config;

// 1. 基本配置
var config = new ConnectionConfig(new ConnectionConfigOptions
{
    Domain = "api.opensandbox.io",
    ApiKey = "your-key",
    RequestTimeoutSeconds = 60,
    // UseServerProxy = true, // 当客户端无法直连沙箱 endpoint 时建议开启
});

// 2. 高级：自定义头部
var config2 = new ConnectionConfig(new ConnectionConfigOptions
{
    Domain = "api.opensandbox.io",
    ApiKey = "your-key",
    Headers = new Dictionary<string, string>
    {
        ["X-Custom-Header"] = "value"
    },
});
```

### 2. 诊断与日志

SDK 使用 `Microsoft.Extensions.Logging` 抽象。

```csharp
using Microsoft.Extensions.Logging;
using OpenSandbox.Config;

using var loggerFactory = LoggerFactory.Create(builder =>
{
    builder.SetMinimumLevel(LogLevel.Debug);
    builder.AddConsole();
});

var sandbox = await Sandbox.CreateAsync(new SandboxCreateOptions
{
    Image = "python:3.11",
    ConnectionConfig = new ConnectionConfig(),
    Diagnostics = new SdkDiagnosticsOptions
    {
        LoggerFactory = loggerFactory
    }
});
```

### 3. 沙箱创建配置

`Sandbox.CreateAsync()` 允许配置沙箱环境。

| 参数 | 描述 | 默认值 |
| --- | --- | --- |
| `Image` | 要使用的 Docker 镜像 | 必需 |
| `TimeoutSeconds` | 自动终止超时（服务器端 TTL） | 10 分钟 |
| `Entrypoint` | 容器入口点命令 | `["tail","-f","/dev/null"]` |
| `Resource` | CPU 和内存限制（字符串映射） | `{"cpu":"1","memory":"2Gi"}` |
| `Env` | 环境变量 | `{}` |
| `Metadata` | 自定义元数据标签 | `{}` |
| `NetworkPolicy` | 可选的出站网络策略（egress） | - |
| `Volumes` | 可选存储挂载（`Host` / `PVC`，支持 `ReadOnly` 与 `SubPath`） | - |
| `Extensions` | 额外的服务器定义字段 | `{}` |
| `SkipHealthCheck` | 跳过就绪检查（`Running` + 健康检查） | `false` |
| `HealthCheck` | 自定义就绪检查 | - |
| `ReadyTimeoutSeconds` | 等待就绪的最大时间 | 30 秒 |
| `HealthCheckPollingInterval` | 等待时的轮询间隔（毫秒） | 200 ms |

注意：`opensandbox.io/` 前缀下的 metadata key 属于系统保留标签，服务端会拒绝用户传入。

```csharp
var sandbox = await Sandbox.CreateAsync(new SandboxCreateOptions
{
    ConnectionConfig = config,
    Image = "python:3.11",
    NetworkPolicy = new NetworkPolicy
    {
        DefaultAction = NetworkRuleAction.Deny,
        Egress = new List<NetworkRule>
        {
            new() { Action = NetworkRuleAction.Allow, Target = "pypi.org" }
        }
    },
    Volumes = new[]
    {
        new Volume
        {
            Name = "workspace",
            Host = new Host { Path = "/tmp/opensandbox-e2e/host-volume-test" },
            MountPath = "/workspace",
            ReadOnly = false
        }
    }
});
```

### 3. 运行时 Egress 策略更新

运行时的 egress 查询和 patch 会直接访问沙箱内的 egress sidecar。
SDK 会先解析 `18080` 端口对应的 sandbox endpoint，再调用 sidecar 的 `/policy` API。

```csharp
var policy = await sandbox.GetEgressPolicyAsync();

await sandbox.PatchEgressRulesAsync(new[]
{
    new NetworkRule { Action = NetworkRuleAction.Allow, Target = "www.github.com" },
    new NetworkRule { Action = NetworkRuleAction.Deny, Target = "pypi.org" }
});
```

### 4. 资源清理

`Sandbox` 和 `SandboxManager` 都实现了 `IAsyncDisposable`。完成后使用 `await using` 或调用 `DisposeAsync()`。

```csharp
await using var sandbox = await Sandbox.CreateAsync(options);
// ... 使用沙箱 ...
// 离开作用域时自动释放
```

## 支持的框架

- .NET Standard 2.0（最大兼容性，支持 .NET Framework 4.6.1+、.NET Core 2.0+、Mono、Xamarin 等）
- .NET Standard 2.1
- .NET 6.0 (LTS)
- .NET 7.0
- .NET 8.0 (LTS)
- .NET 9.0
- .NET 10.0

## 许可证

Apache License 2.0
