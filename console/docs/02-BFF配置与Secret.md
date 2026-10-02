# BFF 配置与 Secret

## 环境变量

| 变量 | 必填 | 说明 |
|------|------|------|
| `LIFECYCLE_API_BASE` | 是 | Lifecycle 根 URL，含 `/v1`，如 `http://opensandbox-server.opensandbox-system.svc:8090/v1` 或 `https://opensandbox.example.com/v1` |
| `TENANTS_TOML_PATH` | 是 | **`tenants.toml` 文件路径**（纯 TOML，与 Server 挂载内容一致） |
| `BFF_SESSION_SECRET` | 是 | 签名 session cookie，≥32 字符随机 |
| `BFF_ADMIN_TOKEN` | 是 | Admin 登录口令；**勿写入前端**；与租户 Key 无关 |
| `BFF_COOKIE_SECURE` | 否 | 生产 HTTPS 设为 `true` |
| `BFF_CORS_ORIGINS` | 否 | 逗号分隔 SPA 源，默认 `*` 仅 dev |
| `BFF_AGGREGATE_CACHE_SECONDS` | 否 | Admin list 缓存秒数，默认 `0`（不缓存） |
| `BFF_HTTP_TIMEOUT_SECONDS` | 否 | 调 Lifecycle 超时，默认 `30` |

### 可选：Node Agent 集成

| 变量 | 默认 | 说明 |
|------|------|------|
| `BFF_K8S_PROBE_ENABLED` | `false` | K8s 探针总开关：platform 组件、node-agent、workloads/events API |
| `BFF_K8S_INSECURE_SKIP_TLS_VERIFY` | `false` | 跳过访问 Kubernetes API 的 TLS 校验（仅当 Pod 内 SA `ca.crt` 与 apiserver 证书链不一致时的临时手段；与 Server `[kubernetes] insecure_skip_tls_verify` 独立，需分别配置） |
| `BFF_K8S_SYSTEM_NAMESPACE` | `opensandbox-system` | controller/ingress Deployment 所在 namespace |
| `BFF_K8S_CONTROLLER_DEPLOYMENT` | `opensandbox-controller-manager` | 与 chart 固定 Deployment 名一致；`kubectl get deploy -n opensandbox-system` 核对 |
| `BFF_K8S_INGRESS_DEPLOYMENT` | `opensandbox-ingress-gateway` | 同上 |
| `BFF_K8S_NODE_AGENT_NAMESPACE` | `opensandbox-system` | DaemonSet 所在 namespace |
| `BFF_K8S_NODE_AGENT_LABEL_SELECTOR` | `app.kubernetes.io/component=node-agent` | Pod 标签选择器 |
| `BFF_K8S_NODE_AGENT_PROBE_PORT` | `8080` | node-agent 健康端口 |
| `BFF_NODEAGENT_ARCHIVE_ENABLED` | `false` | 启用 `GET .../logs/archive` |
| `BFF_NODEAGENT_CLUSTER_ID` | `dev-cluster` | 与 Helm `opensandbox-node-agent.config.clusterID` 一致 |
| `BFF_NODEAGENT_OSS_*` | 空 | OSS 只读凭证；**与 node-agent 写路径同源**（`keyPrefix/cluster/namespace/sandboxId/...`） |
| `BFF_NODEAGENT_ARCHIVE_MAX_BYTES` | `524288` | 单次响应最大字节（尾部截断） |

### 可选：沙箱申请历史（PostgreSQL）

| 变量 | 默认 | 说明 |
|------|------|------|
| `BFF_HISTORY_ENABLED` | `false` | 启用 `sandbox_lifecycle_history` 与 `/api/history/*` |
| `BFF_HISTORY_DATABASE_URL` | 空 | 与 Server `[store.postgresql].dsn` **同库**；见 [07-沙箱历史持久化.md](./07-沙箱历史持久化.md) |
| `BFF_HISTORY_RECONCILE_ON_READ` | `true` | 打开历史 API 时是否 Lifecycle 全量对账；Server `[store.lifecycle_audit]` 开启后建议 `false` |

集群内 BFF 需 [k8s/console-bff-rbac.example.yaml](../k8s/console-bff-rbac.example.yaml) 才能 list Pod。归档日志要求 node-agent **sink.type=oss**（file sink 数据在节点 hostPath，BFF 无法统一读）。

## tenants.toml 来源（与 Server 同源）

1. 维护 ConfigMap `opensandbox-tenants` 中 `data.tenants.toml`（结构见 [k8s/tenants-configmap.example.yaml](../k8s/tenants-configmap.example.yaml)）。
2. 集群内：
   - **Server**：ConfigMap `opensandbox-tenants` 挂到 Server Pod（现有做法）。
   - **BFF**：任选其一  
     - 同一 ConfigMap 挂 `subPath: tenants.toml`；或  
     - 单独 Secret `opensandbox-console-tenants`（内容相同，便于 RBAC 仅 BFF 可读）。

⚠️ BFF **不要**从 YAML ConfigMap 文件路径读（本地开发可手写 `tenants.toml`）；`TENANTS_TOML_PATH` 始终指向 **`.toml` 文件**。

## Secret 示例（结构）

见 [k8s/console-bff-secret.example.yaml](../k8s/console-bff-secret.example.yaml)。

- `BFF_SESSION_SECRET`、`BFF_ADMIN_TOKEN`：仅 BFF Deployment。
- 租户 Key：**不要**拆成 8 个独立 env；整文件挂载 `tenants.toml` 与 Server 同步更新。

## Admin 与租户 Key 分离

| 凭据 | 存放 | 用途 |
|------|------|------|
| 各租户 `api_keys[]` | `tenants.toml`（Server + BFF 挂载） | 研发登录、BFF 代调 Lifecycle |
| `BFF_ADMIN_TOKEN` | BFF 专用 Secret | 仅 Admin 登录 BFF，**不能**调 Lifecycle（无 Key） |

Admin 看全局沙箱：BFF 用 tenants 文件里的 **各 tenant Key 在服务端** 请求 `GET /sandboxes`，合并结果。

## 本地 `.env`

复制 [bff/.env.example](../bff/.env.example) 为 `bff/.env`（勿提交 git）。

## 安全 checklist

- [ ] 租户 Key 不出现在 SPA bundle、浏览器 localStorage。
- [ ] `tenants.toml` RBAC：仅 Server SA + BFF SA 可读。
- [ ] Admin Token 轮换流程与离职吊销。
- [ ] Ingress TLS；`BFF_COOKIE_SECURE=true`。
- [ ] BFF 不对公网暴露 Admin 登录 brute-force（Ingress 限流 / 内网 VPN）。
