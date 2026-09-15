# OpenSandbox Manifests

This directory contains the Helm chart sources for OpenSandbox. Charts here are
versioned sources: they are packaged
and published to the Helm repository by CI (see
`.github/workflows/publish-helm-chart.yml`). If you want to change how
OpenSandbox is deployed, this is the right place.

For the full deployment guide, see [HELM-DEPLOYMENT.md](HELM-DEPLOYMENT.md).

## Layout

```
manifests/
├── charts/
│   ├── base/             # CRDs (sandbox.opensandbox.io) + user-facing CRD RBAC
│   ├── controller/       # OpenSandbox controller (control plane)
│   ├── server/           # Lifecycle API server
│   ├── ingress-gateway/  # Ingress gateway (components/ingress), deployable standalone
│   ├── node-agent/       # Node-level sandbox data collector (DaemonSet)
│   └── opensandbox/      # Umbrella chart aggregating the above as dependencies
├── release/              # Helm release tooling (create/publish/verify/smoke)
└── HELM-DEPLOYMENT.md    # Helm deployment guide
```

## Charts

| Chart | Purpose | Install gate |
|---|---|---|
| `base` | Cluster-scoped resources only: the three CRDs and admin/editor/viewer ClusterRoles. Install once per cluster, before any component. | — |
| `controller` | BatchSandbox/Pool reconciler, pooling, pause/resume snapshot orchestration. | requires `base` |
| `server` | Lifecycle REST API server; announces the ingress gateway through its `[ingress]` config. | requires `base` |
| `ingress-gateway` | Proxies sandbox traffic; can be deployed standalone and scaled independently. | optional |
| `node-agent` | Optional node-level log/data collection. | optional |
| `opensandbox` | Umbrella chart: one release installing everything, with per-component conditions. | — |

## Install

All-in-one (umbrella):

```bash
helm dependency build manifests/charts/opensandbox  # package sub-charts (not committed)
helm install opensandbox manifests/charts/opensandbox --namespace opensandbox-system --create-namespace
```

Per-component (two releases for the minimal stack):

```bash
helm install base manifests/charts/base
helm install opensandbox-controller manifests/charts/controller \
  --namespace opensandbox-system --create-namespace
```

See [HELM-DEPLOYMENT.md](HELM-DEPLOYMENT.md) for values, upgrades, and the
ingress-gateway/node-agent setup.

## Source of truth

- CRD YAML is generated from `kubernetes/apis/sandbox/v1alpha1` by
  controller-gen into `kubernetes/config/crd/bases`, then synced into
  `charts/base/files/crds.yaml` by `make helm-gen-crds` (run automatically by
  `make manifests` from `kubernetes/`). Do not edit `files/crds.yaml` by hand.
- The umbrella chart's `Chart.lock` must stay in sync with its `Chart.yaml`
  dependencies; run `helm dependency update charts/opensandbox` after changing
  dependency versions.
