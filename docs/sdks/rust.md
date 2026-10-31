---
title: Rust SDK
description: Rust client library for the OpenSandbox API covering lifecycle, execd, and egress operations.
---

# OpenSandbox Rust SDK

Rust client library for the [OpenSandbox](https://github.com/opensandbox-group/OpenSandbox) API.

Provides a high-level `Sandbox` facade and low-level clients for three API areas:

- **Lifecycle** — create, inspect, pause, resume, renew, and destroy sandboxes, resolve endpoints, manage snapshots and network policy
- **Execd** — run commands and code, manage bash sessions and files, monitor metrics inside a sandbox
- **Egress** — inspect and mutate the sandbox network policy and its sandbox-local Credential Vault

Highlights:

- A high-level `Sandbox` handle resolves the sandbox endpoint and carries its access token for you; low-level clients cover every API area directly
- Line-by-line streaming for commands, code, and metrics, with a decoder that accepts both execd's NDJSON and standard SSE frames
- Typed errors with the server `code`/`message`, `X-Request-Id`, `Retry-After`, and the OSEP-0024 wake marker
- A retry policy aligned with the other OpenSandbox SDKs: status-code retries only for idempotent methods, plus wake-contract replays for any method
- No hidden global state; every client is built from an explicit `ConnectionConfig`
- `Send + Sync + Clone` handles, so a `Sandbox` or client can be moved into a spawned task or cloned per task

## Installation

The crate lives in the monorepo and is not published to crates.io yet. Depend on the local path inside a checkout:

```toml
[dependencies]
opensandbox = { path = "sdks/sandbox/rust" }
tokio = { version = "1", features = ["macros", "rt-multi-thread"] }
futures-util = "0.3" # only needed to call `.next()` on execution and metrics streams
```

Or depend on the repository directly:

```toml
[dependencies]
opensandbox = { git = "https://github.com/opensandbox-group/OpenSandbox", branch = "main" }
```

The MSRV is Rust 1.75. Only an async runtime is required at compile time; the SDK builds its own `reqwest` clients and does not need a global runtime handle.

## Quick Start

Start an OpenSandbox server first. The default connection target is `http://localhost:8080`.

```rust
use opensandbox::{ConnectionConfig, CreateOptions, Sandbox};

#[tokio::main]
async fn main() -> opensandbox::Result<()> {
    // The API key can also come from OPEN_SANDBOX_API_KEY.
    let config = ConnectionConfig::new()
        .with_domain("localhost:8080")
        .with_api_key("your-api-key");

    // Creation waits for the sandbox to report Running and for execd to
    // answer /ping, and cleans the sandbox up if either step fails.
    let sandbox = Sandbox::create(
        config,
        CreateOptions::from_image("python:3.12").with_timeout_seconds(600),
    )
    .await?;

    let execution = sandbox.exec("python -c 'print(1 + 1)'").await?;
    print!("{}", execution.text());
    println!("exit code: {:?}", execution.exit_code);

    sandbox.kill().await?;
    Ok(())
}
```

A runnable variant lives in [`sdks/sandbox/rust/examples/quickstart.rs`](https://github.com/opensandbox-group/OpenSandbox/blob/main/sdks/sandbox/rust/examples/quickstart.rs).

## Low-level clients

`Sandbox` is a convenience layer. Every API area is also reachable through a dedicated client, which is useful when you manage sandboxes yourself or need a client outside a sandbox handle. All handles are `Send + Sync + Clone` and share a connection pool, so clone them instead of rebuilding clients per task.

### Lifecycle client

```rust
use opensandbox::{ConnectionConfig, LifecycleClient};

let config = ConnectionConfig::new()
    .with_domain("localhost:8080")
    .with_api_key("your-api-key");

let lifecycle = LifecycleClient::new(&config)?;
let info = lifecycle.get_sandbox("sbx-123").await?;
println!("state: {:?}", info.status.state);
```

### Execd client

execd runs inside the sandbox on port `44772`. Resolve its endpoint and access token through the lifecycle API first; in server-proxy mode the endpoint is the lifecycle server itself and the tenant API key replaces the execd token.

```rust
use opensandbox::{ExecdClient, RunCommandRequest, DEFAULT_EXECD_PORT};

let endpoint = lifecycle
    .endpoint("sbx-123", DEFAULT_EXECD_PORT, Some(false))
    .await?;
let base = config.endpoint_url(&endpoint.endpoint)?;

let execd = ExecdClient::new(&base, &endpoint.headers, &config)?;
let execution = execd
    .run_command_collect(&RunCommandRequest::shell("uname -a"))
    .await?;
print!("{}", execution.text());
```

### Egress client

The egress sidecar listens on port `18080` and authenticates with the `OPENSANDBOX-EGRESS-AUTH` header returned in the endpoint headers.

```rust
use opensandbox::{EgressClient, DEFAULT_EGRESS_PORT};

let endpoint = lifecycle
    .endpoint("sbx-123", DEFAULT_EGRESS_PORT, Some(false))
    .await?;
let base = config.endpoint_url(&endpoint.endpoint)?;
let egress = EgressClient::new(&base, None, &endpoint.headers, &config)?;

let policy = egress.policy().await?;
println!("mode: {:?}", policy.mode);
```

On a `Sandbox` handle, use `sandbox.lifecycle()`, `sandbox.execd()`, and `sandbox.egress()`; they reuse the handle's configuration and token.

## Retry behavior

Idempotent methods (`GET`, `HEAD`, `PUT`, `DELETE`, `OPTIONS`) retry `429`, `502`, `503`, and `504` by default with exponential backoff and ±25% jitter. `POST` and `PATCH` are not retried on status codes by default, because a replay can duplicate a sandbox, session, command, or file mutation; opt in with `RetryConfig::retryable_status_codes_non_idempotent` only when the operation is idempotent at the application layer.

A `503` carrying `OpenSandbox-Wake: expired` is retried for every method, including `POST` and stream bootstraps. The OSEP-0024 wake contract guarantees the gateway parked the request before forwarding it, so the replay cannot execute twice. Stream requests never use the generic retry loop; only the wake retry covers them.

`Retry-After` from the server takes precedence over the computed backoff and is capped at 60 seconds. Use `RetryConfig::disabled()` to turn every retry off.

```rust
use opensandbox::{ConnectionConfig, RetryConfig};

let config = ConnectionConfig::new().with_retry(RetryConfig {
    max_retries: 5,
    ..RetryConfig::default()
});
```

## Lifecycle management

```rust
use std::time::Duration;

// Move the absolute expiration to now + 30 minutes.
sandbox.renew(Duration::from_secs(30 * 60)).await?;

// Pause keeps the sandbox state on disk.
sandbox.pause().await?;

// After a pause the cached endpoint is stale: resume_sandbox() resumes the
// sandbox and returns a handle bound to the new endpoint.
let sandbox = sandbox.resume_sandbox().await?;

let info = sandbox.info().await?;
println!("state: {:?}", info.status.state);
println!("expires at: {:?}", info.expires_at); // None without a TTL
```

Reattach to a sandbox created by another process:

```rust
let sandbox = Sandbox::connect(config.clone(), "sbx-123").await?;
```

Create a sandbox without a TTL, so it lives until it is deleted:

```rust
let sandbox = Sandbox::create(
    config.clone(),
    CreateOptions::from_image("ubuntu").with_manual_cleanup(),
)
.await?;
```

List and filter sandboxes, and manage metadata:

```rust
use std::collections::BTreeMap;
use opensandbox::{ListSandboxesQuery, SandboxState};

let page = lifecycle
    .list_sandboxes(&ListSandboxesQuery {
        states: vec![SandboxState::Running],
        page_size: Some(10),
        ..Default::default()
    })
    .await?;
println!("running: {}", page.pagination.total_items);

// Some(value) sets a key, None deletes it.
let patch = BTreeMap::from([("team".to_string(), Some("runtime".to_string()))]);
sandbox.patch_metadata(&patch).await?;
```

## Readiness and health

`Sandbox::create` and `Sandbox::connect` wait for execd to answer `/ping`. Tune the budget and interval at connect time:

```rust
use std::time::Duration;
use opensandbox::{ReadyOptions, Sandbox};

let sandbox = Sandbox::connect_with(
    config.clone(),
    "sbx-123",
    ReadyOptions {
        timeout: Duration::from_secs(60),
        polling_interval: Duration::from_secs(2),
        skip_health_check: false,
    },
)
.await?;

sandbox
    .wait_until_ready(Duration::from_secs(30), Duration::from_millis(200))
    .await?;
println!("healthy: {}", sandbox.is_healthy().await);
sandbox.ping().await?;
```

`Sandbox::connect` retries while the runtime reports `KUBERNETES::POD_IP_NOT_AVAILABLE`; other errors are returned immediately. Set `skip_health_check: true` to resolve the endpoint without probing execd, which helps when execd is deliberately slow to start.

## Commands and streaming

`run_command` returns an `ExecutionStream` that yields typed events as they arrive. `run_command_collect` drains the same stream into an `Execution`.

```rust
use futures_util::StreamExt;
use opensandbox::{ExecutionEvent, RunCommandRequest};

let request = RunCommandRequest::shell("for i in 1 2 3; do echo line-$i; done")
    .with_timeout_millis(30_000)
    .with_cwd("/tmp");

let mut stream = sandbox.execd().run_command(&request).await?;
while let Some(event) = stream.next().await {
    match event? {
        ExecutionEvent::Init(init) => println!("started: {}", init.id),
        ExecutionEvent::Stdout(line) => print!("{}", line.text),
        ExecutionEvent::Stderr(line) => eprint!("{}", line.text),
        ExecutionEvent::Error(error) => {
            eprintln!("failed: {} (exit {:?})", error.value, error.exit_code)
        }
        ExecutionEvent::Complete(complete) => {
            println!("finished in {}ms", complete.execution_time);
            break;
        }
        _ => {}
    }
}
```

Dropping the stream stops reading; there is no need to interrupt a foreground command you no longer care about.

Buffered form, plus the argv form that skips the shell:

```rust
let execution = sandbox.exec("ls -la /").await?;
print!("{}", execution.text());
eprint!("{}", execution.stderr_text());
for result in &execution.results {
    println!("{:?}", result.results); // MIME type -> payload
}

let execution = sandbox
    .execd()
    .run_command_collect(&RunCommandRequest::argv(["python", "-V"]))
    .await?;
```

Background commands end their stream at the startup acknowledgement; poll the real process state afterwards:

```rust
let request = RunCommandRequest::shell("make build").in_background();
let mut stream = sandbox.execd().run_command(&request).await?;

let mut command_id = String::new();
while let Some(event) = stream.next().await {
    if let ExecutionEvent::Init(init) = event? {
        command_id = init.id;
    }
}

let status = sandbox.execd().command_status(&command_id).await?;
if status.running {
    println!("still running: {}", status.content);
}

// Poll logs incrementally; pass the returned cursor back in.
let logs = sandbox.execd().command_logs(&command_id, Some(0)).await?;
print!("{}", logs.output);
let next_cursor = logs.cursor;

sandbox.execd().interrupt_command(&command_id).await?;
```

## Bash sessions

Sessions keep shell state — working directory, variables — across commands.

```rust
use opensandbox::{CreateSessionRequest, RunInSessionRequest};

let session = sandbox
    .execd()
    .create_session(&CreateSessionRequest {
        cwd: Some("/workspace".to_string()),
    })
    .await?;

let request = RunInSessionRequest {
    command: "pwd && echo $PATH".to_string(),
    cwd: None,
    timeout: 0, // milliseconds; zero means no SDK-imposed timeout
};
let _stream = sandbox
    .execd()
    .run_in_session(&session.id, &request)
    .await?;

sandbox.execd().delete_session(&session.id).await?;
```

## Code execution

Code contexts hold interpreter state between calls.

```rust
use opensandbox::RunCodeRequest;

let context = sandbox.execd().create_context("python").await?;

sandbox
    .execd()
    .execute_code_collect(&RunCodeRequest {
        context: Some(context.clone()),
        code: "counter = 41".to_string(),
    })
    .await?;
let second = sandbox
    .execd()
    .execute_code_collect(&RunCodeRequest {
        context: Some(context.clone()),
        code: "print(counter + 1)".to_string(),
    })
    .await?;
print!("{}", second.text());

for result in &second.results {
    for (mime, payload) in &result.results {
        println!("{mime}: {payload}"); // for example text/plain: 42
    }
}

sandbox.execd().delete_context(&context.id).await?;
```

`execute_code` returns the same `ExecutionStream` as commands, and `interrupt_code` stops a running execution. `list_contexts(Some("python"))`, `get_context`, and `delete_contexts_by_language` round out the API.

## Files

```rust
use std::collections::BTreeMap;
use opensandbox::{FileMetadata, MoveItem, Permission, ReplaceItem, UploadEntry};

let execd = sandbox.execd();

// `path` in the metadata is the destination; `mode` uses octal digits
// written as a decimal integer (644 = rw-r--r--).
execd
    .upload_file(UploadEntry::new(
        FileMetadata {
            path: "/workspace/app.py".to_string(),
            owner: None,
            group: None,
            mode: 644,
        },
        b"print(1)\n".to_vec(),
    ))
    .await?;

// Download a whole file, a byte range, or a line range. `range` and
// offset/limit are mutually exclusive.
let full = execd.download_file("/workspace/app.py", None, None, None).await?;
let head = execd
    .download_file("/workspace/app.py", Some("bytes=0-1023"), None, None)
    .await?;
let lines = execd
    .download_file("/workspace/app.py", None, Some(10), Some(50))
    .await?;

// Inspect, list, and search.
let info = execd.file_info(&["/workspace/app.py".to_string()]).await?;
println!("{:?}", info["/workspace/app.py"].size);
let entries = execd.list_directory("/workspace", Some(3)).await?;
let hits = execd.search_files("/workspace", Some("*.json")).await?;

// Directories, moves, permissions, and deletion.
execd.create_directory("/workspace/out", 755).await?;
execd
    .move_files(&[MoveItem {
        src: "/workspace/a.txt".to_string(),
        dest: "/workspace/out/a.txt".to_string(),
    }])
    .await?;
execd
    .set_permissions(&BTreeMap::from([(
        "/workspace/app.py".to_string(),
        Permission {
            owner: None,
            group: None,
            mode: 755,
        },
    )]))
    .await?;
execd.delete_files(&["/workspace/b.txt".to_string()]).await?;
execd.delete_directory("/workspace/out").await?;

// Literal text replacement. `verbose` asks for per-file counts.
let replacements = BTreeMap::from([(
    "/workspace/app.py".to_string(),
    ReplaceItem {
        old: "print(1)".to_string(),
        new: "print(2)".to_string(),
    },
)]);
if let Some(counts) = execd.replace_in_files(&replacements, true).await? {
    println!("replaced: {}", counts["/workspace/app.py"].replaced_count);
}
```

`upload_files` sends several entries in one multipart request.

## Metrics

```rust
use futures_util::StreamExt;

let metrics = sandbox.execd().metrics().await?;
println!(
    "cpu {}% of {} cores, memory {} MiB / {} MiB",
    metrics.cpu_used_pct, metrics.cpu_count, metrics.mem_used_mib, metrics.mem_total_mib
);

// Roughly one sample per second.
let mut samples = sandbox.execd().watch_metrics().await?;
if let Some(sample) = samples.next().await {
    let sample = sample?;
    println!("sample at {}: {}%", sample.timestamp, sample.cpu_used_pct);
}
```

## Snapshots

A snapshot captures a running sandbox; a new sandbox can then be created from it. `image` and snapshot creation are mutually exclusive.

```rust
use std::time::Duration;
use opensandbox::{CreateOptions, Sandbox, SnapshotState};

let snapshot = sandbox.snapshot(Some("pre-migration")).await?;

let mut info = sandbox.lifecycle().get_snapshot(&snapshot.id).await?;
while info.status.state == SnapshotState::Creating {
    tokio::time::sleep(Duration::from_secs(2)).await;
    info = sandbox.lifecycle().get_snapshot(&snapshot.id).await?;
}
if info.status.state != SnapshotState::Ready {
    return Err(opensandbox::Error::invalid_argument(
        "snapshot",
        format!("not ready: {:?} {:?}", info.status.state, info.status.reason),
    ));
}

let restored = Sandbox::create(
    config.clone(),
    CreateOptions::from_snapshot(snapshot.id.clone()),
)
.await?;

// Delete the snapshot only after the restore succeeded.
sandbox.lifecycle().delete_snapshot(&snapshot.id).await?;
```

List snapshots with `ListSnapshotsQuery` (`sandbox_id`, `name`, `states`, `page`, `page_size`); both list responses expose `items` and `pagination`.

## Volumes, network policy, and lifecycle hooks at creation

`Volume` supports exactly one backend per mount: `host`, `pvc`, or `ossfs`.

```rust
use opensandbox::{
    CreateOptions, HostMount, NetworkPolicy, NetworkRule, PvcMount, Volume,
};

let sandbox = Sandbox::create(
    config.clone(),
    CreateOptions::from_image("ubuntu")
        .with_network_policy(NetworkPolicy {
            default_action: Some("deny".to_string()),
            egress: Some(vec![NetworkRule {
                action: "allow".to_string(),
                target: Some("pypi.org".to_string()),
                ports: Some(vec![443]),
            }]),
        })
        .with_volume(Volume {
            name: "workspace".to_string(),
            host: Some(HostMount {
                path: "/srv/workspace".to_string(),
            }),
            pvc: None,
            ossfs: None,
            mount_path: "/workspace".to_string(),
            read_only: false,
            sub_path: None,
        })
        .with_volume(Volume {
            name: "cache".to_string(),
            host: None,
            pvc: Some(PvcMount {
                claim_name: "sandbox-cache".to_string(),
                create_if_not_exists: Some(true),
                delete_on_sandbox_termination: Some(false),
                storage_class: None,
                storage: Some("10Gi".to_string()),
                access_modes: Some(vec!["ReadWriteOnce".to_string()]),
            }),
            ossfs: None,
            mount_path: "/cache".to_string(),
            read_only: false,
            sub_path: None,
        }),
)
.await?;
```

Lifecycle hooks, idle auto-pause, and renew-on-access are set through `with_lifecycle`:

```rust
use opensandbox::{IdlePolicy, LifecycleHook, RenewalPolicy, SandboxLifecycle};

let options = CreateOptions::from_image("python:3.12").with_lifecycle(SandboxLifecycle {
    pre_start: Some(LifecycleHook {
        command: vec!["/bin/sh".into(), "-c".into(), "echo booting".into()],
        timeout_seconds: Some(30),
    }),
    periodic: None,
    idle_policy: Some(IdlePolicy {
        idle_timeout_seconds: Some(600),
        on_idle: Some("pause".to_string()),
        wake_on_access: Some(true),
        active_probe: None,
    }),
    renewal_policy: Some(RenewalPolicy { extend_seconds: 1800 }),
});
```

## Runtime egress policy

For image and snapshot sandboxes, policy reads and patches go directly to the egress sidecar. The SDK resolves the endpoint on port `18080` for you.

```rust
use opensandbox::NetworkRule;

let egress = sandbox.egress().await?;

let policy = egress.policy().await?;
println!("default: {:?}", policy.policy.and_then(|p| p.default_action));

egress
    .patch_policy(&[
        NetworkRule {
            action: "allow".to_string(),
            target: Some("www.github.com".to_string()),
            ports: Some(vec![443]),
        },
        NetworkRule {
            action: "deny".to_string(),
            target: Some("pypi.org".to_string()),
            ports: None,
        },
    ])
    .await?;

egress.delete_policy(&["pypi.org".to_string()]).await?;
```

Patch uses merge semantics: incoming rules win over existing rules for the same `target`, other targets are preserved, the first rule for a target inside one payload wins, and the current `defaultAction` is preserved. `delete_policy` ignores unknown targets and also preserves `defaultAction`.

Template-backed sandboxes have no sandbox-side sidecar; their policy is managed through the lifecycle control plane:

```rust
let policy = sandbox.lifecycle().get_network_policy(sandbox.id()).await?;
sandbox.lifecycle().patch_network_policy(
    sandbox.id(),
    &[NetworkRule {
        action: "allow".to_string(),
        target: Some("api.example.com".to_string()),
        ports: Some(vec![443]),
    }],
)
.await?;
```

`sandbox.is_template_backed()` tells you which path applies.

## Credential Vault

Credential Vault injects outbound credentials from the egress sidecar while keeping real secrets out of sandbox environment variables, commands, files, and logs. Create the sandbox with the credential proxy enabled and a network policy, then write credentials and bindings through the egress client.

```rust
use opensandbox::{CreateOptions, NetworkPolicy, NetworkRule, Sandbox};

let sandbox = Sandbox::create(
    config.clone(),
    CreateOptions::from_image("python:3.11")
        .with_credential_proxy()
        .with_network_policy(NetworkPolicy {
            default_action: Some("deny".to_string()),
            egress: Some(vec![NetworkRule {
                action: "allow".to_string(),
                target: Some("api.example.com".to_string()),
                ports: Some(vec![443]),
            }]),
        }),
)
.await?;

let egress = sandbox.egress().await?;
let state = egress.credential_vault().await?;
println!("revision: {:?}", state);
```

`create_credential_vault` activates the first vault revision, `patch_credential_vault` mutates credentials and bindings atomically, and `delete_credential_vault` removes the sandbox-local vault. `list_credentials`, `credential`, `list_bindings`, and `binding` return sanitized metadata only. See the [`Credential Vault` guide](/guides/credential-vault) for the credential and binding model.

## Template-backed sandboxes

Create from an existing fast-sandbox template ID with `CreateOptions::from_template`; a TTL is required. Template CRUD itself is not part of this SDK, and template-backed sandboxes route egress policy through the lifecycle API.

## Endpoints, proxy mode, and host rewriting

Server-proxy mode routes data-plane traffic through the lifecycle server, which is what you need when execd and the egress sidecar are not directly reachable from the client:

```rust
let config = ConnectionConfig::new()
    .with_domain("localhost:8080")
    .with_server_proxy(true);
```

If the server reports an address that is not reachable from the client, rewrite the host (the port is preserved):

```rust
let config = ConnectionConfig::new().with_endpoint_host_rewrite("127.0.0.1", "sandbox.internal");
```

Resolved endpoints are cached per sandbox, port, and proxy mode for 10 minutes by default, with a 1024-entry cap. `LifecycleClient::invalidate_endpoints` (also called by `pause`, `kill`, and `resume`) drops the entries for one sandbox; `with_endpoint_cache_ttl`, `with_endpoint_cache_size`, and `without_endpoint_cache` tune or disable the cache.

## Configuration

### ConnectionConfig

| Method | Description | Default | Environment variable |
| --- | --- | --- | --- |
| `with_domain` | Lifecycle server address (`host:port` or a URL) | `localhost:8080` | `OPEN_SANDBOX_DOMAIN` |
| `with_protocol` | `http` or `https` | `http` | `OPEN_SANDBOX_PROTOCOL` |
| `with_api_key` | Tenant API key | none | `OPEN_SANDBOX_API_KEY` |
| `with_server_proxy` | Route data-plane traffic through the lifecycle server | `false` | - |
| `with_request_timeout` | Per-request timeout | 30 seconds | - |
| `with_header` | Extra header on every request | empty | - |
| `with_auth_header` | Header that carries the API key | `OPEN-SANDBOX-API-KEY` | - |
| `with_retry` | Retry policy | `RetryConfig::default()` | - |
| `with_endpoint_host_rewrite` | Rewrite a resolved endpoint host | none | - |
| `with_endpoint_cache_ttl` | Endpoint cache lifetime | 600 seconds | - |
| `with_endpoint_cache_size` | Endpoint cache capacity | 1024 entries | - |
| `without_endpoint_cache` | Disable endpoint caching | cache enabled | - |

Environment variables are read when the matching setter was not called, so `ConnectionConfig::new()` alone is enough for a configured CI job.

### RetryConfig

| Field | Default | Description |
| --- | --- | --- |
| `max_retries` | `3` | Generic retries, not counting wake replays |
| `initial_backoff` | 500 ms | First backoff delay |
| `max_backoff` | 30 s | Upper bound for a single delay |
| `multiplier` | `2.0` | Exponential growth factor |
| `jitter` | `0.25` | Randomization as a fraction (±25%) |
| `retryable_status_codes` | `[429, 502, 503, 504]` | Retried for idempotent methods |
| `retryable_status_codes_non_idempotent` | `[]` | Retried for `POST`/`PATCH`; empty by default |
| `wake_retry_enabled` | `true` | Honor `OpenSandbox-Wake: expired` for any method |
| `wake_max_attempts` | `5` | Maximum wake replays |
| `wake_overall_deadline` | 60 s | Overall wake-replay budget |

### CreateOptions

| Method | Description | Default |
| --- | --- | --- |
| `from_image` | Create from an OCI image | - |
| `from_snapshot` | Create from a snapshot | - |
| `from_template` | Create from a fast-sandbox template (TTL required) | - |
| `with_image_auth` | Registry credentials for a private image | none |
| `with_entrypoint` | Override the entrypoint argv | `["tail", "-f", "/dev/null"]` |
| `with_timeout_seconds` | TTL in seconds | 600 |
| `with_manual_cleanup` | No TTL; the sandbox lives until deleted | TTL enabled |
| `with_resource_limit` | Resource limit, for example `cpu=500m` | `cpu=1`, `memory=2Gi` |
| `with_resource_request` | Resource request (Burstable QoS) | none |
| `with_env` | Environment variable | empty |
| `with_metadata` | Metadata key (keys under `opensandbox.io/` are reserved) | empty |
| `with_network_policy` | Egress policy applied at creation | none |
| `with_volume` | Mount a host/PVC/OSS volume | none |
| `with_platform` | Runtime platform constraint (`linux`/`windows`, `amd64`/`arm64`) | none |
| `with_lifecycle` | Pre-start and periodic hooks, idle policy, renewal policy | none |
| `with_credential_proxy` | Enable the Credential Vault proxy at startup | disabled |
| `with_secure_access` | Enable secure access | disabled |
| `with_ready_timeout` | Budget for reaching `Running` | 30 seconds |
| `with_poll_interval` | Interval between state checks | 200 ms |
| `skip_health_check` | Do not probe execd after creation | probe enabled |

`image`, `snapshot`, and `template` are mutually exclusive; `CreateOptions::to_request` validates the combination locally and returns `Error::InvalidArgument` before any network call.

## API reference

### Sandbox

Created with `Sandbox::create(config, options)`, `Sandbox::connect(config, id)`, `Sandbox::connect_with(config, id, ready_options)`, or `Sandbox::resume(config, id)`.

| Method | Description |
| --- | --- |
| `id()` | Sandbox identifier |
| `origin()` | Origin reported by the server (`Template` or `Unknown`) |
| `is_template_backed()` | Whether egress policy goes through the lifecycle API |
| `config()` | The shared `ConnectionConfig` |
| `lifecycle()` | Underlying `LifecycleClient` |
| `execd()` | Underlying `ExecdClient` |
| `info()` | Fetch the current `SandboxInfo` |
| `wait_until_ready(timeout, interval)` | Poll execd `/ping` until it answers |
| `is_healthy()` | One-shot health check as a `bool` |
| `ping()` | Ping execd |
| `exec(command)` | Run a shell command and collect its output |
| `run_command(request)` | Run a command and return the raw event stream |
| `endpoint(port)` | Public endpoint for a sandbox port |
| `signed_endpoint(port, expires)` | Signed endpoint (OSEP-0011) |
| `egress()` | Resolve the sidecar and return an `EgressClient` |
| `pause()` | Pause this sandbox |
| `resume_sandbox()` | Resume and return a handle bound to the new endpoint |
| `renew(duration)` | Move expiration to `now + duration` |
| `patch_metadata(patch)` | Merge metadata (`None` deletes a key) |
| `snapshot(name)` | Capture a snapshot of this sandbox |
| `kill()` | Force-terminate and invalidate cached endpoints |

### LifecycleClient

Created with `LifecycleClient::new(&config)`.

| Method | Description |
| --- | --- |
| `create_sandbox(&request)` | Create a sandbox from a raw `CreateSandboxRequest` |
| `get_sandbox(id)` | Get sandbox details |
| `list_sandboxes(&query)` | List with `states`, `metadata`, `page`, `page_size` |
| `delete_sandbox(id)` | Delete a sandbox |
| `pause_sandbox(id)` | Pause a sandbox |
| `resume_sandbox(id)` | Resume a paused sandbox |
| `renew_expiration(id, expires_at)` | Set an absolute expiration |
| `patch_metadata(id, patch)` | Merge or delete metadata keys |
| `endpoint(id, port, use_server_proxy)` | Resolve a data-plane endpoint (cached) |
| `signed_endpoint(id, port, expires)` | Resolve a signed endpoint |
| `invalidate_endpoints(id)` | Drop cached endpoints for one sandbox |
| `get_network_policy(id)` | Read the effective egress policy |
| `patch_network_policy(id, rules)` | Merge rules into the policy |
| `delete_network_policy(id, targets)` | Remove rules by target |
| `create_snapshot(id, &request)` | Capture a snapshot |
| `get_snapshot(snapshot_id)` | Get snapshot details |
| `list_snapshots(&query)` | List snapshots |
| `delete_snapshot(snapshot_id)` | Delete a snapshot |

### ExecdClient

Created with `ExecdClient::new(endpoint_url, &headers, &config)`.

**Health**

| Method | Description |
| --- | --- |
| `ping()` | Check that execd answers |

**Commands**

| Method | Description |
| --- | --- |
| `run_command(&request)` | Run a command with a typed event stream |
| `run_command_collect(&request)` | Run a command and drain the stream into an `Execution` |
| `interrupt_command(id)` | Interrupt a running foreground command |
| `command_status(id)` | Status of a command (running, exit code, output) |
| `command_logs(id, cursor)` | Incremental background logs plus the next cursor |
| `create_session(&request)` | Create a persistent bash session |
| `run_in_session(session_id, &request)` | Run a command inside a session |
| `delete_session(session_id)` | Delete a session |

**Code execution**

| Method | Description |
| --- | --- |
| `create_context(language)` | Create a code execution context |
| `list_contexts(language)` | List contexts, optionally by language |
| `get_context(context_id)` | Get one context |
| `delete_context(context_id)` | Delete a context |
| `delete_contexts_by_language(language)` | Delete every context for a language |
| `execute_code(&request)` | Execute code with a typed event stream |
| `execute_code_collect(&request)` | Execute code and collect the result |
| `interrupt_code(execution_id)` | Interrupt a running execution |

**Files**

| Method | Description |
| --- | --- |
| `file_info(paths)` | Metadata for one or more paths |
| `delete_files(paths)` | Delete files |
| `set_permissions(&permissions)` | Change owner, group, and mode |
| `move_files(&moves)` | Move or rename files |
| `search_files(dir, pattern)` | Glob search under a directory |
| `list_directory(path, depth)` | List a directory up to `depth` levels |
| `create_directory(path, mode)` | Create a directory tree |
| `delete_directory(path)` | Delete a directory recursively |
| `replace_in_files(&replacements, verbose)` | Literal text replacement, optionally with counts |
| `upload_file(entry)` | Upload one file (multipart) |
| `upload_files(&entries)` | Upload several files in one request |
| `download_file(path, range, offset, limit)` | Download a whole file, a byte range, or a line range |

**Metrics**

| Method | Description |
| --- | --- |
| `metrics()` | Point-in-time CPU and memory usage |
| `watch_metrics()` | Server-sent metric samples, roughly one per second |

### EgressClient

Created with `EgressClient::new(endpoint_url, token, &headers, &config)`.

| Method | Description |
| --- | --- |
| `policy()` | Read the enforced policy and sidecar metadata |
| `patch_policy(&rules)` | Merge rules into the policy |
| `delete_policy(&targets)` | Remove rules by target |
| `create_credential_vault(&request)` | Create and activate the initial vault revision |
| `credential_vault()` | Read sanitized vault state |
| `patch_credential_vault(&request)` | Atomically mutate credentials and bindings |
| `delete_credential_vault()` | Delete the sandbox-local vault |
| `list_credentials()` | List sanitized credential metadata |
| `credential(name)` | Sanitized metadata for one credential |
| `list_bindings()` | List sanitized binding metadata |
| `binding(name)` | Sanitized metadata for one binding |

## Streaming events

`run_command`, `run_in_session`, and `execute_code` return an `ExecutionStream`, which implements `futures_util::Stream<Item = opensandbox::Result<ExecutionEvent>>`. `watch_metrics` returns a similar stream of `Metrics` samples. Every stream the SDK returns is `Unpin`, so `stream.next().await` works on a plain `let mut` binding.

| Event | Payload |
| --- | --- |
| `Init` | Execution identifier, used for interrupt and status calls |
| `Stdout` | One stdout line (`text`, `timestamp`) |
| `Stderr` | One stderr line (`text`, `timestamp`) |
| `Result` | MIME type to payload map, emitted by code execution |
| `Error` | Error name, value, traceback, and optional exit code |
| `Complete` | Timestamp, wall-clock duration, and optional exit code |
| `Other` | Event type and raw payload for events this SDK version does not know |

The decoder accepts execd's newline-delimited JSON and standard SSE frames, including multi-line `data:` payloads, comments, and frames split across network reads. Unknown event types that carry a `text` field are surfaced as `Stdout`, matching the other SDKs.

`Execution::exit_code` prefers the error event's exit code, then a parsed error value, then the completion event's exit code, and infers `0` when the execution completed without an error. A background launch ends its stream at the startup acknowledgement and infers `0`, so use `command_status`/`command_logs` for the real process result.

## Error handling

Every fallible call returns `opensandbox::Result<T>`, which is `std::result::Result<T, opensandbox::Error>`:

| Variant | Meaning |
| --- | --- |
| `Error::Api(ApiError)` | Non-2xx response from the server |
| `Error::InvalidArgument` | Locally rejected argument, no request sent |
| `Error::Http` | Transport failure after retries |
| `Error::Url` | Invalid endpoint or base URL |
| `Error::Json` | Unparseable payload |
| `Error::Io` | Local I/O failure |
| `Error::RunningTimeout` | The sandbox did not reach `Running` in time |
| `Error::ReadyTimeout` | execd did not answer `/ping` in time |
| `Error::TerminalState` | The sandbox reached `Terminated` or `Failed` during creation |
| `Error::ExecdUnavailable` | execd could not be resolved or reached |
| `Error::Stream` | The event stream failed |

`ApiError` carries the HTTP `status`, the server `code` and `message`, the optional `request_id`, the parsed `retry_after`, and `wake_expired`:

```rust
use opensandbox::Error;

match sandbox.info().await {
    Ok(info) => println!("state: {:?}", info.status.state),
    Err(Error::Api(api)) if api.status == 404 => {
        // The sandbox is gone. Log the request id for server-side correlation.
        eprintln!("{}: {} (request id: {:?})", api.code, api.message, api.request_id);
    }
    Err(Error::Api(api)) => eprintln!("HTTP {}: {} - {}", api.status, api.code, api.message),
    Err(other) => eprintln!("sdk error: {other}"),
}
```

`Error::invalid_argument(field, message)` builds the local-validation variant, and `Error::api()` returns the `ApiError` when the failure came from the server.

## Scope and limitations

This SDK covers the lifecycle, execd, and egress surfaces listed above. It deliberately does not implement:

- Client Pool (OSEP-0005 / OSEP-0021) and pool warmup telemetry
- Diagnostic context, logs, and events
- Template CRUD — creating a sandbox from an existing template ID is supported
- Isolated sessions (OSEP-0013); use bash sessions or code contexts instead
- The code-interpreter facade; use `execute_code` with a context instead
- Custom readiness probes beyond execd `/ping` and `skip_health_check`
- Redirect hardening: the underlying HTTP client follows `reqwest`'s default policy of up to 10 hops and does not strip protected `OPEN-SANDBOX-*` and `OPENSANDBOX-*` headers on cross-origin redirects the way the Go and Python SDKs do. Point the SDK at a domain that serves the API directly, and keep long-lived secrets in `with_api_key` rather than in custom headers.

Requests are built from the handwritten models in `opensandbox::models`. Response fields that the model does not know are ignored by serde and become available once the model is updated.
