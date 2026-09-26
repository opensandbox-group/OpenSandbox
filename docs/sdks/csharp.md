---
title: C# SDK
description: C# SDK for creating, managing, and interacting with secure OpenSandbox environments.
---

# OpenSandbox SDK for C\#

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

Run this example in a .NET 8+ console project with implicit usings enabled. It
creates a sandbox, runs a shell command, and releases remote and local resources.

::: tip
Before running this example, ensure the OpenSandbox service is running. See the [Getting Started](/getting-started/) guide for startup instructions.
:::

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

    try
    {
        var execution = await sandbox.Commands.RunAsync("echo 'Hello Sandbox!'");
        Console.WriteLine(execution.Logs.Stdout.FirstOrDefault()?.Text);
    }
    finally
    {
        await sandbox.KillAsync();
    } // await using releases the client even if KillAsync fails.
}
catch (SandboxException ex)
{
    Console.Error.WriteLine($"Sandbox Error: [{ex.Error.Code}] {ex.Error.Message}");
    Console.Error.WriteLine($"Request ID: {ex.RequestId}");
}
```

## Lifecycle Hooks

Set `Lifecycle` in `SandboxCreateOptions`. `PreStart` completes before the entrypoint starts, while `Periodic` hooks run on their schedules after startup.

```csharp
using OpenSandbox.Models;

await using var sandbox = await Sandbox.CreateAsync(new SandboxCreateOptions
{
    ConnectionConfig = config,
    Image = "ubuntu:24.04",
    Lifecycle = new SandboxLifecycle
    {
        PreStart = new LifecycleHook
        {
            Command = new[] { "sh", "-c", "echo ready > /tmp/prestart.done" },
            TimeoutSeconds = 120,
        },
        Periodic = new[]
        {
            new PeriodicLifecycleHook
            {
                Name = "checkpoint",
                Schedule = "@every 5m",
                Command = new[] { "sh", "-c", "date -u >> /tmp/checkpoints.log" },
                TimeoutSeconds = 120,
            },
        },
    },
});
```

The Server validates `TimeoutSeconds`; `PreStart` accepts 1–10800 seconds, while `Periodic` accepts 1–300 seconds. Both default to 60 seconds when omitted. See [Lifecycle Hooks](/guides/lifecycle-hooks) for timing, failure behavior, and provider limitations.

## Usage Examples

The snippets below use `config` and a live `sandbox` from the quick start. Run
them before its cleanup block, with `using OpenSandbox.Models;` at the top of the
file. Creation examples are alternatives. `await using` releases local clients;
call `KillAsync()` to terminate sandboxes that are no longer needed.

### 1. Lifecycle Management

Manage the sandbox lifecycle, including renewal, pausing, and resuming.

```csharp
var info = await sandbox.GetInfoAsync();
Console.WriteLine($"State: {info.Status.State}");
Console.WriteLine($"Created: {info.CreatedAt}");
Console.WriteLine($"Expires: {info.ExpiresAt}"); // null when manual cleanup mode is used

await sandbox.PauseAsync();

using var pauseBudget = new CancellationTokenSource(TimeSpan.FromMinutes(2));
while (true)
{
    var current = await sandbox.GetInfoAsync(pauseBudget.Token);
    if (current.Status.State == SandboxStates.Paused) break;
    if (current.Status.State == "Failed")
        throw new InvalidOperationException(current.Status.Message);
    await Task.Delay(1000, pauseBudget.Token);
}

// Resume returns a fresh local handle.
await using var resumed = await sandbox.ResumeAsync();

// Renew: expiresAt = now + timeoutSeconds
await resumed.RenewAsync(30 * 60);
```

Pause is asynchronous and runtime-dependent. See [Pause and Resume](/guides/pause-resume).

Create a non-expiring sandbox by setting `ManualCleanup = true`:

```csharp
var manual = await Sandbox.CreateAsync(new SandboxCreateOptions
{
    ConnectionConfig = config,
    Image = "ubuntu",
    ManualCleanup = true,
});
```

::: info
`TimeoutSeconds = null` uses the default TTL. Set `ManualCleanup = true` to disable
automatic expiration.
:::

### Connect to an Existing Sandbox

Use `ConnectAsync` when you already have a sandbox ID and need a new SDK instance bound to it.

```csharp
await using var connected = await Sandbox.ConnectAsync(new SandboxConnectOptions
{
    SandboxId = "existing-sandbox-id",
    ConnectionConfig = config
});
```

### 2. Custom Health Check

Resolving an endpoint confirms that a route exists; it does not confirm that the
application on that port is healthy. For service readiness, make a bounded request
to the application's health endpoint and include the returned endpoint headers.

Define custom logic to determine whether the sandbox is ready/healthy.

```csharp
using var http = new HttpClient { Timeout = TimeSpan.FromSeconds(2) };
await using var sandbox = await Sandbox.CreateAsync(new SandboxCreateOptions
{
    ConnectionConfig = config,
    Image = "nginx:latest",
    Entrypoint = new[] { "nginx", "-g", "daemon off;" },
    HealthCheck = async (sbx) =>
    {
        var ep = await sbx.GetEndpointAsync(80);
        using var request = new HttpRequestMessage(HttpMethod.Get, await sbx.GetEndpointUrlAsync(80));
        foreach (var header in ep.Headers)
            request.Headers.TryAddWithoutValidation(header.Key, header.Value);
        try
        {
            using var response = await http.SendAsync(request);
            return response.StatusCode == System.Net.HttpStatusCode.OK;
        }
        catch (HttpRequestException) { return false; }
        catch (TaskCanceledException) { return false; }
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

To execute a native program without shell parsing, pass an argument list. On Linux,
this example prints literal `$HOME` and keeps `hello world` as one argument:

```csharp
await sandbox.Commands.RunAsync(new[] { "printf", "%s\n", "$HOME", "hello world" });
```

Native argv execution requires an updated execd. See [command execution modes](/architecture/data-plane/execd#command-execution) for executable lookup and platform behavior.

#### Background commands

Poll status and incremental logs. Command timeout is separate from sandbox TTL.

```csharp
var execution = await sandbox.Commands.RunAsync(
    "for i in 1 2 3; do echo step-$i; sleep 1; done",
    options: new RunCommandOptions
    {
        Background = true,
        TimeoutSeconds = 30,
    });

var commandId = execution.Id ?? throw new InvalidOperationException("No command ID returned");
long cursor = 0;
using var budget = new CancellationTokenSource(TimeSpan.FromSeconds(45));
try
{
    while (true)
    {
        var status = await sandbox.Commands.GetCommandStatusAsync(commandId, budget.Token);
        var logs = await sandbox.Commands.GetBackgroundCommandLogsAsync(commandId, cursor, budget.Token);
        Console.Write(logs.Content);
        cursor = logs.Cursor ?? cursor;
        if (status.Running == false)
        {
            if (status.ExitCode != 0)
                throw new InvalidOperationException($"Command failed: {status.ExitCode}, {status.Error}");
            break;
        }
        await Task.Delay(500, budget.Token);
    }
}
catch (OperationCanceledException) when (budget.IsCancellationRequested)
{
    await sandbox.Commands.InterruptAsync(commandId);
    throw;
}
```

#### Persistent shell sessions

A Bash session preserves shell variables and the working directory across commands.

```csharp
var sessionId = await sandbox.Commands.CreateSessionAsync(
    new CreateSessionOptions { WorkingDirectory = "/tmp" });
try
{
    await sandbox.Commands.RunInSessionAsync(sessionId, "export DEMO=hello");
    var result = await sandbox.Commands.RunInSessionAsync(sessionId, "echo \"$DEMO\"; pwd");
    foreach (var message in result.Logs.Stdout) Console.Write(message.Text);
}
finally
{
    await sandbox.Commands.DeleteSessionAsync(sessionId);
}
```

#### Persistent environment variables

Set environment variables that the runtime injects into every subsequent command
and session — without hand-writing shell escaping against the sandbox env file.

```csharp
await sandbox.Commands.SetEnvAsync("MY_TOKEN", "it's a safe value");
```

Keys must match `[A-Za-z_][A-Za-z0-9_]*`. Values without a single quote are
stored verbatim; values containing a single quote use the env file's
double-quoted form, in which shell-style `$NAME` sequences may be expanded
when the runtime loads the file. The env file is append-only: the
last write for a key wins. Throws `SandboxException` if the sandbox fails to
persist the variable.

For filesystem/process isolation within a sandbox, see
[Isolation Sessions](/guides/isolation-sessions). These are separate from Bash sessions.

### 4. File Operations

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

var entries = await sandbox.Files.ListDirectoryAsync("/tmp/demo", depth: 1);
foreach (var entry in entries) Console.WriteLine(entry.Path);

var files = await sandbox.Files.SearchAsync(new SearchEntry { Path = "/tmp/demo", Pattern = "*.txt" });
foreach (var file in files)
{
    Console.WriteLine(file.Path);
}

await sandbox.Files.DeleteFilesAsync(new[] { "/tmp/demo/hello.txt" });
await sandbox.Files.DeleteDirectoriesAsync(new[] { "/tmp/demo" });
```

For binary data, use `byte[]` or `Stream` in `WriteEntry.Data`, and download with
`ReadBytesAsync()` or `ReadBytesStreamAsync()`. Read options support partial downloads.

### 5. Endpoints

`GetEndpointAsync()` returns an endpoint **without a scheme** (for example `"localhost:44772"`). Use `GetEndpointUrlAsync()` if you want a ready-to-use absolute URL.

```csharp
var endpoint = await sandbox.GetEndpointAsync(44772);
Console.WriteLine(endpoint.EndpointAddress);

var url = await sandbox.GetEndpointUrlAsync(44772);
Console.WriteLine(url); // e.g., "http://localhost:44772"
```

Forward `endpoint.Headers` on requests to the sandbox service, including any
secure-access credentials. The health-check example above shows an HTTP request
to an application listening on the target port.

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
    Page = 1, // First page only; increase for subsequent pages.
    PageSize = 10
});

foreach (var s in list.Items)
{
    Console.WriteLine(s.Id);
}
```

### Resource metrics

Read current sandbox resource usage with `await sandbox.Metrics.GetMetricsAsync()`.
This is separate from [SDK creation telemetry](/sdks/observability#creation-metrics).

## Snapshots, templates, and metadata

| Operation | Public API |
| --- | --- |
| Snapshot a sandbox | `sandbox.CreateSnapshotAsync` or `manager.CreateSnapshotAsync` |
| Inspect/list/delete snapshots | `manager.GetSnapshotAsync`, `ListSnapshotsAsync`, `DeleteSnapshotAsync` |
| Restore a snapshot | `Sandbox.CreateAsync` with `SnapshotId` and no `Image`/`Entrypoint` |
| Manage Fsb templates | `manager.CreateTemplateAsync`, `GetTemplateAsync`, `ListTemplatesAsync`, `DeleteTemplateAsync` |
| Create from a published template | `Sandbox.CreateFromTemplateAsync` with `SandboxCreateFromTemplateOptions` |
| Patch metadata | `sandbox.PatchMetadataAsync` or `manager.PatchSandboxMetadataAsync` |

Poll snapshot status before restoring. Template builds are asynchronous; wait
for `Succeeded` before use. Template-backed creation requires `TimeoutSeconds`
and inherits workload configuration from the published template.
Metadata patch values add/replace keys; `null` deletes a key.
See the [lifecycle contract](/api/#1-sandbox-lifecycle-yml) for backend constraints.

Snapshot support depends on the runtime and server configuration. Renew the source
sandbox first if its remaining TTL may expire during snapshot creation. This example
waits up to 15 minutes and retains the snapshot for reuse:

```csharp
await using var manager = SandboxManager.Create(new SandboxManagerOptions { ConnectionConfig = config });
var snapshot = await sandbox.CreateSnapshotAsync("demo");
Console.WriteLine($"Snapshot: {snapshot.Id}");
using var budget = new CancellationTokenSource(TimeSpan.FromMinutes(15));
while (true)
{
    snapshot = await manager.GetSnapshotAsync(snapshot.Id, budget.Token);
    if (snapshot.Status.State == "Ready") break;
    if (snapshot.Status.State == "Failed")
        throw new InvalidOperationException(snapshot.Status.Message);
    await Task.Delay(2000, budget.Token);
}
await using var restored = await Sandbox.CreateAsync(new SandboxCreateOptions
{
    SnapshotId = snapshot.Id,
    ConnectionConfig = config,
});
try { Console.WriteLine(restored.Id); }
finally { await restored.KillAsync(); }
// When no longer needed: await manager.DeleteSnapshotAsync(snapshot.Id);
```

Use an existing template after its build reaches `Succeeded`:

```csharp
await using var templated = await Sandbox.CreateFromTemplateAsync(new SandboxCreateFromTemplateOptions
{
    TemplateId = "your-published-template-id",
    TimeoutSeconds = 600,
    ConnectionConfig = config,
});
```

Add/replace a metadata key and remove another:

```csharp
await sandbox.PatchMetadataAsync(new Dictionary<string, string?>
{
    ["project"] = "demo",
    ["obsolete-key"] = null,
});
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
| `DisableMetrics` | Disable SDK create-latency telemetry (see [SDK Telemetry](/sdks/observability#creation-metrics)) | `false` | `OPENSANDBOX_DISABLE_METRICS` |

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

::: tip SDK Telemetry
`Sandbox.CreateAsync` reports create latency to `POST /v1/metrics/events` by default. Set `ConnectionConfigOptions.DisableMetrics = true` or export `OPENSANDBOX_DISABLE_METRICS=1` to opt out. See [SDK Telemetry](/sdks/observability#creation-metrics).
:::

### 2. SDK Logging

The SDK uses `Microsoft.Extensions.Logging` abstractions. `SdkDiagnosticsOptions`
configures local logging; it does not retrieve remote sandbox diagnostic logs or
events. Use the [CLI or HTTP API](/api/#diagnostics) for those. Client Pool and
built-in pool warmup tracing are not currently available in C#.
Install `Microsoft.Extensions.Logging.Console` to use `AddConsole()` below.

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
| `Image` | Docker image to use | One of image or snapshot ID |
| `TimeoutSeconds` | Automatic termination timeout (server-side TTL) | 10 minutes |
| `Entrypoint` | Container entrypoint command | `["tail","-f","/dev/null"]` |
| `Resource` | CPU and memory limits (string map) | `{"cpu":"1","memory":"2Gi"}` |
| `Env` | Environment variables | `{}` |
| `Metadata` | Custom metadata tags | `{}` |
| `NetworkPolicy` | Optional outbound network policy (egress) | - |
| `CredentialProxy` | Optional Credential Vault proxy startup settings | - |
| `Volumes` | Optional storage mounts (`Host` / `PVC` / `OSSFS`, supports `ReadOnly` and `SubPath`) | - |
| `Extensions` | Extra server-defined fields | `{}` |
| `SkipHealthCheck` | Skip readiness checks (`Running` + health check) | `false` |
| `HealthCheck` | Custom readiness check | - |
| `ReadyTimeoutSeconds` | Max time to wait for readiness | 30 seconds |
| `HealthCheckPollingInterval` | Poll interval while waiting (milliseconds) | 200 ms |
| `SnapshotId` | Restore a snapshot instead of passing `Image`; omit `Entrypoint` | - |
| `ResourceRequests` | Kubernetes resource requests; must not exceed limits | - |
| `Lifecycle` | Pre-start and periodic hooks | - |
| `Platform` | OS/architecture constraint | - |
| `SecureAccess` | Require endpoint access credentials | `false` |
| `ManualCleanup` | Disable TTL expiration for image/snapshot creation | `false` |

::: warning
Metadata keys under `opensandbox.io/` are reserved for system-managed labels and will be rejected by the server.
:::

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

Runtime egress policy routing depends on the sandbox origin.
For image-backed sandboxes, the SDK resolves port `18080` and calls the sidecar
`/policy` API. For template-backed sandboxes (including restored template snapshots),
the SDK detects `OPEN-SANDBOX-ORIGIN: template` and routes policy operations through
the lifecycle `/sandboxes/{sandboxId}/networkpolicy` API.

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

### 5. Credential Vault

Credential Vault requires a sandbox-side egress service and is unavailable for
template-backed sandboxes. It injects outbound credentials from the egress sidecar while
keeping real secrets out of sandbox environment variables, commands, files, and
logs. Create the sandbox with `CredentialProxy` enabled, then write credentials
and bindings through `sandbox.CredentialVault` or the sandbox helper methods.

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
            new() { Action = NetworkRuleAction.Allow, Target = "api.example.com" }
        }
    },
    CredentialProxy = new CredentialProxyConfig { Enabled = true }
});

await sandbox.CreateCredentialVaultAsync(
    new[]
    {
        new Credential
        {
            Name = "api-token",
            Source = new InlineCredentialSource { Value = "<token>" }
        }
    },
    new[]
    {
        new CredentialBinding
        {
            Name = "api-token",
            Match = new CredentialMatch
            {
                Schemes = new[] { "https" },
                Hosts = new[] { "api.example.com" },
                Paths = new[] { "/v1/*" }
            },
            Auth = new CredentialAuth
            {
                Type = "apiKey",
                Name = "x-api-key",
                Credential = "api-token"
            }
        }
    });
```

See [Credential Vault](/guides/credential-vault) for auth types, binding
guidance, and Git/curl examples.

### 6. Timeout and Retry Behavior

- `ConnectionConfig.RequestTimeoutSeconds` controls timeout for SDK HTTP calls.
- `RunCommandOptions.TimeoutSeconds` controls command execution timeout for command runs.
- `RunInSessionOptions.TimeoutSeconds` controls command execution timeout for session runs.
- `SandboxCreateOptions.TimeoutSeconds` controls sandbox server-side TTL.
- `ReadyTimeoutSeconds` controls readiness waits. For `ConnectAsync` / `ResumeAsync`, endpoint discovery and health checks share this timeout. Blocking custom code can delay timeout reporting.
- The SDK does not automatically retry failed API requests; implement retries in caller code where appropriate.

### 7. Resource Cleanup

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
- .NET 6.0
- .NET 7.0
- .NET 8.0
- .NET 9.0
- .NET 10.0

## License

Apache License 2.0
