# Alibaba Sandbox JavaScript/TypeScript SDK

中文 | [English](README.md)

用于与 OpenSandbox 进行底层交互的 TypeScript/JavaScript SDK。它提供了创建、管理和与安全沙箱环境交互的能力，包括执行 Shell 命令、管理文件以及读取资源指标等。

## 安装指南

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

## 快速开始

以下示例展示了如何创建一个沙箱并执行 Shell 命令。

> **注意**: 在运行此示例之前，请确保 OpenSandbox 服务已启动。服务启动请参考根目录的 [README_zh.md](../../../docs/README_zh.md)。

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

  await sandbox.kill();
  await sandbox.close();
} catch (err) {
  if (err instanceof SandboxException) {
    console.error(`沙箱错误: [${err.error.code}] ${err.error.message ?? ""}`);
    console.error(`Request ID: ${err.requestId ?? "N/A"}`);
  } else {
    console.error(err);
  }
}
```

## 核心功能示例

### 1. 生命周期管理

管理沙箱的生命周期，包括续期、暂停、恢复和状态查询。

```ts
const info = await sandbox.getInfo();
console.log("状态:", info.status.state);
console.log("创建时间:", info.createdAt);
console.log("过期时间:", info.expiresAt);

await sandbox.pause();

// resume 会返回新的、已连接的 Sandbox 实例
const resumed = await sandbox.resume();

// renew：expiresAt = now + timeoutSeconds
await resumed.renew(30 * 60);

// 获取当前状态
const info = await resumed.getInfo();
console.log("状态:", info.status.state);
console.log("过期时间:", info.expiresAt); // 使用手动清理模式时为 null
```

通过传入 `timeoutSeconds: null` 创建一个不会自动过期的沙箱：

```ts
const manual = await Sandbox.create({
  connectionConfig: config,
  image: "ubuntu",
  timeoutSeconds: null,
});
```

### 2. 自定义健康检查

定义自定义逻辑来判断沙箱是否就绪/健康。这会覆盖“就绪检测”默认使用的 ping 检查逻辑。

```ts
const sandbox = await Sandbox.create({
  connectionConfig: config,
  image: "nginx:latest",
  healthCheck: async (sbx) => {
    // 示例：当 80 端口 endpoint 可获取时认为沙箱可用
    const ep = await sbx.getEndpoint(80);
    return !!ep.endpoint;
  },
});
```

### 3. 命令执行与流式响应

执行命令并实时处理输出流。

```ts
import type { ExecutionHandlers } from "@alibaba-group/opensandbox";

const handlers: ExecutionHandlers = {
  onStdout: (m) => console.log("STDOUT:", m.text),
  onStderr: (m) => console.error("STDERR:", m.text),
  onExecutionComplete: (c) => console.log("耗时(ms):", c.executionTimeMs),
};

await sandbox.commands.run(
  'for i in 1 2 3; do echo "Count $i"; sleep 0.2; done',
  undefined,
  handlers,
);
```

### 4. 全面的文件操作

管理文件和目录，包括读写、列表/搜索与删除。

```ts
await sandbox.files.createDirectories([{ path: "/tmp/demo", mode: 755 }]);

await sandbox.files.writeFiles([
  { path: "/tmp/demo/hello.txt", data: "Hello World", mode: 644 },
]);

const content = await sandbox.files.readFile("/tmp/demo/hello.txt");
console.log("文件内容:", content);

const files = await sandbox.files.search({
  path: "/tmp/demo",
  pattern: "*.txt",
});
console.log(files.map((f) => f.path));

await sandbox.files.deleteDirectories(["/tmp/demo"]);
```

### 5. Endpoint

`getEndpoint()` 返回 **不带 scheme** 的 endpoint（例如 `"localhost:44772"`）。如果你希望直接得到可用的绝对 URL（例如 `"http://localhost:44772"`），请使用 `getEndpointUrl()`。

```ts
const { endpoint } = await sandbox.getEndpoint(44772);
const url = await sandbox.getEndpointUrl(44772);
```

### 6. Volume 挂载

`volumes` 现在支持 `host`、`pvc` 和 `ossfs` 三种 backend。每个 volume 必须且只能指定其中一种。

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

### 7. 沙箱管理（Admin）

使用 `SandboxManager` 进行管理操作，如查询现有沙箱列表。

```ts
import { SandboxManager } from "@alibaba-group/opensandbox";

const manager = SandboxManager.create({ connectionConfig: config });
const list = await manager.listSandboxInfos({
  states: ["Running"],
  pageSize: 10,
});
console.log(list.items.map((s) => s.id));
```

## 配置说明

### 1. 连接配置 (Connection Configuration)

`ConnectionConfig` 类管理与 API 服务器的连接设置。

运行环境说明：

- 浏览器环境下，SDK 使用全局 `fetch`。
- Node.js 环境下，每个 `Sandbox` 和 `SandboxManager` 都会通过 `ConnectionConfig.withTransportIfMissing()` 创建独立的 keep-alive 池（基于 `undici`）。完成交互后请调用 `sandbox.close()` 或 `manager.close()` 来释放对应的 agent，以避免遗留连接，这与 Python SDK 的 transport 生命周期一致。

| 参数                    | 描述                                                                      | 默认值           | 环境变量               |
| ----------------------- | ------------------------------------------------------------------------- | ---------------- | ---------------------- |
| `apiKey`                | 用于认证的 API Key                                                        | 可选             | `OPEN_SANDBOX_API_KEY` |
| `domain`                | 沙箱服务域名（`host[:port]`）                                             | `localhost:8080` | `OPEN_SANDBOX_DOMAIN`  |
| `protocol`              | HTTP 协议（`http`/`https`）                                               | `http`           | -                      |
| `requestTimeoutSeconds` | SDK HTTP 请求超时（秒）                                                   | `30`             | -                      |
| `debug`                 | 是否开启基础 HTTP 调试日志                                                | `false`          | -                      |
| `headers`               | 每次请求附加的 Header                                                     | `{}`             | -                      |
| `useServerProxy`        | 是否通过沙箱服务代理访问 execd/endpoint（适用于客户端无法直连沙箱的场景） | `false`          | -                      |

```ts
import { ConnectionConfig } from "@alibaba-group/opensandbox";

// 1. 基础配置
const config = new ConnectionConfig({
  domain: "api.opensandbox.io",
  apiKey: "your-key",
  requestTimeoutSeconds: 60,
});

// 2. 进阶配置：自定义 headers
const config2 = new ConnectionConfig({
  domain: "api.opensandbox.io",
  apiKey: "your-key",
  headers: { "X-Custom-Header": "value" },
});
```

### 2. 沙箱创建配置 (Sandbox Creation Configuration)

`Sandbox.create()` 用于配置沙箱环境。

| 参数                         | 描述                                 | 默认值                       |
| ---------------------------- | ------------------------------------ | ---------------------------- |
| `image`                      | 使用的 Docker 镜像                   | 必填                         |
| `timeoutSeconds`             | 自动终止超时时间（服务端 TTL）       | 10 分钟                      |
| `entrypoint`                 | 容器启动入口命令                     | `["tail","-f","/dev/null"]`  |
| `resource`                   | CPU/内存限制（字符串 map）           | `{"cpu":"1","memory":"2Gi"}` |
| `env`                        | 环境变量                             | `{}`                         |
| `metadata`                   | 自定义元数据标签                     | `{}`                         |
| `networkPolicy`              | 可选的出站网络策略（egress）         | -                            |
| `extensions`                 | 额外的服务端扩展字段                 | `{}`                         |
| `skipHealthCheck`            | 跳过就绪检测（`Running` + 健康检查） | `false`                      |
| `healthCheck`                | 自定义就绪检查                       | -                            |
| `readyTimeoutSeconds`        | 等待就绪最大时间                     | 30 秒                        |
| `healthCheckPollingInterval` | 就绪轮询间隔（毫秒）                 | 200 ms                       |

注意：`opensandbox.io/` 前缀下的 metadata key 属于系统保留标签，服务端会拒绝用户传入。

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

### 3. 运行时 Egress 策略更新

运行时的 egress 查询和 patch 会直接访问沙箱内的 egress sidecar。
SDK 会先解析 `18080` 端口对应的 sandbox endpoint，再调用 sidecar 的 `/policy` API。

```ts
const policy = await sandbox.getEgressPolicy();

await sandbox.patchEgressRules([
  { action: "allow", target: "www.github.com" },
  { action: "deny", target: "pypi.org" },
]);
```

### 4. 资源清理

在 Node.js 环境下，`Sandbox` 和 `SandboxManager` 会拥有各自的 HTTP agent，因此即使多个实例共享同一个 `ConnectionConfig` 也不会互相影响。SDK 会借助 `ConnectionConfig.withTransportIfMissing()` 复刻每个实例的 transport。完成使用后调用 `sandbox.close()` / `manager.close()` 来释放底层连接池；

## 浏览器注意事项

- SDK 可在浏览器运行，但**流式文件上传仅支持 Node**。
- 如果 `writeFiles` 传入 `ReadableStream` 或 `AsyncIterable`，浏览器会回退为**先缓存在内存，再上传**。
- 原因：浏览器不支持以自定义 boundary 的 `multipart/form-data` 流式请求体（execd 上传接口需要此能力）。
