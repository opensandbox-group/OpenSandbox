---
title: JavaScript/TypeScript SDK
description: TypeScript/JavaScript SDK for creating, managing, and interacting with secure OpenSandbox environments.
---

# OpenSandbox SDK for JavaScript/TypeScript

A TypeScript/JavaScript SDK for low-level interaction with OpenSandbox. It provides the ability to create, manage, and interact with secure sandbox environments, including executing shell commands, managing files, and reading resource metrics.

## Installation

### npm

```bash
npm install @alibaba-group/opensandbox
```

### pnpm

```bash
pnpm add @alibaba-group/opensandbox
```

### yarn

```bash
yarn add @alibaba-group/opensandbox
```

## Quick Start

This example uses Node.js 20+ with ES modules and top-level `await`. It creates a
sandbox, runs a shell command, and releases both remote and local resources.

::: tip
Before running this example, ensure the OpenSandbox service is running. See the [Getting Started](/getting-started/) guide for startup instructions.
:::

```ts
import { ConnectionConfig, Sandbox, SandboxException } from "@alibaba-group/opensandbox";

const config = new ConnectionConfig({
  domain: "api.opensandbox.io",
  apiKey: "your-api-key",
  // protocol: "https",
  // requestTimeoutSeconds: 60,
});

try {
  const sandbox = await Sandbox.create({
    connectionConfig: config,
    image: "ubuntu",
    timeoutSeconds: 10 * 60,
  });

  try {
    const execution = await sandbox.commands.run("echo 'Hello Sandbox!'");
    console.log(execution.logs.stdout[0]?.text);
  } finally {
    try {
      await sandbox.kill();
    } finally {
      await sandbox.close();
    }
  }
} catch (err) {
  if (err instanceof SandboxException) {
    console.error(
      `Sandbox Error: [${err.error.code}] ${err.error.message ?? ""}`,
    );
    console.error(`Request ID: ${err.requestId ?? "N/A"}`);
  } else {
    console.error(err);
  }
}
```

## Lifecycle Hooks

Set `lifecycle` in `Sandbox.create`. `preStart` completes before the entrypoint starts, while `periodic` hooks run on their schedules after startup.

```ts
const sandbox = await Sandbox.create({
  connectionConfig: config,
  image: "ubuntu:24.04",
  lifecycle: {
    preStart: {
      command: ["sh", "-c", "echo ready > /tmp/prestart.done"],
      timeoutSeconds: 120,
    },
    periodic: [
      {
        name: "checkpoint",
        schedule: "@every 5m",
        command: ["sh", "-c", "date -u >> /tmp/checkpoints.log"],
        timeoutSeconds: 120,
      },
    ],
  },
});
```

The Server validates `timeoutSeconds`; `preStart` accepts 1–10800 seconds, while `periodic` accepts 1–300 seconds. Both default to 60 seconds when omitted. See [Lifecycle Hooks](/guides/lifecycle-hooks) for timing, failure behavior, and provider limitations.

## Client Pool and observability

`SandboxPool` provides in-memory and Redis-backed stores, four acquire policies,
and staged warmup controls. The Redis store is exported from
`@alibaba-group/opensandbox/pool-redis`. See [Client Pool](/guides/client-pool)
for examples, configuration, and namespace retirement.

Set `enableTracing: true` in `ConnectionConfig` to enable [pool warmup tracing](/sdks/observability#pool-warmup-tracing).
JavaScript emits phase spans but fewer diagnostic attributes than Python/JVM.
Create-latency [telemetry](/sdks/observability#creation-metrics) has a separate opt-out setting.
Remote diagnostic logs/events are available through [CLI or HTTP](/api/#diagnostics),
not through the JavaScript SDK.

## Usage Examples

The snippets below use `config` and a live `sandbox` from the quick start. Run
them before its cleanup block. Examples use TypeScript with top-level `await` in
Node.js; omit type annotations in JavaScript. Creation examples are alternatives.
Terminate each sandbox with `kill()` and release its client with `close()` when done.

### 1. Lifecycle Management

Manage the sandbox lifecycle, including renewal, pausing, and resuming.

```ts
const info = await sandbox.getInfo();
console.log("State:", info.status.state);
console.log("Created:", info.createdAt);
console.log("Expires:", info.expiresAt); // null when manual cleanup mode is used

await sandbox.pause();

const deadline = Date.now() + 120_000;
while (true) {
  if (Date.now() >= deadline) throw new Error("Sandbox did not pause within 120 seconds");
  const current = await sandbox.getInfo();
  if (current.status.state === "Paused") break;
  if (current.status.state === "Failed") throw new Error(current.status.message);
  await new Promise((resolve) => setTimeout(resolve, 1000));
}

// Resume returns a fresh handle; close both handles when done.
const resumed = await sandbox.resume();
try {
  await resumed.renew(30 * 60); // expiresAt = now + timeoutSeconds
} finally {
  await resumed.close();
}
```

Pause is asynchronous and runtime-dependent. See [Pause and Resume](/guides/pause-resume).
Attach to an already running sandbox with
`Sandbox.connect({ sandboxId, connectionConfig: config })`.

Create a non-expiring sandbox by passing `timeoutSeconds: null`:

```ts
const manual = await Sandbox.create({
  connectionConfig: config,
  image: "ubuntu",
  timeoutSeconds: null,
});
```

### 2. Custom Health Check

Resolving an endpoint confirms that a route exists; it does not confirm that the
application on that port is healthy. For service readiness, make a bounded request
to the application's health endpoint and include the returned endpoint headers.

Define custom logic to determine whether the sandbox is ready/healthy. This overrides the default ping check. Checks must not block the event loop and may continue running after timeout.

```ts
const sandbox = await Sandbox.create({
  connectionConfig: config,
  image: "nginx:latest",
  entrypoint: ["nginx", "-g", "daemon off;"],
  healthCheck: async (sbx) => {
    const ep = await sbx.getEndpoint(80);
    const url = await sbx.getEndpointUrl(80);
    try {
      const response = await fetch(url, {
        headers: ep.headers,
        signal: AbortSignal.timeout(2000),
      });
      await response.body?.cancel();
      return response.status === 200;
    } catch {
      return false;
    }
  },
});
```

### 3. Command Execution & Streaming

Execute commands and handle output streams in real-time.

```ts
import type { ExecutionHandlers } from "@alibaba-group/opensandbox";

const handlers: ExecutionHandlers = {
  onStdout: (m) => console.log("STDOUT:", m.text),
  onStderr: (m) => console.error("STDERR:", m.text),
  onExecutionComplete: (c) =>
    console.log("Finished in", c.executionTimeMs, "ms"),
};

await sandbox.commands.run(
  'for i in 1 2 3; do echo "Count $i"; sleep 0.2; done',
  undefined,
  handlers,
);
```

To execute a native program without shell parsing, pass an argument list. On Linux,
this example prints literal `$HOME` and keeps `hello world` as one argument:

```ts
await sandbox.commands.run(["printf", "%s\n", "$HOME", "hello world"]);
```

Native argv execution requires an updated execd. See [command execution modes](/architecture/data-plane/execd#command-execution) for executable lookup and platform behavior.

#### Background commands

Poll status and incremental logs. The command timeout is separate from sandbox TTL.

```ts
const execution = await sandbox.commands.run(
  'for i in 1 2 3; do echo "step $i"; sleep 1; done',
  { background: true, timeoutSeconds: 30 },
);
if (!execution.id) throw new Error("No command ID returned");
let cursor = 0;
const deadline = Date.now() + 45_000;
while (true) {
  if (Date.now() >= deadline) {
    await sandbox.commands.interrupt(execution.id);
    throw new Error("Command did not finish");
  }
  const status = await sandbox.commands.getCommandStatus(execution.id);
  const logs = await sandbox.commands.getBackgroundCommandLogs(execution.id, cursor);
  process.stdout.write(logs.content);
  cursor = logs.cursor ?? cursor;
  if (status.running === false) {
    if (status.exitCode !== 0) throw new Error(`Command failed: ${status.exitCode}, ${status.error}`);
    break;
  }
  await new Promise((resolve) => setTimeout(resolve, 500));
}
```

#### Persistent shell sessions

A Bash session preserves shell variables and the working directory across commands.

```ts
const sessionId = await sandbox.commands.createSession({ workingDirectory: "/tmp" });
try {
  await sandbox.commands.runInSession(sessionId, "export DEMO=hello");
  const result = await sandbox.commands.runInSession(sessionId, 'echo "$DEMO"; pwd');
  console.log(result.logs.stdout.map((message) => message.text).join(""));
} finally {
  await sandbox.commands.deleteSession(sessionId);
}
```

#### Persistent environment variables

Set environment variables that the runtime injects into every subsequent command
and session — without hand-writing shell escaping against the sandbox env file.

```ts
await sandbox.commands.setEnv("MY_TOKEN", "it's a safe value");
```

Keys must match `[A-Za-z_][A-Za-z0-9_]*`. Values are stored verbatim with proper
escaping (quotes, backslashes, newlines, `=`). The env file is append-only: the
last write for a key wins. Throws if the sandbox fails to persist the variable.

For filesystem/process isolation within a sandbox, see
[Isolation Sessions](/guides/isolation-sessions). These are separate from Bash sessions.

### 4. File Operations

Manage files and directories, including read, write, list/search, and delete.

```ts
await sandbox.files.createDirectories([{ path: "/tmp/demo", mode: 755 }]);

await sandbox.files.writeFiles([
  { path: "/tmp/demo/hello.txt", data: "Hello World", mode: 644 },
]);

const content = await sandbox.files.readFile("/tmp/demo/hello.txt");
console.log("Content:", content);

const entries = await sandbox.files.listDirectory({ path: "/tmp/demo", depth: 1 });
console.log(entries.map((entry) => entry.path));

const files = await sandbox.files.search({
  path: "/tmp/demo",
  pattern: "*.txt",
});
console.log(files.map((f) => f.path));

await sandbox.files.deleteDirectories(["/tmp/demo"]);
```

For binary files, pass a `Uint8Array` to `writeFiles()` and read with `readBytes()`
or `readBytesStream()`. Read options support `offset` and `limit` for partial downloads.

### 5. Endpoints

`getEndpoint()` returns an endpoint **without a scheme** (for example `"localhost:44772"`). Use `getEndpointUrl()` if you want a ready-to-use absolute URL (for example `"http://localhost:44772"`).

```ts
const endpoint = await sandbox.getEndpoint(44772);
const url = await sandbox.getEndpointUrl(44772);
console.log(url, Object.keys(endpoint.headers ?? {}));
```

When making an HTTP request to a sandbox service, forward `endpoint.headers`,
including credentials required by secure access. The health-check example above
shows a request to an application that is actually listening on the target port.

### 6. Volume Mounts

`volumes` supports `host`, `pvc`, and `ossfs` backends. Each volume must specify exactly one backend.

```ts
const sandbox = await Sandbox.create({
  connectionConfig: config,
  image: "ubuntu",
  volumes: [
    {
      name: "oss-data",
      ossfs: {
        bucket: "bucket-a",
        endpoint: "oss-cn-hangzhou.aliyuncs.com",
        accessKeyId: process.env.OSS_ACCESS_KEY_ID!,
        accessKeySecret: process.env.OSS_ACCESS_KEY_SECRET!,
        version: "2.0",
      },
      mountPath: "/mnt/oss",
      subPath: "prefix",
    },
  ],
});
```

### 7. Sandbox Management (Admin)

Use `SandboxManager` for administrative tasks and finding existing sandboxes.

```ts
import { SandboxManager } from "@alibaba-group/opensandbox";

const manager = SandboxManager.create({ connectionConfig: config });
try {
  // First page only; increase page for subsequent pages.
  const list = await manager.listSandboxInfos({
    states: ["Running"], pageSize: 10, page: 1,
  });
  console.log(list.items.map((s) => s.id));
} finally {
  await manager.close();
}
```

### Resource metrics

Read current sandbox resource usage with `await sandbox.getMetrics()`. This is
separate from [SDK creation telemetry](/sdks/observability#creation-metrics).

## Snapshots, templates, and metadata

| Operation | Public API |
| --- | --- |
| Snapshot a sandbox | `manager.createSnapshot(sandboxId, { name })` |
| Inspect/list/delete snapshots | `manager.getSnapshot`, `listSnapshots`, `deleteSnapshot` |
| Restore a snapshot | `Sandbox.create({ snapshotId, connectionConfig })` |
| Manage Fsb templates | `manager.createTemplate`, `getTemplate`, `listTemplates`, `deleteTemplate` |
| Create from a published template | `Sandbox.createFromTemplate({ templateId, timeoutSeconds, connectionConfig })` |
| Patch metadata | `sandbox.patchMetadata` or `manager.patchSandboxMetadata` |

Poll snapshot status before restoring. Template builds are asynchronous; wait
for `status.phase === "Succeeded"` before use. Template-backed creation requires
a TTL and inherits workload configuration from the published template.
Metadata patch values add/replace keys; `null` deletes a key.
See the [lifecycle contract](/api/#1-sandbox-lifecycle-yml) for backend constraints.

Snapshot support depends on the runtime and server configuration. Renew the source
sandbox first if its remaining TTL may expire during snapshot creation. Wait for
`Ready` before restoring; the snapshot remains available after this example:

```ts
import { SandboxManager } from "@alibaba-group/opensandbox";

const manager = SandboxManager.create({ connectionConfig: config });
try {
  let snapshot = await manager.createSnapshot(sandbox.id, { name: "demo" });
  console.log("Snapshot:", snapshot.id);
  const deadline = Date.now() + 900_000;
  while (true) {
    if (Date.now() >= deadline) throw new Error(`Snapshot ${snapshot.id} is not ready`);
    snapshot = await manager.getSnapshot(snapshot.id);
    if (snapshot.status.state === "Ready") break;
    if (snapshot.status.state === "Failed") throw new Error(snapshot.status.message);
    await new Promise((resolve) => setTimeout(resolve, 2000));
  }
  const restored = await Sandbox.create({ snapshotId: snapshot.id, connectionConfig: config });
  try {
    console.log(restored.id);
  } finally {
    try { await restored.kill(); } finally { await restored.close(); }
  }
  // When no longer needed: await manager.deleteSnapshot(snapshot.id);
} finally {
  await manager.close();
}
```

Use an existing template after its build reaches `Succeeded`:

```ts
const templated = await Sandbox.createFromTemplate({
  templateId: "your-published-template-id",
  timeoutSeconds: 600,
  connectionConfig: config,
});
```

Add/replace a metadata key and remove another:

```ts
await sandbox.patchMetadata({ project: "demo", "obsolete-key": null });
```

## Configuration

### 1. Connection Configuration

The `ConnectionConfig` class manages API server connection settings.

::: info Runtime Notes
- In browsers, the SDK uses the global `fetch` implementation.
- In Node.js, every `Sandbox` and `SandboxManager` clones the base `ConnectionConfig` via `withTransportIfMissing()`, so each instance gets an isolated `undici` keep-alive pool. Call `sandbox.close()` or `manager.close()` when you are done so the SDK can release the associated agent.
:::

| Parameter               | Description                                                                                                  | Default          | Environment Variable   |
| ----------------------- | ------------------------------------------------------------------------------------------------------------ | ---------------- | ---------------------- |
| `apiKey`                | API key for authentication                                                                                   | Optional         | `OPEN_SANDBOX_API_KEY` |
| `domain`                | Sandbox service domain (`host[:port]`)                                                                       | `localhost:8080` | `OPEN_SANDBOX_DOMAIN`  |
| `protocol`              | HTTP protocol (`http`/`https`)                                                                               | `http`           | -                      |
| `requestTimeoutSeconds` | Request timeout applied to SDK HTTP calls                                                                    | `30`             | -                      |
| `debug`                 | Enable basic HTTP debug logging                                                                              | `false`          | -                      |
| `headers`               | Extra headers applied to every request                                                                       | `{}`             | -                      |
| `useServerProxy`        | Use sandbox server as proxy for execd/endpoint requests (e.g. when client cannot reach the sandbox directly) | `false`          | -                      |
| `enableTracing` | Enable [pool warmup tracing](/sdks/observability#pool-warmup-tracing) | `false` | - |
| `disableMetrics`        | Disable SDK create-latency telemetry (see [SDK Telemetry](/sdks/observability#creation-metrics))                          | `false`          | `OPENSANDBOX_DISABLE_METRICS` |

```ts
import { ConnectionConfig } from "@alibaba-group/opensandbox";

// 1. Basic configuration
const config = new ConnectionConfig({
  domain: "api.opensandbox.io",
  apiKey: "your-key",
  requestTimeoutSeconds: 60,
});

// 2. Advanced: custom headers
const config2 = new ConnectionConfig({
  domain: "api.opensandbox.io",
  apiKey: "your-key",
  headers: { "X-Custom-Header": "value" },
});
```

### 2. Sandbox Creation Configuration

`Sandbox.create()` allows configuring the sandbox environment.

| Parameter                    | Description                                      | Default                      |
| ---------------------------- | ------------------------------------------------ | ---------------------------- |
| `image`                      | Docker image to use                              | One of image or snapshot ID |
| `timeoutSeconds`             | Automatic termination timeout (server-side TTL)  | 10 minutes                   |
| `entrypoint`                 | Container entrypoint command                     | `["tail","-f","/dev/null"]`  |
| `resource`                   | CPU and memory limits (string map)               | `{"cpu":"1","memory":"2Gi"}` |
| `env`                        | Environment variables                            | `{}`                         |
| `metadata`                   | Custom metadata tags                             | `{}`                         |
| `networkPolicy`              | Optional outbound network policy (egress)        | -                            |
| `credentialProxy`            | Optional Credential Vault proxy startup settings | -                            |
| `extensions`                 | Extra server-defined fields                      | `{}`                         |
| `skipHealthCheck`            | Skip readiness checks (`Running` + health check) | `false`                      |
| `healthCheck`                | Custom readiness check                           | -                            |
| `readyTimeoutSeconds`        | Max time to wait for readiness                   | 30 seconds                   |
| `healthCheckPollingInterval` | Poll interval while waiting (milliseconds)       | 200 ms                       |
| `snapshotId` | Restore a snapshot instead of passing `image` | - |
| `resourceRequests` | Kubernetes resource requests; must not exceed limits | - |
| `lifecycle` | Pre-start and periodic hooks | - |
| `platform` | OS/architecture constraint | - |
| `volumes` | Host, PVC, or OSSFS mounts | - |
| `secureAccess` | Require endpoint access credentials | `false` |

::: warning
Metadata keys under `opensandbox.io/` are reserved for system-managed labels and will be rejected by the server.
:::

```ts
const sandbox = await Sandbox.create({
  connectionConfig: config,
  image: "python:3.11",
  networkPolicy: {
    defaultAction: "deny",
    egress: [{ action: "allow", target: "pypi.org" }],
  },
});
```

### 3. Runtime Egress Policy Updates

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

```ts
const policy = await sandbox.getEgressPolicy();

await sandbox.patchEgressRules([
  { action: "allow", target: "www.github.com" },
  { action: "deny", target: "pypi.org" },
]);
```

### 4. Credential Vault

Credential Vault requires a sandbox-side egress service and is unavailable for
template-backed sandboxes. It injects outbound credentials from the egress sidecar while
keeping real secrets out of sandbox environment variables, commands, files, and
logs. Create the sandbox with `credentialProxy` enabled, then write credentials
and bindings through `sandbox.credentialVault`.

```ts
const sandbox = await Sandbox.create({
  connectionConfig: config,
  image: "python:3.11",
  networkPolicy: {
    defaultAction: "deny",
    egress: [{ action: "allow", target: "api.example.com" }],
  },
  credentialProxy: { enabled: true },
});

await sandbox.credentialVault.create({
  credentials: [{ name: "api-token", source: { value: "<token>" } }],
  bindings: [
    {
      name: "api-token",
      match: {
        schemes: ["https"],
        hosts: ["api.example.com"],
        paths: ["/v1/*"],
      },
      auth: { type: "apiKey", name: "x-api-key", credential: "api-token" },
    },
  ],
});
```

See [Credential Vault](/guides/credential-vault) for auth types, binding
guidance, and Git/curl examples.

### 5. Resource Cleanup

Both `Sandbox` and `SandboxManager` own a scoped HTTP agent when running on Node.js
so you can safely reuse the same `ConnectionConfig`. Once you are finished interacting
with the sandbox or administration APIs, call `sandbox.close()` / `manager.close()` to
release the underlying agent.

## Browser Notes

::: warning
- The SDK can run in browsers, but **streaming file uploads are Node-only**.
- If you pass `ReadableStream` or `AsyncIterable` for `writeFiles`, the browser will fall back to **buffering in memory** before upload.
- Reason: browsers do not support streaming `multipart/form-data` bodies with custom boundaries (required by the execd upload API).
:::
