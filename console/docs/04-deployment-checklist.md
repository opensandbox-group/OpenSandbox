# Deployment checklist

## 1. Prepare `tenants.toml`

Same as the lifecycle server. In-cluster: ConfigMap **`opensandbox-tenants`**. Locally:

```bash
chmod +x console/scripts/extract-tenants-toml.sh
console/scripts/extract-tenants-toml.sh
# reads console/k8s/tenants-configmap.example.yaml by default
# writes console/bff/tenants.local.toml (gitignored; no real keys in git)
```

Custom ConfigMap path: `console/scripts/extract-tenants-toml.sh /path/to/configmap.yaml`

## 2. Build images

See [06-build-and-deploy.md](./06-build-and-deploy.md). BFF Dockerfile: [../bff/Dockerfile](../bff/Dockerfile).

## 3. Apply Kubernetes manifests

1. `kubectl apply -f console/k8s/console-bff-secret.example.yaml` (replace placeholders)
2. Ensure ConfigMap `opensandbox-tenants` exists (shared with server)
3. `kubectl apply -f console/k8s/console-bff-rbac.example.yaml` (platform pages / probes)
4. `kubectl apply -f console/k8s/console-bff-deployment.example.yaml`
5. `kubectl apply -f console/k8s/console-web-deployment.example.yaml`
6. `kubectl apply -f console/k8s/console-ingress.example.yaml` (set `host`)

See [k8s/README.md](../k8s/README.md).

## 4. Verify

```bash
curl -s http://localhost:8091/health

curl -s -c /tmp/cj -X POST http://localhost:8091/api/auth/admin \
  -H 'Content-Type: application/json' \
  -d '{"adminToken":"YOUR_ADMIN_TOKEN"}'

curl -s -b /tmp/cj http://localhost:8091/api/admin/stats/runtime
```

## 5. Optional follow-ups

- Helm subchart under `manifests/charts/` (not bundled yet)
