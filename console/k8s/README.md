# Console Kubernetes 清单

与 OpenSandbox Server 同集群、同 **`opensandbox-system`** namespace（可按环境改）。

## 文件说明

| 文件 | 用途 |
|------|------|
| `console-bff-secret.example.yaml` | `BFF_SESSION_SECRET`、`BFF_ADMIN_TOKEN` |
| `console-bff-rbac.example.yaml` | BFF ServiceAccount + 只读 ClusterRole（K8s 页 / 探针） |
| `tenants-configmap.example.yaml` | `opensandbox-tenants` 结构示例（生产与 Server 共用真实 ConfigMap） |
| `console-bff-deployment.example.yaml` | BFF Deployment + Service |
| `console-web-deployment.example.yaml` | Web Deployment + Service |
| `console-ingress.example.yaml` | Ingress 示例 |

完整构建镜像说明见 [../docs/06-构建与部署.md](../docs/06-构建与部署.md)。

## 部署顺序

```bash
cd console

kubectl apply -f k8s/console-bff-secret.example.yaml
kubectl apply -f k8s/console-bff-rbac.example.yaml
kubectl apply -f k8s/console-bff-deployment.example.yaml
kubectl apply -f k8s/console-web-deployment.example.yaml
kubectl apply -f k8s/console-ingress.example.yaml
```

## 部署前必改

- `console-bff-deployment.example.yaml` / `console-web-deployment.example.yaml`：`YOUR_REGISTRY/...` 镜像 tag
- `console-bff-deployment.example.yaml`：`LIFECYCLE_API_BASE`、`BFF_CORS_ORIGINS`、`BFF_K8S_*_DEPLOYMENT`
- `console-ingress.example.yaml`：`host`
- `console/web/nginx.conf`：BFF Service DNS 与 namespace

## 验证

```bash
kubectl rollout status deployment/opensandbox-console-bff -n opensandbox-system
kubectl rollout status deployment/opensandbox-console-web -n opensandbox-system
kubectl port-forward -n opensandbox-system svc/opensandbox-console-bff 8091:8091
curl -s http://127.0.0.1:8091/health
```
