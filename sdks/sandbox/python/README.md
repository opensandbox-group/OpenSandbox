# OpenSandbox SDK for Python

English | [中文](README_zh.md)

A Python SDK for low-level interaction with OpenSandbox. It provides capabilities to create, manage, and interact with secure sandbox environments, including executing shell commands, managing files, and monitoring resources.

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

> **Note**: Before running this example, ensure the OpenSandbox service is running. See the root [README.md](../../../README.md) for startup instructions.

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
        async with sandbox:

            # 3. Execute a shell command
            execution = await sandbox.commands.run("echo 'Hello Sandbox!'")

            # 4. Print output
            print(execution.logs.stdout[0].text)

            # 5. Cleanup (sandbox.close() called automatically)
            # Note: kill() must be called explicitly if you want to terminate the remote sandbox instance immediately
            await sandbox.kill()

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

import httpx
from opensandbox import SandboxSync
from opensandbox.config import ConnectionConfigSync

config = ConnectionConfigSync(
    domain="api.opensandbox.io",
    api_key="your-api-key",
    request_timeout=timedelta(seconds=30),
    transport=httpx.HTTPTransport(limits=httpx.Limits(max_connections=20)),
)

sandbox = SandboxSync.create("ubuntu", connection_config=config)
with sandbox:
    execution = sandbox.commands.run("echo 'Hello Sandbox!'")
    print(execution.logs.stdout[0].text)
    sandbox.kill()
```

### Synchronous Sandbox Pool

`SandboxPoolSync` keeps a buffer of ready sandboxes to reduce acquire latency. The
pool API is synchronous and aligned with the Kotlin `SandboxPool` semantics: acquire
is allowed on any node, while replenish/shrink is gated by the store's primary lock.

```python
from datetime import timedelta

from opensandbox import (
    AcquirePolicy,
    InMemoryPoolStateStore,
    PoolCreationSpec,
    SandboxPoolSync,
)
from opensandbox.config import ConnectionConfigSync

pool = SandboxPoolSync(
    pool_name="demo-pool",
    owner_id="worker-1",
    max_idle=2,
    state_store=InMemoryPoolStateStore(),  # single-process only
    connection_config=ConnectionConfigSync(domain="api.opensandbox.io"),
    creation_spec=PoolCreationSpec(image="ubuntu:22.04"),
    reconcile_interval=timedelta(seconds=5),
)

pool.start()
try:
    sandbox = pool.acquire(
        sandbox_timeout=timedelta(minutes=30),
        policy=AcquirePolicy.FAIL_FAST,
    )
    try:
        result = sandbox.commands.run("echo pool-ok")
        print(result.logs.stdout[0].text)
    finally:
        sandbox.kill()
        sandbox.close()
finally:
    pool.shutdown(graceful=True)
```

Use `SandboxPoolAsync` in asyncio applications so pool acquire, warmup, health
checks, and lifecycle calls do not block the event loop:

```python
from datetime import timedelta

from opensandbox import (
    AcquirePolicy,
    InMemoryAsyncPoolStateStore,
    PoolCreationSpec,
    SandboxPoolAsync,
)
from opensandbox.config import ConnectionConfig

pool = SandboxPoolAsync(
    pool_name="demo-pool",
    owner_id="worker-1",
    max_idle=2,
    state_store=InMemoryAsyncPoolStateStore(),  # single-event-loop only
    connection_config=ConnectionConfig(domain="api.opensandbox.io"),
    creation_spec=PoolCreationSpec(image="ubuntu:22.04"),
)

await pool.start()
try:
    sandbox = await pool.acquire(
        sandbox_timeout=timedelta(minutes=30),
        policy=AcquirePolicy.FAIL_FAST,
    )
    try:
        result = await sandbox.commands.run("echo pool-ok")
        print(result.logs.stdout[0].text)
    finally:
        await sandbox.kill()
        await sandbox.close()
finally:
    await pool.shutdown(graceful=True)
```

For Python production services with multiple processes or pods, use Redis-backed
pool state. Install the optional dependency:

```bash
pip install "opensandbox[pool-redis]"
```

Create and configure the Redis client yourself, then pass it to `RedisPoolStateStore`.
The store does not create or close Redis clients.

```python
import redis

from opensandbox import PoolCreationSpec, SandboxPoolSync
from opensandbox.config import ConnectionConfigSync
from opensandbox.pool_redis import RedisPoolStateStore

redis_client = redis.Redis.from_url(
    "redis://user:password@redis.example.com:6379/0",
    decode_responses=True,
)

pool = SandboxPoolSync(
    pool_name="prod-pool",
    owner_id="worker-1",
    max_idle=10,
    state_store=RedisPoolStateStore(redis_client, key_prefix="opensandbox:pool:prod"),
    connection_config=ConnectionConfigSync(domain="api.opensandbox.io"),
    creation_spec=PoolCreationSpec(image="ubuntu:22.04"),
    primary_lock_ttl=timedelta(seconds=60),
)
```

For async pools, pass a `redis.asyncio` client to `AsyncRedisPoolStateStore`.

Notes:

- `InMemoryPoolStateStore` is for single-process development and tests. It is not
  a process-wide or pod-wide pool for gunicorn, uvicorn workers, Celery, or Kubernetes.
- `max_idle` is the target/cap for ready idle sandboxes. It is not a global limit
  on borrowed or directly-created sandboxes.
- For distributed deployment, all nodes in one logical pool must share the same
  `key_prefix` and `pool_name`.
- Each running process should use a unique `owner_id`; it identifies the primary
  lock owner and is not the pool identifier.
- All nodes sharing one pool must use the same creation and warmup definition. If
  that definition changes, use a new `pool_name` or `key_prefix` and drain the old pool.
- `resize(max_idle)` can be called from any node. The call returns after the new
  idle target is stored in the shared state store; the current primary applies
  replenish or shrink work during periodic reconcile.
- Use `resize(0)` and wait for `snapshot().idle_count == 0` to drain a distributed
  idle buffer. `release_all_idle()` is only a best-effort cleanup pass in distributed
  mode because another primary may put new idle sandboxes concurrently unless the
  shared target has already been reduced.
- Configure `primary_lock_ttl` greater than `warmup_ready_timeout` plus expected
  warmup preparer time and buffer.
- Redis outages are surfaced as pool state store errors. The pool fails closed; it
  does not bypass shared state.

## Usage Examples

### 1. Lifecycle Management

Manage the sandbox lifecycle, including renewal, pausing, and resuming.

```python
from datetime import timedelta

# Renew the sandbox
# This resets the expiration time to (current time + duration)
await sandbox.renew(timedelta(minutes=30))

# Pause execution (suspends all processes)
await sandbox.pause()

# Resume execution
sandbox = await Sandbox.resume(
    sandbox_id=sandbox.id,
    connection_config=config,
)

# Get current status
info = await sandbox.get_info()
print(f"State: {info.status.state}")
print(f"Expires: {info.expires_at}")  # None when no automatic expiration is configured
```

Create a non-expiring sandbox by omitting `timeout`:

```python
manual = await Sandbox.create(
    "ubuntu",
    connection_config=config,
)
```

### 2. Custom Health Check

Define custom logic to determine if the sandbox is healthy. This overrides the default ping check.

```python
async def custom_health_check(sbx: Sandbox) -> bool:
    try:
        # 1. Get the external mapped address for port 80
        endpoint = await sbx.get_endpoint(80)

        # 2. Perform your connection check (e.g. HTTP request, Socket connect)
        # return await check_connection(endpoint.endpoint)
        return True
    except Exception:
        return False

sandbox = await Sandbox.create(
    "nginx:latest",
    connection_config=config,
    health_check=custom_health_check  # Custom check: Wait for port 80 to be accessible
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

### 4. Comprehensive File Operations

Manage files and directories, including read, write, list, delete, and search.

```python
from opensandbox.models.filesystem import WriteEntry, SearchEntry

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

# 3. List/Search files
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

### 5. Sandbox Management (Admin)

Use `SandboxManager` for administrative tasks and finding existing sandboxes.

```python
from opensandbox.manager import SandboxManager
from opensandbox.models.sandboxes import SandboxFilter

# Create manager using async context manager
async with await SandboxManager.create(connection_config=config) as manager:

    # List running sandboxes
    sandboxes = await manager.list_sandbox_infos(
        SandboxFilter(
            states=["RUNNING"],
            page_size=10
        )
    )

    for info in sandboxes.sandbox_infos:
        print(f"Found sandbox: {info.id}")
        # Perform admin actions
        await manager.kill_sandbox(info.id)
```

## Configuration

### 1. Connection Configuration

The `ConnectionConfig` class manages API server connection settings.

| Parameter         | Description                                | Default                      | Environment Variable   |
| ----------------- | ------------------------------------------ | ---------------------------- | ---------------------- |
| `api_key`         | API Key for authentication                 | Required                     | `OPEN_SANDBOX_API_KEY` |
| `domain`          | The endpoint domain of the sandbox service | Required (or localhost:8080) | `OPEN_SANDBOX_DOMAIN`  |
| `protocol`        | HTTP protocol (http/https)                 | `http`                       | -                      |
| `request_timeout` | Timeout for API requests                   | 30 seconds                   | -                      |
| `debug`           | Enable debug logging for HTTP requests     | `False`                      | -                      |
| `headers`         | Custom HTTP headers                        | Empty                        | -                      |
| `transport`       | Shared httpx transport (pool/proxy/retry)  | SDK-created per instance     | -                      |
| `use_server_proxy` | Use sandbox server as proxy for execd/endpoint requests (e.g. when client cannot reach the sandbox directly) | `False` | -                      |

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

### 2. Sandbox Creation Configuration

The `Sandbox.create()` allows configuring the sandbox environment.

| Parameter       | Description                              | Default                         |
| --------------- | ---------------------------------------- | ------------------------------- |
| `image`    | Docker image specification               | Required                        |
| `timeout`       | Automatic termination timeout            | 10 minutes                      |
| `entrypoint`    | Container entrypoint command             | `["tail", "-f", "/dev/null"]`   |
| `resource`      | CPU and memory limits                    | `{"cpu": "1", "memory": "2Gi"}` |
| `env`           | Environment variables                    | Empty                           |
| `metadata`      | Custom metadata tags                     | Empty                           |
| `network_policy` | Optional outbound network policy (egress) | -                             |
| `ready_timeout` | Max time to wait for sandbox to be ready | 30 seconds                      |

Note: metadata keys under `opensandbox.io/` are reserved for system-managed
labels and will be rejected by the server.

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

### 3. Runtime Egress Policy Updates

Runtime egress policy reads and patches are sent directly to the sandbox egress sidecar.
The SDK first resolves the sandbox endpoint on port `18080`, then calls the sidecar `/policy` API.

Patch uses merge semantics:
- Incoming rules take priority over existing rules with the same `target`.
- Existing rules for other targets remain unchanged.
- Within a single patch payload, the first rule for a `target` wins.
- The current `defaultAction` is preserved.

```python
policy = await sandbox.get_egress_policy()

await sandbox.patch_egress_rules(
    [
        NetworkRule(action="allow", target="www.github.com"),
        NetworkRule(action="deny", target="pypi.org"),
    ]
)
```
