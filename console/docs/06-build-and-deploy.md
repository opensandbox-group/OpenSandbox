# Build and deploy

Source lives under [`console/`](../). Deploy **in the same cluster** as the lifecycle server; prefer in-cluster Lifecycle Service URL.

Commands assume **repository root**. Kubernetes examples: [`console/k8s/`](../k8s/README.md).

## Prerequisites

- OpenSandbox server running (Helm `manifests/charts/opensandbox` or equivalent) with `opensandbox-server` Service.
- ConfigMap **`opensandbox-tenants`** with the same `tenants.toml` as the server ([02-bff-config-and-secrets.md](./02-bff-config-and-secrets.md)).
- Container registry (e.g. `YOUR_REGISTRY.example.com`).

---

## 1. Build images (linux/amd64)

Production nodes are usually **amd64**. On Apple Silicon, use **`--platform linux/amd64`** to avoid `exec format error`.

### buildx and Docker Hub

If `docker pull` works but `buildx build` times out, you may be on a **`docker-container`** builder that bypasses local registry mirrors. Prefer **`docker buildx use default`**.

```bash
export REGISTRY=YOUR_REGISTRY.example.com
export TAG=0.1.0
export PLATFORM=linux/amd64

docker buildx use default
export DOCKER_BUILDKIT=1
docker login "${REGISTRY}"
```

### Base image `--build-arg`

Defaults: `python:3.12-slim`, `node:22-alpine`, `nginx:1.27-alpine`. Mirror to private registry if needed:

```bash
export BASE=YOUR_REGISTRY.example.com/mirror
export PYTHON_IMAGE="${BASE}/python:3.12-slim"
export NODE_IMAGE="${BASE}/node:22-alpine"
export NGINX_IMAGE="${BASE}/nginx:1.27-alpine"
```

**BFF**

```bash
docker buildx build --platform "${PLATFORM}" \
  --build-arg PYTHON_IMAGE="${PYTHON_IMAGE:-python:3.12-slim}" \
  -t "${REGISTRY}/opensandbox-console-bff:${TAG}" \
  -f console/bff/Dockerfile \
  --push \
  console/bff
```

**Web** (check [`console/web/nginx.conf`](../web/nginx.conf) BFF upstream name)

```bash
docker buildx build --platform "${PLATFORM}" \
  --build-arg NODE_IMAGE="${NODE_IMAGE:-node:22-alpine}" \
  --build-arg NGINX_IMAGE="${NGINX_IMAGE:-nginx:1.27-alpine}" \
  -t "${REGISTRY}/opensandbox-console-web:${TAG}" \
  -f console/web/Dockerfile \
  --push \
  console/web
```

Local web check only:

```bash
cd console/web && npm ci && npm run build
```

---

## 2. Secrets and RBAC

```bash
kubectl apply -f console/k8s/console-bff-secret.example.yaml
kubectl apply -f console/k8s/console-bff-rbac.example.yaml
```

Secret must include `BFF_SESSION_SECRET` and `BFF_ADMIN_TOKEN`.

---

## 3. Deploy BFF

Edit [console-bff-deployment.example.yaml](../k8s/console-bff-deployment.example.yaml): image, `LIFECYCLE_API_BASE`, `BFF_CORS_ORIGINS`, `BFF_K8S_*_DEPLOYMENT`.

```bash
kubectl apply -f console/k8s/console-bff-deployment.example.yaml
kubectl rollout status deployment/opensandbox-console-bff -n opensandbox-system
```

---

## 4. Deploy Web

```bash
kubectl apply -f console/k8s/console-web-deployment.example.yaml
kubectl apply -f console/k8s/console-ingress.example.yaml
```

Visit `https://console.example.com/login` (tenant key or admin token).

---

## 5. Optional features

| Feature | Config |
|---------|--------|
| K8s workloads / probes | `BFF_K8S_PROBE_ENABLED=true` + RBAC |
| Archive logs | node-agent OSS + `BFF_NODEAGENT_ARCHIVE_*` |

---

## Helm relationship

Not yet part of the umbrella `opensandbox` chart; shipped as standalone Deployments.

## Troubleshooting

| Symptom | Check |
|---------|--------|
| Login 401 | Same `tenants.toml` as server; valid key |
| Admin login fails | `BFF_ADMIN_TOKEN` in Secret |
| `/api` 502 | nginx → BFF Service; pods Ready |
| Health all disabled | `BFF_K8S_PROBE_ENABLED`; RBAC |
| TLS verify to apiserver | Fix CA or temporary `BFF_K8S_INSECURE_SKIP_TLS_VERIFY` |
| Archive empty | OSS sink required |
| buildx Hub timeout | `docker buildx use default` or private base images |

Feature status: [05-feature-matrix.md](./05-feature-matrix.md).
