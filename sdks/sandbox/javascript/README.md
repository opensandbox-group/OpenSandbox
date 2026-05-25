# Alibaba Sandbox SDK for JavaScript/TypeScript

English | [中文](README_zh.md)

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

The following example shows how to create a sandbox and execute a shell command.

> **Note**: Before running this example, ensure the OpenSandbox service is running. See the root [README.md](../../../README.md) for startup instructions.

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

  const execution = await sandbox.commands.run("echo 'Hello Sandbox!'");
  console.log(execution.logs.stdout[0]?.text);

  // Optional but recommended: terminate the remote instance when you are done.
  await sandbox.kill();
  await sandbox.close();
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

## Usage Examples

### 1. Lifecycle Management

Manage the sandbox lifecycle, including renewal, pausing, and resuming.

```ts
const info = await sandbox.getInfo();
console.log("State:", info.status.state);
console.log("Created:", info.createdAt);
console.log("Expires:", info.expiresAt); // null when manual cleanup mode is used

await sandbox.pause();

// Resume returns a fresh, connected Sandbox instance.
const resumed = await sandbox.resume();

// Renew: expiresAt = now + timeoutSeconds
await resumed.renew(30 * 60);
```

Create a non-expiring sandbox by passing `timeoutSeconds: null`:

```ts
const manual = await Sandbox.create({
  connectionConfig: config,
  image: "ubuntu",
  timeoutSeconds: null,
});
```

### 2. Custom Health Check

Define custom logic to determine whether the sandbox is ready/healthy. This overrides the default ping check used during readiness checks.

```ts
const sandbox = await Sandbox.create({
  connectionConfig: config,
  image: "nginx:latest",
  healthCheck: async (sbx) => {
    // Example: consider the sandbox healthy when port 80 endpoint becomes available
    const ep = await sbx.getEndpoint(80);
    return !!ep.endpoint;
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

### 4. Comprehensive File Operations

Manage files and directories, including read, write, list/search, and delete.

```ts
await sandbox.files.createDirectories([{ path: "/tmp/demo", mode: 755 }]);

await sandbox.files.writeFiles([
  { path: "/tmp/demo/hello.txt", data: "Hello World", mode: 644 },
]);

const content = await sandbox.files.readFile("/tmp/demo/hello.txt");
console.log("Content:", content);

const files = await sandbox.files.search({
  path: "/tmp/demo",
  pattern: "*.txt",
});
console.log(files.map((f) => f.path));

await sandbox.files.deleteDirectories(["/tmp/demo"]);
```

### 5. Endpoints

`getEndpoint()` returns an endpoint **without a scheme** (for example `"localhost:44772"`). Use `getEndpointUrl()` if you want a ready-to-use absolute URL (for example `"http://localhost:44772"`).

```ts
const { endpoint } = await sandbox.getEndpoint(44772);
const url = await sandbox.getEndpointUrl(44772);
```

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
const list = await manager.listSandboxInfos({
  states: ["Running"],
  pageSize: 10,
});
console.log(list.items.map((s) => s.id));
await manager.close();
```

## Configuration

### 1. Connection Configuration

The `ConnectionConfig` class manages API server connection settings.

Runtime notes:

- In browsers, the SDK uses the global `fetch` implementation.
- In Node.js, every `Sandbox` and `SandboxManager` clones the base `ConnectionConfig` via `withTransportIfMissing()`, so each instance gets an isolated `undici` keep-alive pool. Call `sandbox.close()` or `manager.close()` when you are done so the SDK can release the associated agent.

| Parameter               | Description                                                                                                  | Default          | Environment Variable   |
| ----------------------- | ------------------------------------------------------------------------------------------------------------ | ---------------- | ---------------------- |
| `apiKey`                | API key for authentication                                                                                   | Optional         | `OPEN_SANDBOX_API_KEY` |
| `domain`                | Sandbox service domain (`host[:port]`)                                                                       | `localhost:8080` | `OPEN_SANDBOX_DOMAIN`  |
| `protocol`              | HTTP protocol (`http`/`https`)                                                                               | `http`           | -                      |
| `requestTimeoutSeconds` | Request timeout applied to SDK HTTP calls                                                                    | `30`             | -                      |
| `debug`                 | Enable basic HTTP debug logging                                                                              | `false`          | -                      |
| `headers`               | Extra headers applied to every request                                                                       | `{}`             | -                      |
| `useServerProxy`        | Use sandbox server as proxy for execd/endpoint requests (e.g. when client cannot reach the sandbox directly) | `false`          | -                      |

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
| `image`                      | Docker image to use                              | Required                     |
| `timeoutSeconds`             | Automatic termination timeout (server-side TTL)  | 10 minutes                   |
| `entrypoint`                 | Container entrypoint command                     | `["tail","-f","/dev/null"]`  |
| `resource`                   | CPU and memory limits (string map)               | `{"cpu":"1","memory":"2Gi"}` |
| `env`                        | Environment variables                            | `{}`                         |
| `metadata`                   | Custom metadata tags                             | `{}`                         |
| `networkPolicy`              | Optional outbound network policy (egress)        | -                            |
| `extensions`                 | Extra server-defined fields                      | `{}`                         |
| `skipHealthCheck`            | Skip readiness checks (`Running` + health check) | `false`                      |
| `healthCheck`                | Custom readiness check                           | -                            |
| `readyTimeoutSeconds`        | Max time to wait for readiness                   | 30 seconds                   |
| `healthCheckPollingInterval` | Poll interval while waiting (milliseconds)       | 200 ms                       |

Note: metadata keys under `opensandbox.io/` are reserved for system-managed
labels and will be rejected by the server.

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

Runtime egress reads and patches go directly to the sandbox egress sidecar.
The SDK first resolves the sandbox endpoint on port `18080`, then calls the sidecar `/policy` API.

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

### 4. Resource cleanup

Both `Sandbox` and `SandboxManager` own a scoped HTTP agent when running on Node.js
so you can safely reuse the same `ConnectionConfig`. Once you are finished interacting
with the sandbox or administration APIs, call `sandbox.close()` / `manager.close()` to
release the underlying agent.

## Browser Notes

- The SDK can run in browsers, but **streaming file uploads are Node-only**.
- If you pass `ReadableStream` or `AsyncIterable` for `writeFiles`, the browser will fall back to **buffering in memory** before upload.
- Reason: browsers do not support streaming `multipart/form-data` bodies with custom boundaries (required by the execd upload API).
