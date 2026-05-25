# OpenSandbox API 规范文档

中文 | [English](README.md)

本目录包含 OpenSandbox 项目的 OpenAPI 规范文档，定义了完整的 API 接口和数据模型。发起请求时请使用各规范中定义的服务器地址（例如生命周期 API 的 `http://localhost:8080/v1`，execd 的 `http://localhost:44772`，egress 的 `http://localhost:18080`）。

## 规范文件

### 1. sandbox-lifecycle.yml

**沙箱生命周期管理 API**

定义了沙箱环境的创建、管理和销毁的完整生命周期接口，可从容器镜像创建或从快照恢复。

**核心功能：**
- **沙箱管理**：创建、列表、查询、删除沙箱实例，支持元数据过滤与分页
- **状态控制**：暂停 (Pause)、恢复 (Resume) 沙箱执行
- **生命周期**：支持 Pending → Running → Pausing → Paused → Stopping → Terminated，并包含错误态 `Failed`
- **资源与运行时配置**：指定 CPU/内存/GPU 资源限制、镜像启动 `entrypoint`、环境变量，以及自定义 `extensions`
- **镜像支持**：从公共或私有镜像仓库创建沙箱，支持私有仓库认证
- **超时管理**：创建时必填 `timeout`，并可通过 API 续期
- **端点访问**：获取沙箱内服务的公共访问端点
- **快照管理**：从沙箱创建快照、列出快照、删除快照

**主要端点（基础路径 `/v1`）：**
- `POST /sandboxes` - 从镜像或快照创建沙箱，设置超时与资源限制
- `GET /sandboxes` - 列出沙箱，支持状态/元数据过滤与分页
- `GET /sandboxes/{sandboxId}` - 获取完整沙箱详情（包含启动来源与 entrypoint）
- `DELETE /sandboxes/{sandboxId}` - 删除沙箱
- `POST /sandboxes/{sandboxId}/snapshots` - 从沙箱创建快照
- `GET /snapshots` - 列出快照，支持按沙箱过滤与分页
- `GET /snapshots/{snapshotId}` - 获取快照状态与元数据
- `DELETE /snapshots/{snapshotId}` - 删除快照
- `POST /sandboxes/{sandboxId}/pause` - 异步暂停沙箱
- `POST /sandboxes/{sandboxId}/resume` - 恢复已暂停的沙箱
- `POST /sandboxes/{sandboxId}/renew-expiration` - 续期沙箱 TTL
- `GET /sandboxes/{sandboxId}/endpoints/{port}` - 获取指定端口的访问端点

**认证方式：**
- HTTP Header: `OPEN-SANDBOX-API-KEY: your-api-key`
- 环境变量: `OPEN_SANDBOX_API_KEY`（SDK 客户端）

### 2. diagnostic-api.yml

**沙箱诊断 API**

定义用于排障的 best-effort 诊断描述符接口，用于获取沙箱诊断日志和事件。描述符可以内嵌纯文本诊断内容，也可以返回内容下载 URL。该规范不定义结构化审计或可观测性模型。

**主要端点（基础路径 `/v1`）：**
- `GET /sandboxes/{sandboxId}/diagnostics/logs` - 获取可选 scope 下的诊断日志内容描述符
- `GET /sandboxes/{sandboxId}/diagnostics/events` - 获取可选 scope 下的诊断事件内容描述符

**认证方式：**
- HTTP Header: `OPEN-SANDBOX-API-KEY: your-api-key`
- 环境变量: `OPEN_SANDBOX_API_KEY`（SDK 客户端）

### 3. execd-api.yaml

**沙箱内代码执行 API**

定义了在沙箱环境内执行代码、命令和文件操作的接口，提供完整的代码解释器和文件系统管理能力。所有端点需要 `X-EXECD-ACCESS-TOKEN` 认证头。

**核心功能：**
- **代码执行**：支持 Python、JavaScript 等多语言的有状态代码执行，并提供上下文生命周期管理
- **命令执行**：Shell 命令执行，支持前台/后台模式，并可通过轮询端点查看状态和输出
- **文件操作**：完整的文件和目录 CRUD 操作（创建、读取、更新、删除）
- **实时流式输出**：基于 SSE (Server-Sent Events) 的实时输出流
- **系统监控**：CPU 和内存指标的实时监控
- **访问控制**：通过 `X-EXECD-ACCESS-TOKEN` 进行 Token 认证

**主要端点分类：**

**健康检查：**
- `GET /ping` - 服务健康检查

**代码解释器：**
- `GET /code/contexts` - 列出活跃的代码执行上下文（可按语言过滤）
- `DELETE /code/contexts` - 按语言批量删除上下文
- `DELETE /code/contexts/{context_id}` - 删除指定上下文
- `POST /code/context` - 创建代码执行上下文
- `POST /code` - 在上下文中执行代码（流式输出）
- `DELETE /code` - 中断代码执行

**命令执行：**
- `POST /command` - 执行 Shell 命令（流式输出）
- `DELETE /command` - 中断命令执行
- `GET /command/status/{session}` - 查询前台/后台命令状态
- `GET /command/output/{session}` - 获取命令的累积 stdout/stderr

**文件系统：**
- `GET /files/info` - 获取文件元数据
- `DELETE /files` - 删除文件（不包含目录）
- `POST /files/permissions` - 修改文件权限
- `POST /files/mv` - 移动/重命名文件
- `GET /files/search` - 搜索文件（支持 glob 模式）
- `POST /files/replace` - 批量替换文件内容
- `POST /files/upload` - 上传文件（multipart）
- `GET /files/download` - 下载文件（支持断点续传）

**目录操作：**
- `POST /directories` - 按权限配置创建目录（mkdir -p 语义）
- `DELETE /directories` - 递归删除目录

**系统指标：**
- `GET /metrics` - 获取系统资源指标
- `GET /metrics/watch` - 实时监控系统指标（SSE 流）

### 4. egress-api.yaml

**沙箱 Egress 运行时 API**

定义了由沙箱内 egress sidecar 直接暴露的运行时策略接口。与生命周期 API 不同，
该 API 需要先解析沙箱 egress 端口对应的 endpoint，再直接访问 sidecar。

**核心功能：**
- **策略查询**：获取当前生效的 egress 策略及其运行时模式
- **策略变更**：使用 sidecar 的 merge 语义在运行时 patch egress 规则
- **直连 Sidecar**：不再通过生命周期 API 做服务端转发
- **可选鉴权**：当 egress sidecar 需要鉴权时，支持携带 endpoint 返回的请求头

**主要端点：**
- `GET /policy` - 获取当前 egress 策略
- `PATCH /policy` - 将新的 egress 规则合并到当前策略

## 技术特性

### 流式输出 (Server-Sent Events)

代码执行和命令执行接口使用 SSE 提供实时流式输出，支持以下事件类型：
- `init` - 初始化事件
- `status` - 状态更新
- `stdout` / `stderr` - 标准输出/错误流
- `result` - 执行结果
- `execution_complete` - 执行完成
- `execution_count` - 执行计数
- `error` - 错误信息

### 资源限制

支持灵活的资源配置（类似 Kubernetes）：
```json
{
  "cpu": "500m",
  "memory": "512Mi",
  "gpu": "1"
}
```

### 文件权限

支持 Unix 风格的文件权限管理：
- 所有者 (owner)
- 用户组 (group)
- 权限模式 (mode) - 八进制格式，如 755
