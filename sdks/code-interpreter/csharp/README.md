# OpenSandbox Code Interpreter SDK for C#

English | [中文](README_zh.md)

A C# SDK for code interpretation with OpenSandbox. Provides high-level APIs for executing code in multiple languages (Python, JavaScript, TypeScript, Go, Java, Bash) within secure sandbox environments.

## Prerequisites

This SDK requires a Docker image containing the Code Interpreter runtime environment. You must use
`opensandbox/code-interpreter` (or a derivative image) with pre-installed runtimes for Python, Java, Go,
Node.js, and others.

For supported languages and versions, see the
[Environment Documentation](../../../sandboxes/code-interpreter/README.md).

## Installation

```bash
dotnet add package Alibaba.OpenSandbox.CodeInterpreter
```

## Quick Start

```csharp
using OpenSandbox;
using OpenSandbox.CodeInterpreter;
using OpenSandbox.CodeInterpreter.Models;
using OpenSandbox.Config;
using OpenSandbox.Core;

var config = new ConnectionConfig(new ConnectionConfigOptions
{
    Domain = "api.opensandbox.io",
    ApiKey = "your-api-key"
});

try
{
    // Create sandbox with code-interpreter runtime image and entrypoint.
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

    var interpreter = await CodeInterpreter.CreateAsync(sandbox);
    var execution = await interpreter.Codes.RunAsync(
        "print('Hello, World!')",
        new RunCodeOptions { Language = SupportedLanguage.Python });

    foreach (var msg in execution.Logs.Stdout)
    {
        Console.Write(msg.Text);
    }

    await sandbox.KillAsync();
}
catch (SandboxException ex)
{
    Console.Error.WriteLine($"Sandbox Error: [{ex.Error.Code}] {ex.Error.Message}");
}
```

## Logging (ILogger)

The SDK uses `Microsoft.Extensions.Logging` abstractions. Pass your own `ILoggerFactory`
through diagnostics options when creating the sandbox/code interpreter:

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
    ConnectionConfig = new ConnectionConfig(),
    Image = "opensandbox/code-interpreter:v1.0.2",
    Entrypoint = new[] { "/opt/opensandbox/code-interpreter.sh" },
    Diagnostics = new SdkDiagnosticsOptions
    {
        LoggerFactory = loggerFactory
    }
});

var interpreter = await CodeInterpreter.CreateAsync(sandbox, new CodeInterpreterCreateOptions
{
    Diagnostics = new SdkDiagnosticsOptions
    {
        LoggerFactory = loggerFactory
    }
});
```

## Runtime Configuration

### Docker Image

The Code Interpreter SDK relies on a specialized runtime image. Ensure your sandbox provider has
`opensandbox/code-interpreter` available.

### Language Version Selection

You can specify language versions through environment variables when creating the sandbox:

| Language | Environment Variable | Example Value | Default (if unset) |
| --- | --- | --- | --- |
| Python | `PYTHON_VERSION` | `3.11` | Image default |
| Java | `JAVA_VERSION` | `17` | Image default |
| Node.js | `NODE_VERSION` | `20` | Image default |
| Go | `GO_VERSION` | `1.24` | Image default |

## Features

### Run with `Language` (default language context)

If you do not need explicit context IDs, run code by setting only `Language`.
When `Context` is omitted, execd creates/reuses a default session for that language, so state can persist across runs.

```csharp
await interpreter.Codes.RunAsync(
    "x = 42",
    new RunCodeOptions { Language = SupportedLanguage.Python });

var execution = await interpreter.Codes.RunAsync(
    "result = x\nresult",
    new RunCodeOptions { Language = SupportedLanguage.Python });

Console.WriteLine(execution.Results.FirstOrDefault()?.Text); // "42"
```

### Supported Languages

- Python (`SupportedLanguage.Python`)
- JavaScript (`SupportedLanguage.JavaScript`)
- TypeScript (`SupportedLanguage.TypeScript`)
- Go (`SupportedLanguage.Go`)
- Java (`SupportedLanguage.Java`)
- Bash (`SupportedLanguage.Bash`)

### Context Management

Contexts allow you to maintain state between code executions:

```csharp
// Create a context for Python
var context = await interpreter.Codes.CreateContextAsync(SupportedLanguage.Python);

// Run code in the context - variables persist
await interpreter.Codes.RunAsync("x = 42", new RunCodeOptions { Context = context });
var result = await interpreter.Codes.RunAsync("print(x)", new RunCodeOptions { Context = context });
// Output: 42

// List contexts for a specific language
var pythonContexts = await interpreter.Codes.ListContextsAsync(SupportedLanguage.Python);

// Delete a specific context
await interpreter.Codes.DeleteContextAsync(context.Id!);

// Delete all contexts for a language
await interpreter.Codes.DeleteContextsAsync(SupportedLanguage.Python);
```

### Streaming Execution

For real-time output, use streaming:

```csharp
var request = new RunCodeRequest
{
    Code = "for i in range(5): print(i)",
    Context = new CodeContext { Language = SupportedLanguage.Python }
};

await foreach (var ev in interpreter.Codes.RunStreamAsync(request))
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
            var text = ev.Results != null
                && ev.Results.TryGetValue("text/plain", out var value)
                ? value?.ToString()
                : null;
            Console.WriteLine($"Result: {text ?? "(no text/plain)"}");
            break;
        case "error":
            Console.WriteLine($"Error: {ev.Error}");
            break;
    }
}
```

### Event Handlers

Use handlers for fine-grained control over execution events:

```csharp
var execution = await interpreter.Codes.RunAsync(
    "print('Hello')\nprint('World')",
    new RunCodeOptions
    {
        Language = SupportedLanguage.Python,
        Handlers = new ExecutionHandlers
        {
            OnStdout = async msg => Console.Write($"[OUT] {msg.Text}"),
            OnStderr = async msg => Console.Error.Write($"[ERR] {msg.Text}"),
            OnResult = async result => Console.WriteLine($"[RESULT] {result.Text}"),
            OnError = async error => Console.WriteLine($"[ERROR] {error.Name}: {error.Value}"),
            OnExecutionComplete = async complete => Console.WriteLine($"[DONE] Took {complete.ExecutionTimeMs}ms")
        }
    });
```

### Interrupt Execution

Stop a running code execution:

```csharp
var context = await interpreter.Codes.CreateContextAsync(SupportedLanguage.Python);

// Start a long-running task
var executionId = new TaskCompletionSource<string>();
var task = interpreter.Codes.RunAsync(
    "import time\nwhile True: time.sleep(1)",
    new RunCodeOptions
    {
        Context = context,
        Handlers = new ExecutionHandlers
        {
            OnInit = init =>
            {
                executionId.TrySetResult(init.Id);
                return Task.CompletedTask;
            }
        }
    });

// Interrupt after some time
await interpreter.Codes.InterruptAsync(await executionId.Task);
```

### Access Sandbox Services

The code interpreter provides convenient access to underlying sandbox services:

```csharp
// File operations
await interpreter.Files.WriteFilesAsync(new[]
{
    new WriteEntry { Path = "/tmp/data.txt", Data = "Hello, World!" }
});
var content = await interpreter.Files.ReadFileAsync("/tmp/data.txt");

// Shell commands
var commandExecution = await interpreter.Commands.RunAsync("ls -la /tmp");
foreach (var msg in commandExecution.Logs.Stdout)
{
    Console.Write(msg.Text);
}

// Metrics
var metrics = await interpreter.Sandbox.GetMetricsAsync();
Console.WriteLine($"CPU: {metrics.CpuUsedPercentage}%, Memory: {metrics.MemoryUsedMiB}MiB");
```

## API Reference

### CodeInterpreter

| Method | Description |
|--------|-------------|
| `CreateAsync(sandbox, options?)` | Creates a code interpreter from a sandbox |

| Property | Description |
|----------|-------------|
| `Sandbox` | The underlying sandbox instance |
| `Codes` | The codes service for code execution |
| `Id` | The sandbox ID |
| `Files` | File system operations |
| `Commands` | Shell command execution |
| `Metrics` | Resource metrics |

### ICodes

| Method | Description |
|--------|-------------|
| `CreateContextAsync(language)` | Creates a new execution context |
| `GetContextAsync(contextId)` | Gets an existing context |
| `ListContextsAsync(language)` | Lists contexts for a specific language |
| `DeleteContextAsync(contextId)` | Deletes a specific context |
| `DeleteContextsAsync(language)` | Deletes all contexts for a language |
| `RunAsync(code, options?)` | Executes code and returns the result |
| `RunStreamAsync(request)` | Executes code with streaming output |
| `InterruptAsync(executionId)` | Interrupts a running execution by execution ID |

> All async methods support `CancellationToken`.

## Requirements

- .NET Standard 2.0+ / .NET 6.0+
- OpenSandbox Sandbox SDK (`Alibaba.OpenSandbox`)

## Notes

- **Lifecycle**: `CodeInterpreter` wraps an existing `Sandbox` and reuses its connection and services.
- **Default context behavior**: `RunAsync(..., new RunCodeOptions { Language = ... })` uses the language default context.
- **Cleanup**: `DisposeAsync` only cleans local resources. Call `KillAsync()` to terminate the remote sandbox instance.

## License

Apache License 2.0
