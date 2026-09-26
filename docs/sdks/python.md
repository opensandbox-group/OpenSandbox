---
title: Python SDK
description: Python SDK for creating, managing, and interacting with secure OpenSandbox environments.
---

# OpenSandbox SDK for Python

Create sandboxes, run commands, and manage files with async or synchronous Python APIs.

## Installation

### pip

```bash
pip install opensandbox
```

### uv

```bash
uv add opensandbox
```

## Quick Start

The following example shows how to create a sandbox and execute a shell command.

::: tip
Before running this example, ensure the OpenSandbox service is running. See the [Getting Started](/getting-started/) guide for startup instructions.
:::

```python
import asyncio
from opensandbox.sandbox import Sandbox
from opensandbox.config import ConnectionConfig
from opensandbox.exceptions import SandboxException

async def main():
    # 1. Configure connection
    config = ConnectionConfig(
        domain="api.opensandbox.io",
        api_key="your-api-key"
    )

    # 2. Create a Sandbox
    try:
        sandbox = await Sandbox.create(
            "ubuntu",
            connection_config=config
        )
        try:
            # 3. Execute a shell command
            execution = await sandbox.commands.run("echo 'Hello Sandbox!'")

            # 4. Print output
            print(execution.logs.stdout[0].text)
        finally:
            # 5. Terminate the remote sandbox and close local resources
            await sandbox.destroy()

    except SandboxException as e:
        # Handle Sandbox specific exceptions
        print(f"Sandbox Error: [{e.error.code}] {e.error.message}")
        # Server logs can be correlated by this request id (if available)
        print(f"Request ID: {e.request_id}")
    except Exception as e:
        print(f"Error: {e}")

if __name__ == "__main__":
    asyncio.run(main())
```

### Synchronous Quick Start

If you prefer a synchronous API, use `SandboxSync` / `SandboxManagerSync` and `ConnectionConfigSync`:

```python
from datetime import timedelta

from opensandbox import SandboxSync
from opensandbox.config import ConnectionConfigSync

config = ConnectionConfigSync(
    domain="api.opensandbox.io",
    api_key="your-api-key",
    request_timeout=timedelta(seconds=30),
)

sandbox = SandboxSync.create("ubuntu", connection_config=config)
try:
    execution = sandbox.commands.run("echo 'Hello Sandbox!'")
    print(execution.logs.stdout[0].text)
finally:
    sandbox.destroy()
```

Use `destroy()` for create-use-discard workflows. It calls `kill()` before
`close()` and still closes local resources if remote termination fails. Context
managers continue to call only `close()`, so the remote sandbox remains available
for later `connect()` calls unless you explicitly kill or destroy it.

## Client Pool and observability

Use `SandboxPoolSync` for synchronous applications and `SandboxPoolAsync` for
asyncio. Both provide in-memory and Redis-backed stores, four acquire policies,
and staged warmup controls. See [Client Pool](/guides/client-pool) for examples,
configuration, cleanup, and distributed deployment.

Enable `ConnectionConfig(enable_tracing=True)` or
`ConnectionConfigSync(enable_tracing=True)` for [pool warmup traces](/sdks/observability#pool-warmup-tracing).
For remote logs/events, use the [Diagnostics](/api/#diagnostics) manager API.
Create-latency reporting is controlled separately by [SDK Telemetry](/sdks/observability#creation-metrics).

## Lifecycle Hooks

Pass a `SandboxLifecycle` when creating a sandbox. `pre_start` completes before the entrypoint starts, while `periodic` hooks run on their schedules after startup.

```python
from opensandbox.models.sandboxes import (
    LifecycleHook,
    PeriodicLifecycleHook,
    SandboxLifecycle,
)

sandbox = await Sandbox.create(
    "ubuntu:24.04",
    connection_config=config,
    lifecycle=SandboxLifecycle(
        pre_start=LifecycleHook(
            command=["sh", "-c", "echo ready > /tmp/prestart.done"],
            timeout_seconds=120,
        ),
        periodic=[
            PeriodicLifecycleHook(
                name="checkpoint",
                schedule="@every 5m",
                command=["sh", "-c", "date -u >> /tmp/checkpoints.log"],
                timeout_seconds=120,
            )
        ],
    ),
)
```

The Server validates `timeout_seconds`; `pre_start` accepts 1–10800 seconds, while `periodic` accepts 1–300 seconds. Both default to 60 seconds when omitted. See [Lifecycle Hooks](/guides/lifecycle-hooks) for timing, failure behavior, and provider limitations.

## Usage Examples

The snippets below use `config` and a live `sandbox` from the quick start. Run
async snippets inside an async function, before its cleanup block. Creation
examples are alternatives; call `destroy()` when each sandbox is no longer needed.
For the synchronous API, use `SandboxSync` and `SandboxManagerSync`, and omit
`await` / `async` from SDK calls and context managers.

### 1. Lifecycle Management

Manage the sandbox lifecycle, including renewal, pausing, and resuming.

```python
import time
from datetime import timedelta

# Renew the sandbox
# This resets the expiration time to (current time + duration)
await sandbox.renew(timedelta(minutes=30))

# Request pause (runtime-dependent)
await sandbox.pause()

deadline = time.monotonic() + 120
while True:
    if time.monotonic() >= deadline:
        raise TimeoutError("Sandbox did not pause within 120 seconds")
    info = await sandbox.get_info()
    if info.status.state == "Paused":
        break
    if info.status.state == "Failed":
        raise RuntimeError(info.status.message)
    await asyncio.sleep(1)

# Resume creates a new local handle.
resumed = await Sandbox.resume(sandbox_id=sandbox.id, connection_config=config)
await sandbox.close()
sandbox = resumed

# Get current status
info = await sandbox.get_info()
print(f"State: {info.status.state}")
print(f"Expires: {info.expires_at}")  # None when no automatic expiration is configured
```

Pause is asynchronous and runtime-dependent. See [Pause and Resume](/guides/pause-resume).
Use `await Sandbox.connect(sandbox_id, connection_config=config)` to attach to an
already running sandbox without resuming it.

Create a non-expiring sandbox by explicitly passing `timeout=None`. Omitting
`timeout` uses the default 10-minute TTL:

```python
manual = await Sandbox.create(
    "ubuntu",
    connection_config=config,
    timeout=None,
)
```

### 2. Custom Health Check

Resolving an endpoint confirms that a route exists; it does not confirm that the
application on that port is healthy. For service readiness, make a bounded request
to the application's health endpoint and include the returned endpoint headers.

With the built-in health probe, readiness checks during creation, connection, and
resume fail immediately when the health endpoint returns HTTP 401 or 403.
The SDK raises `SandboxApiException`
with the original status, error details, and request ID instead of waiting for
`SandboxReadyTimeoutException`. Check the endpoint credentials or permissions
before retrying. Transient health failures retain their existing polling behavior;
`is_healthy()` still returns `False` for a failed built-in health probe.

Define custom logic to determine if the sandbox is healthy. This overrides the default ping check. Synchronous checks must set their own timeouts because the SDK cannot interrupt them; asynchronous checks must not block the event loop or suppress cancellation.

```python
import httpx

async def custom_health_check(sbx: Sandbox) -> bool:
    endpoint = await sbx.get_endpoint(80)
    url = f"{sbx.connection_config.protocol}://{endpoint.endpoint}/"
    try:
        async with httpx.AsyncClient(timeout=2) as client:
            response = await client.get(url, headers=endpoint.headers)
        return response.status_code == 200
    except httpx.RequestError:
        return False

sandbox = await Sandbox.create(
    "nginx:latest",
    entrypoint=["nginx", "-g", "daemon off;"],
    connection_config=config,
    health_check=custom_health_check,
)
```

### 3. Command Execution & Streaming

Execute commands and handle output streams in real-time.

```python
from opensandbox.models.execd import ExecutionHandlers, RunCommandOpts

# Define async handlers for streaming output
async def handle_stdout(msg):
    print(f"STDOUT: {msg.text}")

async def handle_stderr(msg):
    print(f"STDERR: {msg.text}")

async def handle_complete(complete):
    print(f"Command finished in {complete.execution_time_in_millis}ms")

# Create handlers (all handlers must be async)
handlers = ExecutionHandlers(
    on_stdout=handle_stdout,
    on_stderr=handle_stderr,
    on_execution_complete=handle_complete
)

# Execute command with handlers
result = await sandbox.commands.run(
    "for i in {1..5}; do echo \"Count $i\"; sleep 0.5; done",
    handlers=handlers
)
```

To execute a native program without shell parsing, pass an argument list. On Linux,
this example prints literal `$HOME` and keeps `hello world` as one argument:

```python
result = await sandbox.commands.run(["printf", "%s\n", "$HOME", "hello world"])
```

Native argv execution requires an updated execd. See [command execution modes](/architecture/data-plane/execd#command-execution) for executable lookup and platform behavior.

#### Background commands

Start a bounded command, read incremental logs, and check its exit status.
Command timeout and sandbox TTL are separate settings.

```python
import time
from datetime import timedelta
from opensandbox.models.execd import RunCommandOpts

execution = await sandbox.commands.run(
    'for i in 1 2 3; do echo "step $i"; sleep 1; done',
    opts=RunCommandOpts(background=True, timeout=timedelta(seconds=30)),
)
if not execution.id:
    raise RuntimeError("No command ID returned")
cursor = 0
deadline = time.monotonic() + 45
while True:
    if time.monotonic() >= deadline:
        await sandbox.commands.interrupt(execution.id)
        raise TimeoutError("Command did not finish")
    status = await sandbox.commands.get_command_status(execution.id)
    logs = await sandbox.commands.get_background_command_logs(execution.id, cursor)
    print(logs.content, end="")
    cursor = logs.cursor if logs.cursor is not None else cursor
    if status.running is False:
        if status.exit_code != 0:
            raise RuntimeError(f"Command failed: {status.exit_code}, {status.error}")
        break
    await asyncio.sleep(0.5)
```

#### Persistent shell sessions

Use a Bash session to preserve shell variables and the working directory across
commands. Delete the session when finished.

```python
session_id = await sandbox.commands.create_session(working_directory="/tmp")
try:
    await sandbox.commands.run_in_session(session_id, "export DEMO=hello")
    result = await sandbox.commands.run_in_session(session_id, 'echo "$DEMO"; pwd')
    print("".join(message.text for message in result.logs.stdout))
finally:
    await sandbox.commands.delete_session(session_id)
```

#### Persistent environment variables

Set environment variables that the runtime injects into every subsequent command
and session — without hand-writing shell escaping against the sandbox env file.

```python
await sandbox.commands.set_env("MY_TOKEN", "it's a safe value")
```

Keys must match `[A-Za-z_][A-Za-z0-9_]*`. Values are stored verbatim with proper
escaping (quotes, backslashes, newlines, `=`). The env file is append-only: the
last write for a key wins. Raises `SandboxException` if the sandbox fails to
persist the variable. The sync API exposes the same method on
`sandbox.commands`.

For commands that need filesystem/process isolation within a sandbox, see
[Isolation Sessions](/guides/isolation-sessions). These are separate from Bash sessions.

### 4. File Operations

Manage files and directories, including read, write, list, delete, and search.

```python
from opensandbox.models.filesystem import DirectoryListEntry, WriteEntry, SearchEntry

# 1. Write file
await sandbox.files.write_files([
    WriteEntry(
        path="/tmp/hello.txt",
        data="Hello World",
        mode=644
    )
])

# 2. Read file
content = await sandbox.files.read_file("/tmp/hello.txt")
print(f"Content: {content}")

# List immediate children; search filters by a filename pattern.
entries = await sandbox.files.list_directory(DirectoryListEntry(path="/tmp", depth=1))
print([entry.path for entry in entries])

# 3. Search files
files = await sandbox.files.search(
    SearchEntry(
        path="/tmp",
        pattern="*.txt"
    )
)
for f in files:
    print(f"Found: {f.path}")

# 4. Delete file
await sandbox.files.delete_files(["/tmp/hello.txt"])
```

For binary data, pass `bytes` to `WriteEntry.data` and use `read_bytes()`;
use `read_bytes_stream()` for large downloads. `read_file()` and `read_bytes()`
also accept `offset` and `limit` for partial reads.

### 5. Sandbox Management (Admin)

Use `SandboxManager` for administrative tasks and finding existing sandboxes.

```python
from opensandbox.manager import SandboxManager
from opensandbox.models.sandboxes import SandboxFilter

# Create manager using async context manager
async with await SandboxManager.create(connection_config=config) as manager:

    # First page only; increase page to retrieve subsequent pages.
    sandboxes = await manager.list_sandbox_infos(
        SandboxFilter(
            states=["Running"],
            page_size=10
        )
    )

    for info in sandboxes.sandbox_infos:
        print(f"Found sandbox: {info.id}")
```

### Resource metrics

Read current sandbox resource usage with `await sandbox.get_metrics()`. This is
separate from [SDK creation telemetry](/sdks/observability#creation-metrics).

## Snapshots, templates, and metadata

| Operation | Public API |
| --- | --- |
| Snapshot a sandbox | `sandbox.create_snapshot(name=...)` or `manager.create_snapshot(sandbox_id, name=...)` |
| Inspect/list/delete snapshots | `manager.get_snapshot`, `list_snapshots`, `delete_snapshot` |
| Restore a snapshot | `Sandbox.create(snapshot_id=..., connection_config=config)` |
| Manage Fsb templates | `manager.create_template`, `get_template`, `list_templates`, `delete_template` |
| Create from a published template | `Sandbox.create_from_template(template_id, timeout=..., connection_config=config)` |
| Patch metadata | `sandbox.patch_metadata` or `manager.patch_sandbox_metadata` |

These APIs also exist on the synchronous SDK. Snapshot creation and template
builds are asynchronous: inspect status before restoring or using a template.
Templates must reach `Succeeded`; template-backed creation requires a TTL and
inherits environment, resources, volumes, and lifecycle hooks from the template.
Metadata patch values add/replace keys; `None` deletes a key.
See the [lifecycle contract](/api/#1-sandbox-lifecycle-yml) for backend constraints.

Snapshot support depends on the runtime and server configuration. Renew the source
sandbox first if its remaining TTL may expire during snapshot creation. This example
waits up to 15 minutes, restores a new sandbox, and retains the snapshot for reuse:

```python
import time
from opensandbox.manager import SandboxManager

async with await SandboxManager.create(connection_config=config) as manager:
    snapshot = await sandbox.create_snapshot(name="demo")
    print("Snapshot:", snapshot.id)
    deadline = time.monotonic() + 900
    while True:
        if time.monotonic() >= deadline:
            raise TimeoutError(f"Snapshot {snapshot.id} is not ready")
        snapshot = await manager.get_snapshot(snapshot.id)
        if snapshot.status.state == "Ready":
            break
        if snapshot.status.state == "Failed":
            raise RuntimeError(snapshot.status.message)
        await asyncio.sleep(2)
    restored = await Sandbox.create(snapshot_id=snapshot.id, connection_config=config)
    try:
        print(restored.id)
    finally:
        await restored.destroy()
    # When no longer needed: await manager.delete_snapshot(snapshot.id)
```

Create from an existing template after its build reaches `Succeeded`:

```python
from datetime import timedelta

templated = await Sandbox.create_from_template(
    "your-published-template-id",
    timeout=timedelta(minutes=10),
    connection_config=config,
)
```

Add, replace, or remove metadata on a running sandbox:

```python
await sandbox.patch_metadata({"project": "demo", "obsolete-key": None})
```

## Configuration

### 1. Connection Configuration

The `ConnectionConfig` class manages API server connection settings.

| Parameter         | Description                                | Default                      | Environment Variable   |
| ----------------- | ------------------------------------------ | ---------------------------- | ---------------------- |
| `api_key`         | API Key for authentication                 | Optional; needed when server auth is enabled | `OPEN_SANDBOX_API_KEY` |
| `domain`          | The endpoint domain of the sandbox service | `localhost:8080` | `OPEN_SANDBOX_DOMAIN`  |
| `protocol`        | HTTP protocol (http/https)                 | `http`                       | -                      |
| `request_timeout` | Timeout for API requests                   | 30 seconds                   | -                      |
| `debug`           | Enable debug logging for HTTP requests     | `False`                      | -                      |
| `headers`         | Custom HTTP headers                        | Empty                        | -                      |
| `transport`       | Shared httpx transport (pool/proxy/retry); custom transports must honor request timeouts  | SDK-created per instance     | -                      |
| `retry_policy`    | Automatic retry policy for non-streaming requests (see [Automatic retries](#_2-automatic-retries)) | Enabled (`RetryPolicy()`) | -                 |
| `use_server_proxy` | Use sandbox server as proxy for execd/endpoint requests (e.g. when client cannot reach the sandbox directly) | `False` | -                      |
| `disable_metrics` | Disable SDK create-latency telemetry (see [SDK Telemetry](/sdks/observability#creation-metrics)) | `False` | `OPENSANDBOX_DISABLE_METRICS` |
| `enable_tracing` | Enable OpenTelemetry tracing for pool warmup (see [SDK Tracing](/sdks/observability#pool-warmup-tracing)) | `False` | - |

```python
from datetime import timedelta

# 1. Basic configuration
config = ConnectionConfig(
    api_key="your-key",
    domain="api.opensandbox.io",
    request_timeout=timedelta(seconds=60)
)

# 2. Advanced: Custom headers and custom transport
# If you create many Sandbox instances, configuring a shared transport is recommended to optimize resource usage.
# SDK default keep-alive is 30 seconds for its own transports.
import httpx

config = ConnectionConfig(
    api_key="your-key",
    domain="api.opensandbox.io",
    headers={
        "X-Custom-Header": "value",
        "X-Request-ID": "trace-123",
    },
    transport=httpx.AsyncHTTPTransport(
        limits=httpx.Limits(
            max_connections=100,
            max_keepalive_connections=50,
            keepalive_expiry=30.0,
        )
    ),
)

# If you provide a custom transport, you are responsible for closing it:
# await config.transport.aclose()
```

### 2. Automatic retries

The SDK retries transient failures automatically. `ConnectionConfig` /
`ConnectionConfigSync` install a retry wrapper around the default shared
transport, controlled by `retry_policy` (`opensandbox.transport.RetryPolicy`).

Default behavior:

- **Enabled by default.** Idempotent methods (`GET/HEAD/PUT/DELETE/OPTIONS`)
  are retried on `429`, `502`, `503`, and on pre-send transport failures
  (DNS, TCP connect, TLS handshake, fresh-connection reset).
- **`POST`/`PATCH` are never retried on a status code by default**, since the
  request may already have been applied server-side. Pre-send transport
  failures (before any byte is written) are still retried for these methods.
- Up to `3` retries with decorrelated-jitter exponential backoff, honoring a
  server `Retry-After` header (capped at 60s).
- **SSE / streaming requests bypass retry** entirely (bodies are not
  replayable).

::: warning Behavior change
Retries are on by default. This can increase the number of HTTP attempts and
tail latency compared to earlier SDK versions. If you rely on fast-fail
semantics, opt out explicitly with `RetryPolicy.disabled()`.
:::

```python
from datetime import timedelta

from opensandbox.transport import RetryPolicy

# Fast-fail: never retry (also skips fresh-connection recovery).
config = ConnectionConfig(
    api_key="your-key",
    domain="api.opensandbox.io",
    retry_policy=RetryPolicy.disabled(),
)

# Custom policy: more retries, an overall wall-clock deadline, and an
# opt-in to retry POST/PATCH on 503 (only safe if your endpoints are
# idempotent).
from http import HTTPStatus

config = ConnectionConfig(
    api_key="your-key",
    domain="api.opensandbox.io",
    retry_policy=RetryPolicy(
        max_retries=5,
        overall_deadline=timedelta(seconds=20),
        retryable_status_codes_non_idempotent=frozenset(
            {HTTPStatus.SERVICE_UNAVAILABLE}
        ),
    ),
)
```

::: info
If you pass a custom `transport`, the SDK does **not** wrap it; installing
retry behavior is then your responsibility.
:::

### 3. Sandbox Creation Configuration

The `Sandbox.create()` allows configuring the sandbox environment.

| Parameter       | Description                              | Default                         |
| --------------- | ---------------------------------------- | ------------------------------- |
| `image`    | Docker image specification               | One of image or snapshot ID |
| `timeout`       | Automatic termination timeout            | 10 minutes                      |
| `entrypoint`    | Container entrypoint command             | `["tail", "-f", "/dev/null"]`   |
| `resource`      | CPU and memory limits                    | `{"cpu": "1", "memory": "2Gi"}` |
| `env`           | Environment variables                    | Empty                           |
| `metadata`      | Custom metadata tags                     | Empty                           |
| `network_policy` | Optional outbound network policy (egress) | -                             |
| `credential_proxy` | Optional Credential Vault proxy startup settings | -                     |
| `ready_timeout` | Total budget for endpoint publication and health checks | 30 seconds                      |
| `snapshot_id` | Restore a snapshot instead of passing `image` | - |
| `resource_requests` | Kubernetes resource requests; must not exceed limits | - |
| `lifecycle` | Pre-start and periodic hooks | - |
| `platform` | OS/architecture constraint | - |
| `volumes` | Host, PVC, or OSSFS mounts | - |
| `secure_access` | Require endpoint access credentials | `False` |
| `skip_health_check` | Skip health checks; endpoint publication is still awaited | `False` |

::: warning
Metadata keys under `opensandbox.io/` are reserved for system-managed labels and will be rejected by the server.
:::

```python
from datetime import timedelta

from opensandbox.models.sandboxes import NetworkPolicy, NetworkRule

sandbox = await Sandbox.create(
    "python:3.11",
    connection_config=config,
    timeout=timedelta(minutes=30),
    resource={"cpu": "2", "memory": "4Gi"},
    env={"PYTHONPATH": "/app"},
    metadata={"project": "demo"},
    network_policy=NetworkPolicy(
        defaultAction="deny",
        egress=[NetworkRule(action="allow", target="pypi.org")],
    ),
)
```

### 4. Runtime Egress Policy Updates

Runtime egress policy routing depends on the sandbox origin.
For image-backed sandboxes, the SDK resolves port `18080` and calls the sidecar
`/policy` API. For template-backed sandboxes (including restored template snapshots),
the SDK detects `OPEN-SANDBOX-ORIGIN: template` and routes policy operations through
the lifecycle `/sandboxes/{sandboxId}/networkpolicy` API.

Patch uses merge semantics:
- Incoming rules take priority over existing rules with the same `target`.
- Existing rules for other targets remain unchanged.
- Within a single patch payload, the first rule for a `target` wins.
- The current `defaultAction` is preserved.

```python
from opensandbox.models.sandboxes import NetworkRule

policy = await sandbox.get_egress_policy()

await sandbox.patch_egress_rules(
    [
        NetworkRule(action="allow", target="www.github.com"),
        NetworkRule(action="deny", target="pypi.org"),
    ]
)
```

### 5. Credential Vault

Credential Vault requires a sandbox-side egress service and is unavailable for
template-backed sandboxes. It injects outbound credentials from the egress sidecar while
keeping real secrets out of sandbox environment variables, commands, files, and
logs. Create the sandbox with `credential_proxy` enabled, then write credentials
and bindings through `sandbox.credential_vault`.

```python
from opensandbox.models.sandboxes import (
    Credential,
    CredentialBinding,
    CredentialProxyConfig,
    NetworkPolicy,
    NetworkRule,
)

sandbox = await Sandbox.create(
    "python:3.11",
    connection_config=config,
    network_policy=NetworkPolicy(
        defaultAction="deny",
        egress=[NetworkRule(action="allow", target="api.example.com")],
    ),
    credential_proxy=CredentialProxyConfig(enabled=True),
)

await sandbox.credential_vault.create(
    credentials=[Credential(name="api-token", source={"value": "<token>"})],
    bindings=[
        CredentialBinding(
            name="api-token",
            match={
                "schemes": ["https"],
                "hosts": ["api.example.com"],
                "paths": ["/v1/*"],
            },
            auth={"type": "apiKey", "name": "x-api-key", "credential": "api-token"},
        )
    ],
)
```

See [Credential Vault](/guides/credential-vault) for auth types, binding
guidance, and Git/curl examples.
