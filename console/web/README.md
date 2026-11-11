# OpenSandbox Console Web

React + Vite + Ant Design SPA. Talks to the BFF at `/api` with cookie sessions; **no Lifecycle API key in the browser**.

## Development

Terminal 1 — BFF ([../README.md](../README.md)):

```bash
cd ../bff && source .venv/bin/activate
uvicorn app.main:app --reload --port 8091
```

Terminal 2 — Web:

```bash
npm install
npm run dev
```

Open the Vite URL (default `http://localhost:5173`). Sign in at `/login` (tenant API key or admin token).

## Build

```bash
npm run build
```

Output in `dist/`; serve via nginx or ingress (same origin as BFF recommended).

## Routes (summary)

| Path | Notes |
|------|--------|
| `/` | Overview KPIs |
| `/sandboxes`, `/sandboxes/new`, `/sandboxes/:id` | Tenant lifecycle |
| `/admin/sandboxes` | Admin global list |
| `/snapshots`, `/pools`, `/diagnostics` | Admin / ops |
| `/platform/*` | Health, version, K8s (when RBAC enabled) |

Full matrix: [../docs/05-feature-matrix.md](../docs/05-feature-matrix.md).
