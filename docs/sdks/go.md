---
title: Go SDK
description: Go client library for the OpenSandbox API covering lifecycle, execd, and egress operations.
---

# OpenSandbox Go SDK

Go client library for the [OpenSandbox](https://github.com/opensandbox-group/OpenSandbox/) API.

Provides high-level sandbox helpers and low-level clients for these API areas:
- **Lifecycle** -- Create, manage, and destroy sandbox instances
- **Execd** -- Execute commands, manage files, monitor metrics inside sandboxes
- **Egress** -- Inspect and mutate sandbox network policy at runtime

## Installation

```bash
# go 1.20+
go get github.com/alibaba/OpenSandbox/sdks/sandbox/go
```

## Quick Start

### Create and manage a sandbox

Use the high-level helper to create a sandbox, resolve its endpoint, and wait for
readiness before running commands:

```go
package main

import (
    "context"
    "fmt"
    "log"

    opensandbox "github.com/alibaba/OpenSandbox/sdks/sandbox/go"
)

func run() error {
    ctx := context.Background()
    ttl := 600
    config := opensandbox.ConnectionConfig{
        Domain: "localhost:8080",
        Protocol: "http",
        UseServerProxy: true,
    }
    sandbox, err := opensandbox.CreateSandbox(ctx, config, opensandbox.SandboxCreateOptions{
        Image: "python:3.12",
        TimeoutSeconds: &ttl,
    })
    if err != nil {
        return err
    }
    defer sandbox.Close()
    defer sandbox.Kill(context.Background())

    result, err := sandbox.RunCommand(ctx, "echo sandbox-ready", nil)
    if err != nil {
        return err
    }
    fmt.Println(result.Text())
    return nil
}

func main() {
    if err := run(); err != nil {
        log.Fatal(err)
    }
}
```

`ConnectSandbox` and `ResumeSandbox` check health only when you pass the optional
`ReadyOptions` argument (an empty `ReadyOptions{}` uses the default check).
Omitting it resolves the endpoint without a health check. Pause is asynchronous:
wait until sandbox info reports `Paused` before resuming.

The following snippets go inside a function returning `error`, with the live
`sandbox`, `config`, and `ctx` above. Add standard-library imports used by each
snippet. Creation examples are alternatives; terminate sandboxes with `Kill`
when done. `Close` currently does not terminate the sandbox.

### Run a command with streaming output

Use structured callbacks for normal command output:

```go
result, err := sandbox.RunCommand(ctx, "echo 'Hello from sandbox!'", &opensandbox.ExecutionHandlers{
    OnStdout: func(message opensandbox.OutputMessage) error {
        fmt.Println(message.Text)
        return nil
    },
})
if err != nil {
    return err
}
if result.ExitCode == nil || *result.ExitCode != 0 {
    return fmt.Errorf("command failed: %v", result.Error)
}
```

Use the low-level client when you need raw SSE events or command status/log APIs.
`event.Data` contains JSON; import `encoding/json` and `os` for this example:

```go
endpoint, err := sandbox.GetEndpoint(ctx, 44772)
if err != nil {
    return err
}
exec := opensandbox.NewExecdClient(
    config.Protocol + "://" + endpoint.Endpoint, "",
    opensandbox.WithHeaders(endpoint.Headers),
)

err = exec.RunCommand(ctx, opensandbox.RunCommandRequest{
    Command: "echo 'Hello from sandbox!'",
    Timeout: 30000,
}, func(event opensandbox.StreamEvent) error {
    var message struct{ Type, Text string }
    if err := json.Unmarshal([]byte(event.Data), &message); err != nil {
        return err
    }
    switch message.Type {
    case "stdout":
        fmt.Println(message.Text)
    case "stderr":
        fmt.Fprintln(os.Stderr, message.Text)
    case "execution_complete":
        fmt.Println("[done]")
    }
    return nil
})
if err != nil {
    return err
}
```

`RunCommandRequest.Timeout` is in **milliseconds**. Use this low-level client for
`InterruptCommand`, `GetCommandStatus`, and `GetCommandLogs`, which are not exposed
as high-level `Sandbox` methods.

For native execution, replace the request above with the following. On Linux,
it prints literal `$HOME` and keeps `hello world` as one argument:

```go
_, err = sandbox.RunCommandWithOpts(ctx, opensandbox.RunCommandRequest{
    Argv:    []string{"printf", "%s\n", "$HOME", "hello world"},
    Timeout: 30000,
}, nil)
if err != nil {
    return err
}
```

Native argv execution requires an updated execd. See [command execution modes](/architecture/data-plane/execd#command-execution) for executable lookup and platform behavior.

### Background commands

Reuse the `exec` client from the streaming example to poll status and incremental
logs. Command timeout is in milliseconds and is separate from sandbox TTL.

```go
execution, err := sandbox.RunCommandWithOpts(ctx, opensandbox.RunCommandRequest{
    Command: "for i in 1 2 3; do echo step-$i; sleep 1; done",
    Background: true,
    Timeout: 30_000,
}, nil)
if err != nil {
    return err
}
if execution.ID == "" {
    return fmt.Errorf("no command ID returned")
}
cursor := int64(0)
deadline := time.Now().Add(45 * time.Second)
for {
    if time.Now().After(deadline) {
        if err := exec.InterruptCommand(ctx, execution.ID); err != nil {
            return err
        }
        return fmt.Errorf("command did not finish")
    }
    status, err := exec.GetCommandStatus(ctx, execution.ID)
    if err != nil {
        return err
    }
    logs, err := exec.GetCommandLogs(ctx, execution.ID, &cursor)
    if err != nil {
        return err
    }
    fmt.Print(logs.Output)
    cursor = logs.Cursor
    if !status.Running {
        if status.ExitCode == nil || *status.ExitCode != 0 {
            return fmt.Errorf("command failed: %s", status.Error)
        }
        break
    }
    select {
    case <-ctx.Done():
        return ctx.Err()
    case <-time.After(500 * time.Millisecond):
    }
}
```

### Persistent shell sessions

A Bash session preserves shell variables and the working directory across commands.

```go
session, err := sandbox.CreateSession(ctx)
if err != nil {
    return err
}
defer sandbox.DeleteSession(context.Background(), session.ID)
_, err = sandbox.RunInSession(ctx, session.ID, opensandbox.RunInSessionRequest{
    Command: "cd /tmp; export DEMO=hello",
    Timeout: 30_000,
}, nil)
if err != nil {
    return err
}
result, err := sandbox.RunInSession(ctx, session.ID, opensandbox.RunInSessionRequest{
    Command: "echo \"$DEMO\"; pwd",
    Timeout: 30_000,
}, nil)
if err != nil {
    return err
}
fmt.Println(result.Text())
```

### Persistent environment variables

Set environment variables that the runtime injects into every subsequent command
and session — without hand-writing shell escaping against the sandbox env file.

```go
err := sandbox.SetEnv(ctx, "MY_TOKEN", "it's a safe value")
```

Keys must match `[A-Za-z_][A-Za-z0-9_]*`. Values without a single quote are
stored verbatim; values containing a single quote use the env file's
double-quoted form, in which shell-style `$NAME` sequences may be expanded
when the runtime loads the file. The env file is append-only: the
last write for a key wins. Returns an error if the sandbox fails to persist the
variable.

For filesystem/process isolation within a sandbox, see
[Isolation Sessions](/guides/isolation-sessions). These are separate from Bash sessions.

### File operations

Upload from an `io.Reader` and close download streams after use. This example uses
`strings`, `io`, and `os` from the standard library:

```go
err := sandbox.UploadFile(ctx, strings.NewReader("Hello Sandbox!"), opensandbox.UploadFileOptions{
    Metadata: opensandbox.FileMetadata{Path: "/tmp/demo.txt", Mode: 644},
})
if err != nil {
    return err
}
body, err := sandbox.DownloadFile(ctx, "/tmp/demo.txt", "")
if err != nil {
    return err
}
_, copyErr := io.Copy(os.Stdout, body)
closeErr := body.Close()
if copyErr != nil {
    return copyErr
}
if closeErr != nil {
    return closeErr
}
entries, err := sandbox.ListDirectory(ctx, "/tmp")
if err != nil {
    return err
}
for _, entry := range entries {
    fmt.Println(entry.Path)
}
if err := sandbox.DeleteFiles(ctx, []string{"/tmp/demo.txt"}); err != nil {
    return err
}
```

The same upload/download methods support binary files. Pass a Range header such
as `"bytes=0-1023"` to `DownloadFile` for a partial download.

### Pause and reconnect

Pause is asynchronous and runtime-dependent. Use a deadline before resuming:

```go
pauseCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
defer cancel()
if err := sandbox.Pause(pauseCtx); err != nil {
    return err
}
for {
    info, err := sandbox.GetInfo(pauseCtx)
    if err != nil {
        return err
    }
    if info.Status.State == "Paused" {
        break
    }
    if info.Status.State == "Failed" {
        return fmt.Errorf("pause failed: %s", info.Status.Message)
    }
    select {
    case <-pauseCtx.Done():
        return pauseCtx.Err()
    case <-time.After(time.Second):
    }
}
resumed, err := opensandbox.ResumeSandbox(ctx, config, sandbox.ID(), opensandbox.ReadyOptions{})
if err != nil {
    return err
}
defer resumed.Close()
if _, err := resumed.Renew(ctx, 30*time.Minute); err != nil {
    return err
}
```

Use `ConnectSandbox(ctx, config, id, opensandbox.ReadyOptions{})` to attach to an
already running sandbox with a readiness check. See [Pause and Resume](/guides/pause-resume).

### Resource metrics

Use `metrics, err := sandbox.GetMetrics(ctx)` to read current sandbox resource
usage. This is separate from [SDK creation telemetry](/sdks/observability#creation-metrics).

### Check egress policy

Runtime egress reads and patches go directly to the sandbox egress sidecar.
The SDK first resolves the sandbox endpoint on port `18080`, then calls the
sidecar `/policy` API.

```go
policy, err := sandbox.GetEgressPolicy(ctx)
if err != nil {
    return err
}
fmt.Printf("Mode: %s, Default: %s\n", policy.Mode, policy.Policy.DefaultAction)

_, err = sandbox.PatchEgressRules(ctx, []opensandbox.NetworkRule{
    {Action: "allow", Target: "api.example.com"},
})
if err != nil {
    return err
}
```

Template-backed sandboxes have no sandbox-side egress sidecar: the SDK detects
them via the server's `OPEN-SANDBOX-ORIGIN` response header (see
[Fsb Template Management](#fsb-template-management)) and routes the same
`GetEgressPolicy` / `PatchEgressRules` / `DeleteEgressRules` calls through the
lifecycle control plane (`/sandboxes/{sandboxId}/networkpolicy`) instead.

Patch uses merge semantics:
- Incoming rules take priority over existing rules with the same `target`.
- Existing rules for other targets remain unchanged.

### Use Credential Vault

Credential Vault injects outbound credentials from the egress sidecar while
keeping real secrets out of sandbox environment variables, commands, files, and
logs. Create the sandbox with `CredentialProxy` enabled, then write credentials
and bindings through the sandbox helpers or `EgressClient`.

```go
config := opensandbox.ConnectionConfig{
    Domain:   "localhost:8080",
    Protocol: "http",
    APIKey:   "your-api-key",
}

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
                Ports:   []int{443},
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

See [Credential Vault](/guides/credential-vault) for auth types, binding
guidance, and Git/curl examples.

::: warning
Credential Vault is unavailable for template-backed sandboxes: they have no
sandbox-side egress sidecar. `sandbox.CredentialVault(ctx)` returns an error
for them.
:::

### Client Pool

Use `NewSandboxPoolBuilder` with an in-memory store or the Redis adapter in
`github.com/alibaba/OpenSandbox/sdks/sandbox/go/poolredis`. Go supports all four
acquire policies, namespace retirement, and per-acquire TTL/health-check overrides.

Go uses `ReconcileInterval` and `WarmupConcurrency`; it does not expose the
Python/JVM/JavaScript create-QPS, initial-delay, or post-prepare-check settings.
See [Client Pool](/guides/client-pool) for defaults, examples, and cleanup.
Go does not currently emit built-in pool warmup traces or expose stable remote
diagnostics; use [CLI or HTTP diagnostics](/api/#diagnostics).

## Lifecycle Hooks

Set `Lifecycle` in `SandboxCreateOptions`. `PreStart` completes before the entrypoint starts, while `Periodic` hooks run on their schedules after startup.

```go
hookTimeout := 120
sandbox, err := opensandbox.CreateSandbox(ctx, config, opensandbox.SandboxCreateOptions{
    Image: "ubuntu:24.04",
    Lifecycle: &opensandbox.SandboxLifecycle{
        PreStart: &opensandbox.LifecycleHook{
            Command:        []string{"sh", "-c", "echo ready > /tmp/prestart.done"},
            TimeoutSeconds: &hookTimeout,
        },
        Periodic: []opensandbox.PeriodicLifecycleHook{
            {
                Name:           "checkpoint",
                Schedule:       "@every 5m",
                Command:        []string{"sh", "-c", "date -u >> /tmp/checkpoints.log"},
                TimeoutSeconds: &hookTimeout,
            },
        },
    },
})
if err != nil {
    return err
}
defer sandbox.Close()
defer sandbox.Kill(context.Background())
fmt.Println(sandbox.ID())
```

The Server validates `TimeoutSeconds`; `PreStart` accepts 1–10800 seconds, while `Periodic` accepts 1–300 seconds. Both default to 60 seconds when omitted. See [Lifecycle Hooks](/guides/lifecycle-hooks) for timing, failure behavior, and provider limitations.

## Fsb Template Management

fsb (fast-sandbox microVM) golden-image templates are managed through
`SandboxManager`. Template builds are asynchronous: `CreateTemplate` returns
with `Status.Phase` set to `Pending`; poll `GetTemplate` until the phase
reaches `Succeeded` or `Failed`. Only a `Succeeded` template can create
sandboxes. Template management requires a configured Fsb provider on Kubernetes.
Replace the publish URI with a location configured for your server.

```go
manager := opensandbox.NewSandboxManager(config)

buildCtx, cancel := context.WithTimeout(ctx, 15*time.Minute)
defer cancel()

// Start the async build (starts at TemplatePhasePending)
template, err := manager.CreateTemplate(buildCtx, opensandbox.CreateTemplateRequest{
    Image:          "alpine:3.19",
    Publish:        "s3://bucket/publish",
    ResourceLimits: opensandbox.ResourceLimits{"cpu": "1", "memory": "512Mi", "disk": "2Gi"},
    Readiness:      &opensandbox.TemplateReadiness{Probe: "tcp://127.0.0.1:44772"},
    Metadata:       map[string]string{"team": "backend"},
})
if err != nil {
    return err
}

// Poll until the build finishes
for template.Status.Phase != opensandbox.TemplatePhaseSucceeded &&
    template.Status.Phase != opensandbox.TemplatePhaseFailed {
    select {
    case <-buildCtx.Done():
        return buildCtx.Err()
    case <-time.After(2 * time.Second):
    }
    template, err = manager.GetTemplate(buildCtx, template.TemplateID)
    if err != nil {
        return err
    }
}
if template.Status.Phase == opensandbox.TemplatePhaseFailed {
    return fmt.Errorf("template build failed: %s", template.TemplateID)
}

// List with metadata filters (1-indexed paging)
listed, err := manager.ListTemplates(ctx, opensandbox.ListTemplatesOptions{
    Metadata: map[string]string{"team": "backend"},
    Page:     1,
    PageSize: 20,
})
if err != nil {
    return err
}
fmt.Println(listed)

// When no longer needed: manager.DeleteTemplate(ctx, template.TemplateID)
```

### Creating a Sandbox from a Template

Use `CreateSandboxFromTemplate` to create a sandbox from a `Succeeded`
template. Template mode fixes the workload shape on the server: only
`Metadata`, `NetworkPolicy` and `Extensions` may accompany the template ID,
and `TimeoutSeconds` is required.

```go
sandbox, err := opensandbox.CreateSandboxFromTemplate(ctx, config, "tpl-abc",
    opensandbox.SandboxFromTemplateOptions{
        TimeoutSeconds: 600,
        NetworkPolicy: &opensandbox.NetworkPolicy{
            DefaultAction: "deny",
            Egress: []opensandbox.NetworkRule{
                {Action: "allow", Target: "api.example.com"},
            },
        },
    })
if err != nil {
    return err
}

fmt.Println(sandbox.Origin()) // "template"
```

## Snapshots and metadata

`Sandbox.CreateSnapshot` and `SandboxManager.CreateSnapshot` start snapshot
creation. Use manager `GetSnapshot`, `ListSnapshots`, and `DeleteSnapshot` to
manage snapshots, then pass `SnapshotID` to `SandboxCreateOptions` to restore.
Poll snapshot status before restore. Use `Sandbox.PatchMetadata` or
`SandboxManager.PatchSandboxMetadata` to add/replace keys; nil values remove keys.

Snapshot support depends on the runtime and server configuration. Renew the source
sandbox first if its remaining TTL may expire during snapshot creation. Wait for
`Ready` before restoring. This example retains the snapshot for reuse:

```go
manager := opensandbox.NewSandboxManager(config)
snapshot, err := sandbox.CreateSnapshot(ctx, opensandbox.CreateSnapshotRequest{Name: "demo"})
if err != nil {
    return err
}
fmt.Println("Snapshot:", snapshot.ID)
snapshotCtx, cancel := context.WithTimeout(ctx, 15*time.Minute)
defer cancel()
for {
    snapshot, err = manager.GetSnapshot(snapshotCtx, snapshot.ID)
    if err != nil {
        return err
    }
    if snapshot.Status.State == opensandbox.SnapshotStateReady {
        break
    }
    if snapshot.Status.State == opensandbox.SnapshotStateFailed {
        return fmt.Errorf("snapshot failed: %s", snapshot.Status.Message)
    }
    select {
    case <-snapshotCtx.Done():
        return snapshotCtx.Err()
    case <-time.After(2 * time.Second):
    }
}
restored, err := opensandbox.CreateSandbox(ctx, config, opensandbox.SandboxCreateOptions{
    SnapshotID: snapshot.ID,
})
if err != nil {
    return err
}
defer restored.Close()
defer restored.Kill(context.Background())
fmt.Println(restored.ID())
// When no longer needed: manager.DeleteSnapshot(ctx, snapshot.ID)
```

Add/replace a metadata key and remove another:

```go
project := "demo"
if _, err := sandbox.PatchMetadata(ctx, opensandbox.MetadataPatch{
    "project": &project, "obsolete-key": nil,
}); err != nil {
    return err
}
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
| `GetSignedEndpoint(ctx, sandboxID, port, expires)` | Get signed endpoint URL with a signed route token |
| `CreateTemplate(ctx, req)` | Declare a fsb template (async golden-image build) |
| `GetTemplate(ctx, templateID)` | Get a template with its latest build status |
| `ListTemplates(ctx, opts)` | List templates with metadata filtering and pagination |
| `DeleteTemplate(ctx, templateID)` | Delete a template |
| `GetNetworkPolicy(ctx, sandboxID)` | Get a sandbox's egress policy from the control plane |
| `PatchNetworkPolicy(ctx, sandboxID, rules)` | Merge egress rules into a sandbox's policy |
| `DeleteNetworkPolicyRules(ctx, sandboxID, targets)` | Remove a sandbox's egress rules by target |

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
| `ListDirectory(ctx, path)` | List immediate directory contents (server-side default depth) |
| `ListDirectoryWithDepth(ctx, path, depth)` | List directory contents up to the given depth (`0` returns empty) |
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
| `CreateCredentialVault(ctx, req)` | Create sandbox-local Credential Vault state |
| `GetCredentialVault(ctx)` | Get sanitized Credential Vault state |
| `PatchCredentialVault(ctx, req)` | Atomically mutate credentials and bindings |
| `DeleteCredentialVault(ctx)` | Delete sandbox-local Credential Vault state |
| `ListCredentialVaultCredentials(ctx)` | List sanitized credential metadata |
| `GetCredentialVaultCredential(ctx, name)` | Get sanitized metadata for one credential |
| `ListCredentialVaultBindings(ctx)` | List sanitized binding metadata |
| `GetCredentialVaultBinding(ctx, name)` | Get sanitized metadata for one binding |

## SSE Streaming

Methods that stream output (`RunCommand`, `ExecuteCode`, `RunInSession`, `WatchMetrics`) accept an `EventHandler` callback:

```go
type EventHandler func(event opensandbox.StreamEvent) error
```

Each `StreamEvent` contains:
- `Event` -- the event type (e.g. `"stdout"`, `"stderr"`, `"result"`, `"execution_complete"`). For NDJSON streams, this is extracted from the JSON `type` field automatically.
- `Data` -- the raw event payload (JSON string for NDJSON streams).
- `ID` -- optional event identifier

Return a non-nil error from the handler to stop processing the stream early.

## Client Options

All client constructors accept optional `Option` functions. Custom HTTP clients and health checks must honor context cancellation for timeouts to take effect:

```go
// Reuse the resolved endpoint from the streaming example.
execClient := opensandbox.NewExecdClient(
    config.Protocol + "://" + endpoint.Endpoint, "",
    opensandbox.WithHeaders(endpoint.Headers),
    opensandbox.WithHTTPClient(&http.Client{Timeout: 60 * time.Second}),
)
if err := execClient.Ping(ctx); err != nil {
    return err
}
```

::: info TLS Certificate Strength
SDK-created HTTP clients enforce NIST 2030 minimum TLS certificate strength by default (RSA >= 2048, EC >= 224, DSA P >= 2048/Q >= 224, hash >= 224). If you must interoperate with legacy endpoints, set `AllowWeakServerCertKeyLengths: true` in `TransportConfig`.
:::

::: tip SDK Telemetry
`CreateSandbox` reports create latency to `POST /v1/metrics/events` by default. Set `ConnectionConfig.DisableMetrics` or `OPENSANDBOX_DISABLE_METRICS=1` to opt out. See [SDK Telemetry](/sdks/observability#creation-metrics).
:::

## Error Handling

Non-2xx responses are returned as `*opensandbox.APIError`:

```go
_, err := sandbox.GetInfo(ctx)
if err != nil {
    var apiErr *opensandbox.APIError
    if errors.As(err, &apiErr) {
        fmt.Printf("HTTP %d: %s — %s\n", apiErr.StatusCode, apiErr.Response.Code, apiErr.Response.Message)
    }
    return err
}
```

## License

Apache 2.0
