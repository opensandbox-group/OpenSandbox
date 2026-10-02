# OpenSandbox AGS 控制台（外挂方案）

独立于 OpenSandbox 主仓库 **server/specs/kubernetes** 源码的控制台与 BFF。不依赖 [PR #835](https://github.com/opensandbox-group/OpenSandbox/pull/835) 合并。

## 目录

| 路径 | 说明 |
|------|------|
| [docs/00-设计原则.md](./docs/00-设计原则.md) | 吸取 #835 教训、与 upstream 边界 |
| [docs/01-架构.md](./docs/01-架构.md) | 租户 Key / Admin 双轨、数据流 |
| [docs/02-BFF配置与Secret.md](./docs/02-BFF配置与Secret.md) | 环境变量、Secret、与 `tenants.toml` 同源 |
| [docs/03-API契约.md](./docs/03-API契约.md) | BFF 对前端的 HTTP 接口 |
| [docs/05-功能清单.md](./docs/05-功能清单.md) | 一期 / 二期实现状态 |
| [docs/06-构建与部署.md](./docs/06-构建与部署.md) | 镜像构建（linux/amd64）与 K8s 部署 |
| [bff/](./bff/) | Python FastAPI BFF（可本地跑） |
| [web/](./web/) | React + Ant Design SPA（`npm run dev` / `npm run build`） |
| [k8s/](./k8s/) | 部署示例（Secret / Deployment / Ingress，需按环境改） |

## 快速本地跑 BFF

```bash
cd console/bff
python -m venv .venv && source .venv/bin/activate
pip install -r requirements.txt
export LIFECYCLE_API_BASE=http://127.0.0.1:8090/v1
# 先从 ConfigMap 抽出 tenants.local.toml（含真实 Key，已 gitignore）
../scripts/extract-tenants-toml.sh
export TENANTS_TOML_PATH=$(pwd)/tenants.local.toml
export BFF_SESSION_SECRET=dev-change-me
export BFF_ADMIN_TOKEN=dev-admin-token
uvicorn app.main:app --reload --port 8091
```

生产环境请使用挂载的 `tenants.toml` 路径，见 [docs/02-BFF配置与Secret.md](./docs/02-BFF配置与Secret.md)。

## 快速本地跑前端

```bash
# 先启动 BFF（上节），再：
cd console/web
npm install
npm run dev
```

登录页：`/login`（租户 Key / Admin Token）。详见 [web/README.md](./web/README.md)。

## 与现有部署的关系

- 多租户：ConfigMap **`opensandbox-tenants`**（`tenants.toml` 与 Server 同源），结构见 [k8s/tenants-configmap.example.yaml](./k8s/tenants-configmap.example.yaml)。
- Lifecycle API：集群内 `http://opensandbox-server.<namespace>.svc:8090/v1`，或经 Ingress 的 HTTPS 入口。
- **Pool API 不按租户隔离**：控制台 Pool 页仅 Admin / 平台视角，见 tenants 注释。
