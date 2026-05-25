# Alibaba Code Interpreter SDK for C#

[English](README.md) | 中文

一个用于在安全隔离沙箱中执行代码的 C# SDK。它提供了高级 API，用于安全地运行 Python、Java、Go、TypeScript 等语言，并支持代码执行上下文管理。

## 前置条件

此 SDK 需要包含 Code Interpreter 运行时环境的 Docker 镜像。您必须使用 `opensandbox/code-interpreter` 镜像（或其衍生版本），该镜像预装了 Python、Java、Go、Node.js 等运行时。

有关支持的语言和版本的详细信息，请参阅[环境文档](../../../sandboxes/code-interpreter/README.md)。

## 安装

### NuGet

```bash
dotnet add package Alibaba.OpenSandbox.CodeInterpreter
```

### PackageReference

```xml
<PackageReference Include="Alibaba.OpenSandbox.CodeInterpreter" Version="0.1.1" />
```

## 快速开始

以下示例演示如何创建具有特定运行时配置的沙箱并执行简单脚本。

> **注意**：运行此示例之前，请确保 OpenSandbox 服务正在运行。有关启动说明，请参阅根目录的 [README.md](../../../README.md)。

```csharp
using OpenSandbox;
using OpenSandbox.CodeInterpreter;
using OpenSandbox.CodeInterpreter.Models;
using OpenSandbox.Config;

// 1. 配置连接
var config = new ConnectionConfig(new ConnectionConfigOptions
{
    Domain = "api.opensandbox.io",
    ApiKey = "your-api-key"
});

// 2. 创建带有 code-interpreter 镜像和运行时版本的 Sandbox
await using var sandbox = await Sandbox.CreateAsync(new SandboxCreateOptions
{
    ConnectionConfig = config,
    Image = "opensandbox/code-interpreter:v1.0.2",
    Entrypoint = new[] { "/opt/opensandbox/code-interpreter.sh" },
    Env = new Dictionary<string, string>
    {
        ["PYTHON_VERSION"] = "3.11",
        ["JAVA_VERSION"] = "17",
        ["NODE_VERSION"] = "20",
        ["GO_VERSION"] = "1.24"
    },
    TimeoutSeconds = 15 * 60
});

// 3. 创建 CodeInterpreter 包装器
var ci = await CodeInterpreter.CreateAsync(sandbox);

// 4. 创建执行上下文 (Python)
var ctx = await ci.Codes.CreateContextAsync(SupportedLanguage.Python);

// 5. 运行代码
var result = await ci.Codes.RunAsync(
    "import sys\nprint(sys.version)\nresult = 2 + 2\nresult",
    new RunCodeOptions { Context = ctx });

// 6. 打印输出
Console.WriteLine(result.Results.FirstOrDefault()?.Text);

// 7. 清理远程实例（可选但推荐）
await sandbox.KillAsync();
```

## 日志（ILogger）

SDK 使用 `Microsoft.Extensions.Logging` 抽象。创建 Sandbox/CodeInterpreter 时可通过
diagnostics 传入你自己的 `ILoggerFactory`：

```csharp
using Microsoft.Extensions.Logging;
using OpenSandbox.Config;

using var loggerFactory = LoggerFactory.Create(builder =>
{
    builder.SetMinimumLevel(LogLevel.Debug);
    builder.AddConsole();
});

await using var sandbox = await Sandbox.CreateAsync(new SandboxCreateOptions
{
    Image = "opensandbox/code-interpreter:v1.0.2",
    Diagnostics = new SdkDiagnosticsOptions
    {
        LoggerFactory = loggerFactory
    }
});

var ci = await CodeInterpreter.CreateAsync(sandbox, new CodeInterpreterCreateOptions
{
    Diagnostics = new SdkDiagnosticsOptions
    {
        LoggerFactory = loggerFactory
    }
});
```

## 运行时配置

### Docker 镜像

Code Interpreter SDK 依赖于专门的环境。请确保您的沙箱提供者有可用的 `opensandbox/code-interpreter` 镜像。

### 语言版本选择

您可以通过在创建 `Sandbox` 时设置相应的环境变量来指定所需的编程语言版本。

| 语言 | 环境变量 | 示例值 | 默认值（如未设置） |
| --- | --- | --- | --- |
| Python | `PYTHON_VERSION` | `3.11` | 镜像默认值 |
| Java | `JAVA_VERSION` | `17` | 镜像默认值 |
| Node.js | `NODE_VERSION` | `20` | 镜像默认值 |
| Go | `GO_VERSION` | `1.24` | 镜像默认值 |

```csharp
await using var sandbox = await Sandbox.CreateAsync(new SandboxCreateOptions
{
    ConnectionConfig = config,
    Image = "opensandbox/code-interpreter:v1.0.2",
    Entrypoint = new[] { "/opt/opensandbox/code-interpreter.sh" },
    Env = new Dictionary<string, string>
    {
        ["JAVA_VERSION"] = "17",
        ["GO_VERSION"] = "1.24"
    }
});
```

## 使用示例

### 0. 使用 `Language` 运行（默认语言上下文）

如果您不需要管理显式的上下文 ID，可以仅通过指定 `Language` 来运行代码。
当省略 `Context.Id` 时，execd 可以为该语言创建/重用默认会话，因此状态可以在多次运行之间持久化。

```csharp
await ci.Codes.RunAsync("x = 42", new RunCodeOptions { Language = SupportedLanguage.Python });
var execution = await ci.Codes.RunAsync("result = x\nresult", new RunCodeOptions { Language = SupportedLanguage.Python });
Console.WriteLine(execution.Results.FirstOrDefault()?.Text); // "42"
```

### 0.1 上下文管理（列出/获取/删除）

您可以显式管理上下文（与 Python/Kotlin SDK 对齐）：

```csharp
var ctx = await ci.Codes.CreateContextAsync(SupportedLanguage.Python);

var same = await ci.Codes.GetContextAsync(ctx.Id!);
Console.WriteLine($"{same.Id}, {same.Language}");

var pyOnly = await ci.Codes.ListContextsAsync(SupportedLanguage.Python);

await ci.Codes.DeleteContextAsync(ctx.Id!);
await ci.Codes.DeleteContextsAsync(SupportedLanguage.Python); // 批量清理
```

### 1. Java 代码执行

```csharp
var javaCtx = await ci.Codes.CreateContextAsync(SupportedLanguage.Java);
var execution = await ci.Codes.RunAsync(
    @"System.out.println(""Calculating sum..."");
int a = 10;
int b = 20;
int sum = a + b;
System.out.println(""Sum: "" + sum);
sum",
    new RunCodeOptions { Context = javaCtx });

foreach (var msg in execution.Logs.Stdout)
{
    Console.WriteLine(msg.Text);
}
```

### 2. 流式输出处理

实时处理 stdout/stderr 和执行事件。

```csharp
using OpenSandbox.Models;

var handlers = new ExecutionHandlers
{
    OnStdout = async msg => Console.WriteLine($"STDOUT: {msg.Text}"),
    OnStderr = async msg => Console.Error.WriteLine($"STDERR: {msg.Text}"),
    OnResult = async r => Console.WriteLine($"RESULT: {r.Text}")
};

var pyCtx = await ci.Codes.CreateContextAsync(SupportedLanguage.Python);
await ci.Codes.RunAsync(
    "import time\nfor i in range(5):\n    print(i)\n    time.sleep(0.2)",
    new RunCodeOptions { Context = pyCtx, Handlers = handlers });
```

### 3. 使用 IAsyncEnumerable 流式处理

```csharp
var request = new RunCodeRequest
{
    Code = "for i in range(10): print(i)",
    Context = new CodeContext { Language = SupportedLanguage.Python }
};

await foreach (var ev in ci.Codes.RunStreamAsync(request))
{
    switch (ev.Type)
    {
        case "stdout":
            Console.Write(ev.Text);
            break;
        case "stderr":
            Console.Error.Write(ev.Text);
            break;
        case "result":
            Console.WriteLine($"结果: {ev.Results}");
            break;
        case "error":
            Console.WriteLine($"错误: {ev.Error}");
            break;
    }
}
```

### 4. 中断执行

```csharp
var ctx = await ci.Codes.CreateContextAsync(SupportedLanguage.Python);

// 启动长时间运行的任务
var executionId = new TaskCompletionSource<string>();
var task = ci.Codes.RunAsync(
    "import time\nwhile True: time.sleep(1)",
    new RunCodeOptions
    {
        Context = ctx,
        Handlers = new ExecutionHandlers
        {
            OnInit = init =>
            {
                executionId.TrySetResult(init.Id);
                return Task.CompletedTask;
            }
        }
    });

// 拿到执行 ID 后中断
await ci.Codes.InterruptAsync(await executionId.Task);
```

## API 参考

### CodeInterpreter

| 方法 | 描述 |
|------|------|
| `CreateAsync(sandbox, options?)` | 从沙箱创建代码解释器 |

| 属性 | 描述 |
|------|------|
| `Sandbox` | 底层沙箱实例 |
| `Codes` | 代码执行服务 |
| `Id` | 沙箱 ID |
| `Files` | 文件系统操作 |
| `Commands` | Shell 命令执行 |
| `Metrics` | 资源指标 |

### ICodes

| 方法 | 描述 |
|------|------|
| `CreateContextAsync(language)` | 创建新的执行上下文 |
| `GetContextAsync(contextId)` | 获取现有上下文 |
| `ListContextsAsync(language)` | 列出指定语言的上下文 |
| `DeleteContextAsync(contextId)` | 删除特定上下文 |
| `DeleteContextsAsync(language)` | 删除某语言的所有上下文 |
| `RunAsync(code, options?)` | 执行代码并返回结果 |
| `RunStreamAsync(request)` | 执行代码并流式输出 |
| `InterruptAsync(executionId)` | 按执行 ID 中断正在运行的执行 |

## 注意事项

- **生命周期**：`CodeInterpreter` 包装现有的 `Sandbox` 实例并重用其连接配置。完成后调用 `sandbox.KillAsync()` 以释放资源。
- **默认上下文**：`Codes.RunAsync(..., new RunCodeOptions { Language = ... })` 使用语言默认上下文（状态可以在多次运行之间持久化）。
- **取消支持**：所有异步方法都支持 `CancellationToken`。

## 系统要求

- .NET Standard 2.0+ / .NET 6.0+
- OpenSandbox Sandbox SDK (`Alibaba.OpenSandbox`)

## 许可证

Apache License 2.0
