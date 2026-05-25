# OpenSandbox SDK for C#

English | [中文](README_zh.md)

A C# SDK for low-level interaction with OpenSandbox. It provides the ability to create, manage, and interact with secure sandbox environments, including executing shell commands, managing files, and reading resource metrics.

## Installation

### NuGet

```bash
dotnet add package Alibaba.OpenSandbox
```

### Package Manager

```powershell
Install-Package Alibaba.OpenSandbox
```

## Quick Start

The following example shows how to create a sandbox and execute a shell command.

> **Note**: Before running this example, ensure the OpenSandbox service is running. See the root [README.md](../../../README.md) for startup instructions.

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

    // Optional but recommended: terminate the remote instance when you are done.
    await sandbox.KillAsync();
}
catch (SandboxException ex)
{
    Console.Error.WriteLine($"Sandbox Error: [{ex.Error.Code}] {ex.Error.Message}");
    Console.Error.WriteLine($"Request ID: {ex.RequestId}");
}
```

## Usage Examples

### 1. Lifecycle Management

Manage the sandbox lifecycle, including renewal, pausing, and resuming.

```csharp
var info = await sandbox.GetInfoAsync();
Console.WriteLine($"State: {info.Status.State}");
Console.WriteLine($"Created: {info.CreatedAt}");
Console.WriteLine($"Expires: {info.ExpiresAt}"); // null when manual cleanup mode is used

await sandbox.PauseAsync();

// Resume returns a fresh, connected Sandbox instance.
var resumed = await sandbox.ResumeAsync();

// Renew: expiresAt = now + timeoutSeconds
await resumed.RenewAsync(30 * 60);
```

Create a non-expiring sandbox by setting `ManualCleanup = true`:

```csharp
var manual = await Sandbox.CreateAsync(new SandboxCreateOptions
{
    ConnectionConfig = config,
    Image = "ubuntu",
    ManualCleanup = true,
});
```

Note: unlike the Python, JavaScript, and Kotlin SDKs, the C# SDK uses an explicit
`ManualCleanup` flag instead of `TimeoutSeconds = null`. This is intentional:
`int?` in the current options model cannot reliably distinguish "unset, use the
default TTL" from "explicitly request manual cleanup" without making the default
creation path ambiguous.

### Connect to an Existing Sandbox

Use `ConnectAsync` when you already have a sandbox ID and need a new SDK instance bound to it.

```csharp
var connected = await Sandbox.ConnectAsync(new SandboxConnectOptions
{
    SandboxId = "existing-sandbox-id",
    ConnectionConfig = config
});
```

### 2. Custom Health Check

Define custom logic to determine whether the sandbox is ready/healthy.

```csharp
var sandbox = await Sandbox.CreateAsync(new SandboxCreateOptions
{
    ConnectionConfig = config,
    Image = "nginx:latest",
    HealthCheck = async (sbx) =>
    {
        // Example: consider the sandbox healthy when port 80 endpoint becomes available
        var ep = await sbx.GetEndpointAsync(80);
        return !string.IsNullOrEmpty(ep.EndpointAddress);
    },
});
```

### 3. Command Execution & Streaming

Execute commands and handle output streams in real-time.

```csharp
using OpenSandbox.Models;

var handlers = new ExecutionHandlers
{
    OnStdout = msg => { Console.WriteLine($"STDOUT: {msg.Text}"); return Task.CompletedTask; },
    OnStderr = msg => { Console.Error.WriteLine($"STDERR: {msg.Text}"); return Task.CompletedTask; },
    OnExecutionComplete = c => { Console.WriteLine($"Finished in {c.ExecutionTimeMs}ms"); return Task.CompletedTask; },
};

await sandbox.Commands.RunAsync(
    "for i in 1 2 3; do echo \"Count $i\"; sleep 0.2; done",
    handlers: handlers
);
```

For background commands, you can poll status and incremental logs:

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

### 4. Comprehensive File Operations

Manage files and directories, including read, write, list/search, and delete.

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
Console.WriteLine($"Content: {content}");

var files = await sandbox.Files.SearchAsync(new SearchEntry { Path = "/tmp/demo", Pattern = "*.txt" });
foreach (var file in files)
{
    Console.WriteLine(file.Path);
}

await sandbox.Files.DeleteDirectoriesAsync(new[] { "/tmp/demo" });

// Delete one or more files directly.
await sandbox.Files.DeleteFilesAsync(new[] { "/tmp/demo/hello.txt" });
```

### 5. Endpoints

`GetEndpointAsync()` returns an endpoint **without a scheme** (for example `"localhost:44772"`). Use `GetEndpointUrlAsync()` if you want a ready-to-use absolute URL.

```csharp
var endpoint = await sandbox.GetEndpointAsync(44772);
Console.WriteLine(endpoint.EndpointAddress);

var url = await sandbox.GetEndpointUrlAsync(44772);
Console.WriteLine(url); // e.g., "http://localhost:44772"
```

### 6. Sandbox Management (Admin)

Use `SandboxManager` for administrative tasks and finding existing sandboxes.

```csharp
await using var manager = SandboxManager.Create(new SandboxManagerOptions
{
    ConnectionConfig = config
});

var list = await manager.ListSandboxInfosAsync(new SandboxFilter
{
    States = new[] { SandboxStates.Running },
    PageSize = 10
});

foreach (var s in list.Items)
{
    Console.WriteLine(s.Id);
}
```

## Configuration

### 1. Connection Configuration

The `ConnectionConfig` class manages API server connection settings.

| Parameter | Description | Default | Environment Variable |
| --- | --- | --- | --- |
| `ApiKey` | API key for authentication | Optional | `OPEN_SANDBOX_API_KEY` |
| `Domain` | Sandbox service domain (`host[:port]`) | `localhost:8080` | `OPEN_SANDBOX_DOMAIN` |
| `Protocol` | HTTP protocol (`Http`/`Https`) | `Http` | - |
| `RequestTimeoutSeconds` | Request timeout applied to SDK HTTP calls | `30` | - |
| `UseServerProxy` | Request server-proxied sandbox endpoint URLs | `false` | - |
| `Headers` | Extra headers applied to every request | `{}` | - |

```csharp
using OpenSandbox.Config;

// 1. Basic configuration
var config = new ConnectionConfig(new ConnectionConfigOptions
{
    Domain = "api.opensandbox.io",
    ApiKey = "your-key",
    RequestTimeoutSeconds = 60,
    // UseServerProxy = true, // Useful when the client cannot access sandbox endpoint directly
});

// 2. Advanced: custom headers
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

### 2. Diagnostics and Logging

The SDK uses `Microsoft.Extensions.Logging` abstractions.

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

### 3. Sandbox Creation Configuration

`Sandbox.CreateAsync()` allows configuring the sandbox environment.

| Parameter | Description | Default |
| --- | --- | --- |
| `Image` | Docker image to use | Required |
| `TimeoutSeconds` | Automatic termination timeout (server-side TTL) | 10 minutes |
| `Entrypoint` | Container entrypoint command | `["tail","-f","/dev/null"]` |
| `Resource` | CPU and memory limits (string map) | `{"cpu":"1","memory":"2Gi"}` |
| `Env` | Environment variables | `{}` |
| `Metadata` | Custom metadata tags | `{}` |
| `NetworkPolicy` | Optional outbound network policy (egress) | - |
| `Volumes` | Optional storage mounts (`Host` / `PVC`, supports `ReadOnly` and `SubPath`) | - |
| `Extensions` | Extra server-defined fields | `{}` |
| `SkipHealthCheck` | Skip readiness checks (`Running` + health check) | `false` |
| `HealthCheck` | Custom readiness check | - |
| `ReadyTimeoutSeconds` | Max time to wait for readiness | 30 seconds |
| `HealthCheckPollingInterval` | Poll interval while waiting (milliseconds) | 200 ms |

Note: metadata keys under `opensandbox.io/` are reserved for system-managed
labels and will be rejected by the server.

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

### 4. Runtime Egress Policy Updates

Runtime egress reads and patches go directly to the sandbox egress sidecar.
The SDK first resolves the sandbox endpoint on port `18080`, then calls the sidecar `/policy` API.

Patch uses merge semantics:
- Incoming rules take priority over existing rules with the same `Target`.
- Existing rules for other targets remain unchanged.
- Within a single patch payload, the first rule for a `Target` wins.
- The current `DefaultAction` is preserved.

```csharp
var policy = await sandbox.GetEgressPolicyAsync();

await sandbox.PatchEgressRulesAsync(new[]
{
    new NetworkRule { Action = NetworkRuleAction.Allow, Target = "www.github.com" },
    new NetworkRule { Action = NetworkRuleAction.Deny, Target = "pypi.org" }
});
```

### 5. Timeout and Retry Behavior

- `ConnectionConfig.RequestTimeoutSeconds` controls timeout for SDK HTTP calls.
- `RunCommandOptions.TimeoutSeconds` controls command execution timeout for command runs.
- `RunInSessionOptions.TimeoutSeconds` controls command execution timeout for session runs.
- `SandboxCreateOptions.TimeoutSeconds` controls sandbox server-side TTL.
- `ReadyTimeoutSeconds` controls how long `CreateAsync` / `ConnectAsync` waits for readiness.
- The SDK does not automatically retry failed API requests; implement retries in caller code where appropriate.

### 6. Resource Cleanup

Both `Sandbox` and `SandboxManager` implement `IAsyncDisposable`. Use `await using` or call `DisposeAsync()` when done.

```csharp
await using var sandbox = await Sandbox.CreateAsync(options);
// ... use sandbox ...
// Automatically disposed when leaving scope
```

## Error Handling

The SDK throws `SandboxException` (and derived exceptions such as `SandboxApiException`,
`SandboxReadyTimeoutException`, and `InvalidArgumentException`) when operations fail.

```csharp
try
{
    var execution = await sandbox.Commands.RunAsync("echo 'Hello Sandbox!'");
    Console.WriteLine(execution.Logs.Stdout.FirstOrDefault()?.Text);
}
catch (SandboxReadyTimeoutException)
{
    Console.Error.WriteLine("Sandbox did not become ready before the configured timeout.");
}
catch (SandboxApiException ex)
{
    Console.Error.WriteLine($"API Error: status={ex.StatusCode}, requestId={ex.RequestId}, message={ex.Message}");
}
catch (SandboxException ex)
{
    Console.Error.WriteLine($"Sandbox Error: [{ex.Error.Code}] {ex.Error.Message}");
}
```

## Supported Frameworks

- .NET Standard 2.0 (for maximum compatibility with .NET Framework 4.6.1+, .NET Core 2.0+, Mono, Xamarin, etc.)
- .NET Standard 2.1
- .NET 6.0 (LTS)
- .NET 7.0
- .NET 8.0 (LTS)
- .NET 9.0
- .NET 10.0

## License

Apache License 2.0
