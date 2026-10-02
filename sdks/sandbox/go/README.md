# OpenSandbox Go SDK

Go client library for the [OpenSandbox](https://github.com/opensandbox-group/OpenSandbox) API.

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
        Image:      &opensandbox.ImageSpec{URI: "python:3.12"},
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
exec := opensandbox.NewExecdClient("http://localhost:44772", "your-execd-token")

err := exec.RunCommand(ctx, opensandbox.RunCommandRequest{
    Command: "echo 'Hello from sandbox!'",
    Timeout: 30000, // milliseconds
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

### Create and restore snapshots

```go
// Create a snapshot from a running sandbox, then wait until it is Ready
mgr := opensandbox.NewSandboxManager(config)
snap, err := mgr.CreateSnapshot(ctx, sandboxID, opensandbox.CreateSnapshotRequest{
    Name: "pre-migration",
})
if err != nil {
    log.Fatal(err)
}

info, err := mgr.GetSnapshot(ctx, snap.ID)
for err == nil && info.Status.State == opensandbox.SnapshotStateCreating {
    select {
    case <-ctx.Done():
        log.Fatal(ctx.Err())
    case <-time.After(2 * time.Second):
        info, err = mgr.GetSnapshot(ctx, snap.ID)
    }
}
if err != nil {
    log.Fatal(err)
}
if info.Status.State != opensandbox.SnapshotStateReady {
    log.Fatalf("snapshot not Ready: state=%s reason=%s", info.Status.State, info.Status.Reason)
}

page, err := mgr.ListSnapshots(ctx, opensandbox.ListSnapshotsOptions{})

// Restore: create a new sandbox FROM a snapshot (Image and SnapshotID are
// mutually exclusive — set exactly one)
restored, err := lc.CreateSandbox(ctx, opensandbox.CreateSandboxRequest{
    SnapshotID: snap.ID,
    ResourceLimits: opensandbox.ResourceLimits{
        "cpu":    "500m",
        "memory": "512Mi",
    },
})
if err != nil {
    log.Fatal(err)
}
_ = restored

// Only delete the snapshot after the restore has succeeded
_ = mgr.DeleteSnapshot(ctx, snap.ID)
```

### Run code in an isolated session

Isolated sessions run multi-step code in a hardened, resource-bounded
environment with bind mounts — reachable through `Sandbox.IsolationCreate`:

```go
shareNet := true
ephemeral := false
session, err := sbx.IsolationCreate(ctx, opensandbox.CreateIsolatedSessionRequest{
    // Workspace is a pointer; Overlays carries additional independent mounts.
    Workspace: &opensandbox.IsolatedWorkspaceSpec{Path: "/workspace", Mode: "rw"},
    Overlays: []opensandbox.IsolatedOverlaySpec{{
        Path:    "/data/scratch",
        Mode:    "overlay",
        Persist: &ephemeral, // tmpfs upper: writes die with the session
    }},
    Profile:  "strict",
    // Optional bind mounts (source on host, dest inside the session)
    Binds:    []opensandbox.BindMount{{Source: "/data", Dest: "/data", ReadOnly: true}},
    ShareNet: &shareNet,
})

// Foreground run — TimeoutSeconds applies here only; background runs are
// deliberately not time-limited
run, err := session.Run(ctx, opensandbox.IsolatedRunRequest{
    Code:           "python -c 'print(1+1)'",
    TimeoutSeconds: 30,
}, nil)
for _, out := range run.Stdout {
    fmt.Println(out.Text)
}

// Background runs: start, poll until finished, then fetch logs
bg, err := session.RunBackground(ctx, "make build")
status, err := session.GetRunStatus(ctx, bg.RunID)
for err == nil && status.Running {
    select {
    case <-ctx.Done():
        log.Fatal(ctx.Err())
    case <-time.After(2 * time.Second):
        status, err = session.GetRunStatus(ctx, bg.RunID)
    }
}
logs, _, err := session.GetRunLogs(ctx, bg.RunID, 0)

_ = session.Delete(ctx)
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

### Use Credential Vault

Credential Vault injects outbound credentials from the egress sidecar while
keeping real secrets out of sandbox environment variables, commands, files, and
logs. Create the sandbox with `CredentialProxy` enabled, then write credentials
and bindings through the sandbox helpers or `EgressClient`.

```go
sandbox, err := opensandbox.CreateSandbox(ctx, config, opensandbox.SandboxCreateOptions{
    Image: "python:3.11",
    NetworkPolicy: &opensandbox.NetworkPolicy{
        DefaultAction: "deny",
        Egress: []opensandbox.NetworkRule{
            {Action: "allow", Target: "api.example.com"},
        },
    },
    CredentialProxy: &opensandbox.CredentialProxyConfig{Enabled: true},
})
if err != nil {
    return err
}

_, err = sandbox.CreateCredentialVault(ctx, opensandbox.CredentialVaultCreateRequest{
    Credentials: []opensandbox.Credential{
        {
            Name: "api-token",
            Source: opensandbox.InlineCredentialSource{
                Type:  opensandbox.CredentialSourceInline,
                Value: "<token>",
            },
        },
    },
    Bindings: []opensandbox.CredentialBinding{
        {
            Name: "api-token",
            Match: opensandbox.CredentialMatch{
                Schemes: []opensandbox.CredentialScheme{opensandbox.CredentialSchemeHTTPS},
                Hosts:   []string{"api.example.com"},
                Paths:   []string{"/v1/*"},
            },
            Auth: opensandbox.CredentialAuth{
                Type:       opensandbox.CredentialAuthAPIKey,
                Name:       "x-api-key",
                Credential: "api-token",
            },
        },
    },
})
```

See [Credential Vault](../../../docs/guides/credential-vault.md) for auth types,
binding guidance, and Git/curl examples.

### Work with files

File helpers live on `Sandbox` (they wrap the `ExecdClient` file operations
with the sandbox's own access token):

```go
// The examples below need a live handle; the quick start above left `sbx`
// as a *SandboxInfo, which has no file methods.
sbx, err := opensandbox.CreateSandbox(ctx, config, opensandbox.SandboxCreateOptions{
    Image: "python:3.11",
})

// Upload (single or multipart-batch). Metadata.Path is the destination and
// is required — FileName alone is just the multipart filename.
err = sbx.UploadFile(ctx, bytes.NewReader(data), opensandbox.UploadFileOptions{
    FileName: "app.py",
    Metadata: opensandbox.FileMetadata{Path: "/workspace/app.py"},
})
err = sbx.UploadFiles(ctx, []opensandbox.UploadFileEntry{
    {File: f1, Options: opensandbox.UploadFileOptions{
        FileName: "a.txt", Metadata: opensandbox.FileMetadata{Path: "/workspace/a.txt"},
    }},
    {File: f2, Options: opensandbox.UploadFileOptions{
        FileName: "b.txt", Metadata: opensandbox.FileMetadata{Path: "/workspace/b.txt"},
    }},
})

// Download: full file, byte-range, or line-based. Each call returns its
// own reader — close each one (a single shared defer would leak the first two).
full, err := sbx.DownloadFile(ctx, "/var/log/app.log", "") // full
defer full.Close()
partial, err := sbx.DownloadFile(ctx, "/var/log/app.log", "bytes=0-1023") // partial
defer partial.Close()
lines, err := sbx.DownloadFile(ctx, "/var/log/app.log", "",
    opensandbox.DownloadFileOptions{Offset: 10, Limit: 50}) // lines 10-59
defer lines.Close()

// List, inspect, search
entries, err := sbx.ListDirectory(ctx, "/workspace")
deep, err := sbx.ListDirectoryWithDepth(ctx, "/workspace", 3) // 0 returns empty
hits, err := sbx.SearchFiles(ctx, "/workspace", "*.json")
info, err := sbx.GetFileInfo(ctx, "/workspace/app.py")

// Directories, moves, permissions
// mode is octal digits packed into a decimal int: pass 755, or
// opensandbox.OctalMode(0o755) when starting from a Go FileMode.
err = sbx.CreateDirectory(ctx, "/workspace/out", 755)
err = sbx.MoveFiles(ctx, opensandbox.MoveRequest{...})
err = sbx.SetPermissions(ctx, opensandbox.PermissionsRequest{...})
err = sbx.DeleteFiles(ctx, []string{"/workspace/tmp.log"})
err = sbx.DeleteDirectory(ctx, "/workspace/out")

// Text replacement across files
replaceReq := opensandbox.ReplaceRequest{
    "/workspace/config.yaml": {Old: "debug: true", New: "debug: false"},
}
err = sbx.ReplaceInFiles(ctx, replaceReq)
detailed, err := sbx.ReplaceInFilesDetailed(ctx, replaceReq) // per-file ReplacedCount
```

### Mount volumes into a sandbox

`Volumes` supports `host`, `pvc`, and `ossfs` backends. Each volume must
specify exactly one backend:

```go
sandbox, err := opensandbox.CreateSandbox(ctx, config, opensandbox.SandboxCreateOptions{
    Image: "ubuntu",
    Volumes: []opensandbox.Volume{
        {
            Name:     "oss-data",
            OSSFS: &opensandbox.OSSFS{
                Bucket:          "bucket-a",
                Endpoint:        "oss-cn-hangzhou.aliyuncs.com",
                AccessKeyID:     os.Getenv("OSS_ACCESS_KEY_ID"),
                AccessKeySecret: os.Getenv("OSS_ACCESS_KEY_SECRET"),
            },
            MountPath: "/mnt/oss",
        },
    },
})
```

### Build reusable base images with templates

`SandboxManager` builds golden images ("templates") from an OCI image and
publishes them to an S3-compatible target; sandboxes then boot from a
`TemplateID` instead of a plain image:

```go
mgr := opensandbox.NewSandboxManager(config)

tpl, err := mgr.CreateTemplate(ctx, opensandbox.CreateTemplateRequest{
    Image:   "python:3.11",
    Publish: "s3://bucket/templates/py311",
    ResourceLimits: map[string]string{
        "cpu": "1", "memory": "512Mi", "disk": "2Gi",
    },
})
// Poll GetTemplate until the build finishes. Status is a TemplateStatus
// struct — compare its Phase field, not a string (bound the loop in real
// code; a failed build never reaches Succeeded):
for tpl.Status.Phase != opensandbox.TemplatePhaseSucceeded {
    time.Sleep(10 * time.Second)
    tpl, err = mgr.GetTemplate(ctx, tpl.TemplateID)
}

// Boot from the template: template-based creation requires an explicit
// timeout — the server rejects creation without one.
sandbox, err := opensandbox.CreateSandboxFromTemplate(ctx, config, tpl.TemplateID,
    opensandbox.SandboxFromTemplateOptions{TimeoutSeconds: 3600})

list, err := mgr.ListTemplates(ctx, opensandbox.ListTemplatesOptions{})
err = mgr.DeleteTemplate(ctx, tpl.TemplateID)
```

### Wait for readiness with a custom health check

`WaitUntilReady` polls until the sandbox is ready — by default the execd
`/ping`; a custom `HealthCheck` replaces that (the JS SDK's custom health
check equivalent):

```go
err := sbx.WaitUntilReady(ctx, opensandbox.ReadyOptions{
    Timeout:         60 * time.Second,
    PollingInterval: 2 * time.Second,
    HealthCheck: func(ctx context.Context, sb *opensandbox.Sandbox) (bool, error) {
        out, err := sb.RunCommand(ctx, "curl -fsS http://localhost:8080/healthz", nil)
        return err == nil && out.ExitCode != nil && *out.ExitCode == 0, nil
    },
})
```

### Release idle pool sandboxes

`ReleaseAllIdle(ctx)` preserves the original fire-and-forget behavior: it drains
idle IDs and returns after scheduling best-effort kills. Call
`ReleaseAllIdleParallel(ctx, maxWorkers)` on `*DefaultSandboxPool` to bound kill
concurrency and wait until every drained ID has received a kill attempt.
`maxWorkers` must be positive. The parallel method is intentionally not part of
the `SandboxPool` interface, so existing interface implementors remain compatible.

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
| `RenewExpiration(ctx, id, expiresAt)` | Set sandbox expiration to a future timestamp (may shorten or extend) |
| `GetEndpoint(ctx, sandboxID, port, useServerProxy)` | Get public endpoint for a sandbox port |
| `GetSignedEndpoint(ctx, sandboxID, port, expires)` | Get signed endpoint URL with OSEP-0011 route token |
| `PatchSandboxMetadata(ctx, id, patch)` | Add/remove sandbox metadata keys |
| `CreateSnapshot(ctx, sandboxID, req)` | Create a snapshot of a sandbox |
| `GetSnapshot(ctx, id)` | Get snapshot details by ID |
| `ListSnapshots(ctx, opts)` | List snapshots with filtering and pagination |
| `DeleteSnapshot(ctx, id)` | Delete a snapshot |
| `CreateTemplate(ctx, req)` | Declare a fsb template (asynchronous build) |
| `GetTemplate(ctx, id)` | Get template with latest build status |
| `ListTemplates(ctx, opts)` | List templates with filtering and pagination |
| `DeleteTemplate(ctx, id)` | Delete a template |
| `GetNetworkPolicy(ctx, sandboxID)` | Get the network policy of a sandbox |
| `PatchNetworkPolicy(ctx, sandboxID, rules)` | Merge rules into a sandbox's network policy |
| `DeleteNetworkPolicyRules(ctx, sandboxID, targets)` | Remove rules from a sandbox's network policy |

### SandboxManager

Created with `NewSandboxManager(config ConnectionConfig)`. Administrative
operations on sandboxes without connecting to a specific one; every method is a
thin wrapper over the corresponding `LifecycleClient` call (see table above),
plus `Close()` (a no-op kept for symmetry — it does not terminate
sandboxes). Manager operations act across sandboxes by ID (the quick-start
snapshot example is one of these).

**Snapshots:**
| Method | Description |
|--------|-------------|
| `CreateSnapshot(ctx, sandboxID, req)` | Create a snapshot from a running sandbox |
| `GetSnapshot(ctx, snapshotID)` | Get snapshot details (poll `Status.State` for `Ready`) |
| `ListSnapshots(ctx, filter)` | List snapshots with filtering |
| `DeleteSnapshot(ctx, snapshotID)` | Delete a snapshot |

**Templates (golden images):**
| Method | Description |
|--------|-------------|
| `CreateTemplate(ctx, req)` | Build a reusable golden image from an OCI image and publish it |
| `GetTemplate(ctx, templateID)` | Get template details and build status |
| `ListTemplates(ctx, opts)` | List templates |
| `DeleteTemplate(ctx, templateID)` | Delete a template |

**Sandbox administration:**
| Method | Description |
|--------|-------------|
| `ListSandboxInfos(ctx, filter)` | List sandboxes with filtering and pagination |
| `GetSandboxInfo(ctx, sandboxID)` | Get sandbox details by ID |
| `PatchSandboxMetadata(ctx, sandboxID, patch)` | Patch sandbox metadata |
| `PauseSandbox(ctx, sandboxID)` | Pause a running sandbox |
| `ResumeSandbox(ctx, sandboxID)` | Resume a paused sandbox |
| `KillSandbox(ctx, sandboxID)` | Force-terminate a sandbox |
| `RenewSandbox(ctx, sandboxID, duration)` | Extend a sandbox's expiration |
| `Close()` | No-op; does not terminate sandboxes |


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
| `CreateSession(ctx, opts...)` | Create a bash session (optional `CreateSessionRequest` sets the working directory) |
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
| `ListDirectory(ctx, path)` | List immediate directory contents (server-side default depth) |
| `ListDirectoryWithDepth(ctx, path, depth)` | List directory contents up to the given depth (`0` returns empty) |
| `ReplaceInFiles(ctx, req)` | Text replacement in files |
| `ReplaceInFilesDetailed(ctx, req)` | Text replacement returning per-file replacement counts |
| `UploadFile(ctx, file, opts)` | Upload a file to the sandbox |
| `UploadFiles(ctx, entries)` | Upload multiple files to the sandbox |
| `DownloadFile(ctx, remotePath, rangeHeader, opts...)` | Download a file from the sandbox (optional `DownloadFileOptions` reads by line) |

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
| `CreateCredentialVault(ctx, req)` | Create sandbox-local Credential Vault state |
| `GetCredentialVault(ctx)` | Get sanitized Credential Vault state |
| `PatchCredentialVault(ctx, req)` | Atomically mutate credentials and bindings |
| `DeleteCredentialVault(ctx)` | Delete sandbox-local Credential Vault state |
| `ListCredentialVaultCredentials(ctx)` | List sanitized credential metadata |
| `GetCredentialVaultCredential(ctx, name)` | Get sanitized metadata for one credential |
| `ListCredentialVaultBindings(ctx)` | List sanitized binding metadata |
| `GetCredentialVaultBinding(ctx, name)` | Get sanitized metadata for one binding |

### Sandbox helpers

Convenience methods on `*Sandbox` (the object `CreateSandbox` returns) — they
wrap the underlying clients with the sandbox's own credentials:

| Method | Description |
|--------|-------------|
| `ID()` | The sandbox ID |
| `Origin()` | Origin reported by the server: `SandboxOriginTemplate`, or `SandboxOriginUnknown` |
| `GetInfo(ctx)` | Fetch the current `SandboxInfo` |
| `WaitUntilReady(ctx, opts)` | Poll until ready (execd `/ping` by default; custom `ReadyOptions.HealthCheck` replaces it) |
| `IsHealthy(ctx)` | One-shot health check |
| `Ping(ctx)` | Ping execd |
| `GetEndpoint(ctx, port)` | Get a public endpoint for a sandbox port |
| `GetSignedEndpoint(ctx, port, expires)` | Get a signed endpoint URL with OSEP-0011 route token |
| `Pause(ctx)` | Pause this sandbox |
| `Resume(ctx)` | Resume and return a newly connected handle (not in-place) |
| `Renew(ctx, duration)` | Extend expiration |
| `PatchMetadata(ctx, patch)` | Patch metadata |
| `SetEnv(ctx, key, value)` | Set an environment variable inside the sandbox |
| `Kill(ctx)` | Force-terminate |
| `Close()` | No-op; does not terminate the sandbox |
| `CreateSnapshot(ctx, req)` | Create a snapshot of this sandbox |

File operations, command execution, code execution, and isolated sessions are
also reachable as `Sandbox` helpers — see the file and isolated-session
examples above, and the `ExecdClient` tables for the full list.

### CodeInterpreter

`CreateCodeInterpreter(ctx, config, opts)` creates a sandbox from the
code-interpreter image and returns a `*CodeInterpreter` that embeds
`*Sandbox` (all sandbox helpers above work on it):

| Method | Description |
|--------|-------------|
| `Execute(ctx, language, code, handlers)` | Execute code with SSE streaming |
| `ExecuteInContext(ctx, contextID, language, code, handlers)` | Execute code in an existing context |
| `IsHealthy(ctx)` | Report interpreter health |

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
