# OpenSandbox Go SDK

Go client library for the [OpenSandbox](https://github.com/alibaba/OpenSandbox) API.

Covers all three OpenAPI specs:
- **Lifecycle** — Create, manage, and destroy sandbox instances
- **Execd** — Execute commands, manage files, monitor metrics inside sandboxes
- **Egress** — Inspect and mutate sandbox network policy at runtime

## Installation

```bash
# go 1.20+
go get github.com/alibaba/OpenSandbox/sdks/sandbox/go
```

## Quick Start

### Create and manage a sandbox

```go
package main

import (
    "context"
    "fmt"
    "log"

    "github.com/alibaba/OpenSandbox/sdks/sandbox/go"
)

func main() {
    ctx := context.Background()

    lc := opensandbox.NewLifecycleClient("http://localhost:8080/v1", "your-api-key")

    sbx, err := lc.CreateSandbox(ctx, opensandbox.CreateSandboxRequest{
        Image:      opensandbox.ImageSpec{URI: "python:3.12"},
        Entrypoint: []string{"/bin/sh"},
        ResourceLimits: opensandbox.ResourceLimits{
            "cpu":    "500m",
            "memory": "512Mi",
        },
    })
    if err != nil {
        log.Fatal(err)
    }
    fmt.Printf("Created sandbox: %s (state: %s)\n", sbx.ID, sbx.Status.State)

    sbx, err = lc.GetSandbox(ctx, sbx.ID)
    if err != nil {
        log.Fatal(err)
    }

    list, err := lc.ListSandboxes(ctx, opensandbox.ListOptions{
        States:   []opensandbox.SandboxState{opensandbox.StateRunning},
        PageSize: 10,
    })
    if err != nil {
        log.Fatal(err)
    }
    fmt.Printf("Running sandboxes: %d\n", list.Pagination.TotalItems)

    _ = lc.PauseSandbox(ctx, sbx.ID)
    _ = lc.ResumeSandbox(ctx, sbx.ID)

    _ = lc.DeleteSandbox(ctx, sbx.ID)
}
```

### Run a command with streaming output

```go
exec := opensandbox.NewExecdClient("http://localhost:9090", "your-execd-token")

err := exec.RunCommand(ctx, opensandbox.RunCommandRequest{
    Command: "echo 'Hello from sandbox!'",
    Timeout: 30000,
}, func(event opensandbox.StreamEvent) error {
    switch event.Event {
    case "stdout":
        fmt.Print(event.Data)
    case "stderr":
        fmt.Fprintf(os.Stderr, "%s", event.Data)
    case "execution_complete":
        fmt.Println("\n[done]")
    }
    return nil
})
```

### Check egress policy

```go
egress := opensandbox.NewEgressClient("http://localhost:18080", "your-egress-token")

policy, err := egress.GetPolicy(ctx)
fmt.Printf("Mode: %s, Default: %s\n", policy.Mode, policy.Policy.DefaultAction)

updated, err := egress.PatchPolicy(ctx, []opensandbox.NetworkRule{
    {Action: "allow", Target: "api.example.com"},
})
```

## API Reference

### LifecycleClient

Created with `NewLifecycleClient(baseURL, apiKey string, opts ...Option)`.

| Method | Description |
|--------|-------------|
| `CreateSandbox(ctx, req)` | Create a new sandbox from a container image |
| `GetSandbox(ctx, id)` | Get sandbox details by ID |
| `ListSandboxes(ctx, opts)` | List sandboxes with filtering and pagination |
| `DeleteSandbox(ctx, id)` | Delete a sandbox |
| `PauseSandbox(ctx, id)` | Pause a running sandbox |
| `ResumeSandbox(ctx, id)` | Resume a paused sandbox |
| `RenewExpiration(ctx, id, expiresAt)` | Extend sandbox expiration time |
| `GetEndpoint(ctx, sandboxID, port, useServerProxy)` | Get public endpoint for a sandbox port |
| `GetSignedEndpoint(ctx, sandboxID, port, expires)` | Get signed endpoint URL with OSEP-0011 route token |

### ExecdClient

Created with `NewExecdClient(baseURL, accessToken string, opts ...Option)`.

**Health:**
| Method | Description |
|--------|-------------|
| `Ping(ctx)` | Check server health |

**Code Execution:**
| Method | Description |
|--------|-------------|
| `ListContexts(ctx, language)` | List active code execution contexts |
| `CreateContext(ctx, req)` | Create a code execution context |
| `GetContext(ctx, contextID)` | Get context details |
| `DeleteContext(ctx, contextID)` | Delete a context |
| `DeleteContextsByLanguage(ctx, language)` | Delete all contexts for a language |
| `ExecuteCode(ctx, req, handler)` | Execute code with SSE streaming |
| `InterruptCode(ctx, sessionID)` | Interrupt running code |

**Command Execution:**
| Method | Description |
|--------|-------------|
| `CreateSession(ctx)` | Create a bash session |
| `RunInSession(ctx, sessionID, req, handler)` | Run command in session with SSE |
| `DeleteSession(ctx, sessionID)` | Delete a bash session |
| `RunCommand(ctx, req, handler)` | Run a command with SSE streaming |
| `InterruptCommand(ctx, sessionID)` | Interrupt running command |
| `GetCommandStatus(ctx, commandID)` | Get command execution status |
| `GetCommandLogs(ctx, commandID, cursor)` | Get command stdout/stderr |

**File Operations:**
| Method | Description |
|--------|-------------|
| `GetFileInfo(ctx, path)` | Get file metadata |
| `DeleteFiles(ctx, paths)` | Delete files |
| `SetPermissions(ctx, req)` | Change file permissions |
| `MoveFiles(ctx, req)` | Move/rename files |
| `SearchFiles(ctx, dir, pattern)` | Search files by glob pattern |
| `ReplaceInFiles(ctx, req)` | Text replacement in files |
| `UploadFile(ctx, file, opts)` | Upload a file to the sandbox |
| `UploadFiles(ctx, entries)` | Upload multiple files to the sandbox |
| `DownloadFile(ctx, remotePath, rangeHeader)` | Download a file from the sandbox |

**Directory Operations:**
| Method | Description |
|--------|-------------|
| `CreateDirectory(ctx, path, mode)` | Create a directory (mkdir -p) |
| `DeleteDirectory(ctx, path)` | Delete a directory recursively |

**Metrics:**
| Method | Description |
|--------|-------------|
| `GetMetrics(ctx)` | Get system resource metrics |
| `WatchMetrics(ctx, handler)` | Stream metrics via SSE |

### EgressClient

Created with `NewEgressClient(baseURL, authToken string, opts ...Option)`.

| Method | Description |
|--------|-------------|
| `GetPolicy(ctx)` | Get current egress policy |
| `PatchPolicy(ctx, rules)` | Merge rules into current policy |

## SSE Streaming

Methods that stream output (`RunCommand`, `ExecuteCode`, `RunInSession`, `WatchMetrics`) accept an `EventHandler` callback:

```go
type EventHandler func(event StreamEvent) error
```

Each `StreamEvent` contains:
- `Event` — the event type (e.g. `"stdout"`, `"stderr"`, `"result"`, `"execution_complete"`). For NDJSON streams, this is extracted from the JSON `type` field automatically.
- `Data` — the raw event payload (JSON string for NDJSON streams).
- `ID` — optional event identifier

Return a non-nil error from the handler to stop processing the stream early.

## Client Options

All client constructors accept optional `Option` functions:

```go
client := opensandbox.NewLifecycleClient(url, key,
    opensandbox.WithHTTPClient(myHTTPClient),
)

client := opensandbox.NewExecdClient(url, token,
    opensandbox.WithTimeout(60 * time.Second),
)
```

SDK-created HTTP clients enforce NIST 2030 minimum TLS certificate strength by default
(RSA >= 2048, EC >= 224, DSA P >= 2048/Q >= 224, hash >= 224). If you must interoperate
with legacy endpoints, set `AllowWeakServerCertKeyLengths: true` in `TransportConfig`.

## Error Handling

Non-2xx responses are returned as `*opensandbox.APIError`:

```go
_, err := lc.GetSandbox(ctx, "nonexistent")
if apiErr, ok := err.(*opensandbox.APIError); ok {
    fmt.Printf("HTTP %d: %s — %s\n", apiErr.StatusCode, apiErr.Response.Code, apiErr.Response.Message)
}
```

## License

Apache 2.0
