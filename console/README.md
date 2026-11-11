# OpenSandbox Developer Console

Standalone **React SPA + FastAPI BFF** for sandbox operations. Aligns with [OSEP-0006](../../oseps/0006-developer-console.md) as a **BFF adjunct** (httpOnly session; API keys stay on the server). The **upstream MVP** targets Lifecycle proxy + UI only; PostgreSQL history and server-side audit are deferred ([07-sandbox-history.md](./docs/07-sandbox-history.md)).

Long-form docs for the published site: [Developer Console guide](../docs/guides/developer-console.md).

## Layout

| Path | Description |
|------|-------------|
| [docs/00-design-principles.md](./docs/00-design-principles.md) | Scope, boundaries, phasing |
| [docs/01-architecture.md](./docs/01-architecture.md) | Tenant vs admin flows |
| [docs/02-bff-config-and-secrets.md](./docs/02-bff-config-and-secrets.md) | Env vars, secrets, `tenants.toml` |
| [docs/03-bff-api.md](./docs/03-bff-api.md) | BFF HTTP API for the SPA |
| [docs/05-feature-matrix.md](./docs/05-feature-matrix.md) | Implemented vs optional features |
| [docs/06-build-and-deploy.md](./docs/06-build-and-deploy.md) | Images (linux/amd64) and Kubernetes |
| [docs/07-sandbox-history.md](./docs/07-sandbox-history.md) | Deferred history / usage design |
| [bff/](./bff/) | FastAPI BFF |
| [web/](./web/) | Vite + React + Ant Design |
| [k8s/](./k8s/) | Example manifests (adjust for your cluster) |

## Run BFF locally

```bash
cd console/bff
python -m venv .venv && source .venv/bin/activate
pip install -r requirements.txt
export LIFECYCLE_API_BASE=http://127.0.0.1:8090/v1
../scripts/extract-tenants-toml.sh
export TENANTS_TOML_PATH=$(pwd)/tenants.local.toml
export BFF_SESSION_SECRET=dev-change-me-use-at-least-32-chars
export BFF_ADMIN_TOKEN=dev-admin-token
uvicorn app.main:app --reload --port 8091
```

For production, mount the same `tenants.toml` as the lifecycle server. See [02-bff-config-and-secrets.md](./docs/02-bff-config-and-secrets.md).

## Run Web locally

```bash
# With BFF running:
cd console/web
npm install
npm run dev
```

Open the Vite URL (default `http://localhost:5173`) and sign in at `/login` (tenant API key or admin token). See [web/README.md](./web/README.md).

## Deployment notes

- Multi-tenant: ConfigMap **`opensandbox-tenants`** (same `tenants.toml` as the server). Example shape: [k8s/tenants-configmap.example.yaml](./k8s/tenants-configmap.example.yaml).
- Lifecycle: in-cluster `http://opensandbox-server.<namespace>.svc:8090/v1`, or HTTPS via ingress.
- **Pool API** is platform-scoped, not tenant-isolated (documented in UI and tenant config comments).
