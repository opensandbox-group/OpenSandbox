# BFF configuration and secrets

## Required environment variables

| Variable | Required | Description |
|----------|----------|-------------|
| `LIFECYCLE_API_BASE` | yes | Lifecycle root including `/v1`, e.g. `http://opensandbox-server.opensandbox-system.svc:8090/v1` |
| `TENANTS_TOML_PATH` | yes | Path to **`tenants.toml`** (same content as server mount) |
| `BFF_SESSION_SECRET` | yes | Session cookie signing secret (≥32 random chars) |
| `BFF_ADMIN_TOKEN` | yes | Admin login password; **never** in frontend build |
| `BFF_COOKIE_SECURE` | no | Set `true` behind HTTPS |
| `BFF_CORS_ORIGINS` | no | Comma-separated SPA origins; `*` dev only |
| `BFF_AGGREGATE_CACHE_SECONDS` | no | Admin list cache; default `0` |
| `BFF_HTTP_TIMEOUT_SECONDS` | no | Upstream timeout; default `30` |

### Optional: node-agent integration

| Variable | Default | Description |
|----------|---------|-------------|
| `BFF_K8S_PROBE_ENABLED` | `false` | Platform probes, node-agent, workloads/events APIs |
| `BFF_K8S_INSECURE_SKIP_TLS_VERIFY` | `false` | Skip Kubernetes API TLS verify (temporary; independent of server `[kubernetes] insecure_skip_tls_verify`) |
| `BFF_K8S_SYSTEM_NAMESPACE` | `opensandbox-system` | Controller/ingress namespace |
| `BFF_K8S_CONTROLLER_DEPLOYMENT` | `opensandbox-controller-manager` | Match `kubectl get deploy -n opensandbox-system` |
| `BFF_K8S_INGRESS_DEPLOYMENT` | `opensandbox-ingress-gateway` | Same |
| `BFF_K8S_NODE_AGENT_NAMESPACE` | `opensandbox-system` | DaemonSet namespace |
| `BFF_K8S_NODE_AGENT_LABEL_SELECTOR` | `app.kubernetes.io/component=node-agent` | Pod selector |
| `BFF_K8S_NODE_AGENT_PROBE_PORT` | `8080` | node-agent health port |
| `BFF_NODEAGENT_ARCHIVE_ENABLED` | `false` | Enable `GET .../logs/archive` |
| `BFF_NODEAGENT_CLUSTER_ID` | `dev-cluster` | Align with node-agent Helm `clusterID` |
| `BFF_NODEAGENT_OSS_*` | empty | Read-only OSS credentials (same key layout as node-agent writer) |
| `BFF_NODEAGENT_ARCHIVE_MAX_BYTES` | `524288` | Max bytes per archive response |

In-cluster BFF needs [console-bff-rbac.example.yaml](../k8s/console-bff-rbac.example.yaml) to list pods. Archive logs require node-agent **OSS sink** (file sink on hostPath is not readable cluster-wide).

Future optional features (history PostgreSQL, Grafana monitor) are documented in [07-sandbox-history.md](./07-sandbox-history.md) and tracked on branch `feat/console-extended-scope`, not in the MVP PR.

## `tenants.toml` source

1. Maintain ConfigMap `opensandbox-tenants` `data.tenants.toml` (see [tenants-configmap.example.yaml](../k8s/tenants-configmap.example.yaml)).
2. Mount the same file on server and BFF pods (`subPath: tenants.toml`).

`TENANTS_TOML_PATH` must point to a **`.toml` file**, not a Kubernetes YAML file.

## Secret structure

See [console-bff-secret.example.yaml](../k8s/console-bff-secret.example.yaml): `BFF_SESSION_SECRET`, `BFF_ADMIN_TOKEN`. Tenant keys live only in mounted `tenants.toml`.

## Credentials separation

| Credential | Storage | Use |
|------------|---------|-----|
| Tenant `api_keys[]` | `tenants.toml` | Developer login; BFF → Lifecycle |
| `BFF_ADMIN_TOKEN` | BFF Secret | Admin BFF login only |

## Local `.env`

Copy [bff/.env.example](../bff/.env.example) to `bff/.env` (gitignored).

## Security checklist

- [ ] Tenant keys not in SPA bundle or `localStorage`
- [ ] RBAC: only server + BFF service accounts read `tenants.toml`
- [ ] Admin token rotation process
- [ ] Ingress TLS; `BFF_COOKIE_SECURE=true`
- [ ] Rate-limit or restrict admin login exposure
