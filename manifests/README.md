# OpenSandbox Manifests

This directory contains the Helm chart sources for OpenSandbox. Charts here are
versioned sources: releases are umbrella releases (one `release-X.Y.Z` tag for
the whole platform; see `docs/community/release-automation.md`), and charts are
installed from a checkout of that tag. If you want to change how
OpenSandbox is deployed, this is the right place.

For the full deployment guide, see [HELM-DEPLOYMENT.md](HELM-DEPLOYMENT.md).

## Layout

```
manifests/
├── charts/
│   ├── base/             # CRDs (sandbox.opensandbox.io + sandbox.fast.io) + CRD RBAC
│   ├── controller/       # OpenSandbox controller (control plane)
│   ├── server/           # Lifecycle API server
│   ├── ingress-gateway/  # Ingress gateway (components/ingress), deployable standalone
│   ├── node-agent/       # Node-level sandbox data collector (DaemonSet)
│   ├── fast-sandbox/     # fast-sandbox Firecracker control plane + node runtime
│   └── opensandbox/      # Umbrella chart aggregating the above as dependencies
├── third-party/          # Pinned upstream sources (fast-sandbox)
├── release/              # Helm release tooling (create/publish/verify/smoke)
└── HELM-DEPLOYMENT.md    # Helm deployment guide
```

## Charts

| Chart | Purpose | Install gate |
|---|---|---|
| `base` | Cluster-scoped resources only: the three CRDs, the fast-sandbox CRDs (`sandbox.fast.io`) with their component RBAC, and admin/editor/viewer ClusterRoles. Install once per cluster, before any component. | — |
| `controller` | BatchSandbox/Pool reconciler, pooling, pause/resume snapshot orchestration. | requires `base` |
| `server` | Lifecycle REST API server; announces the ingress gateway through its `[ingress]` config. | requires `base` |
| `ingress-gateway` | Proxies sandbox traffic; can be deployed standalone and scaled independently. | optional |
| `node-agent` | Optional node-level log/data collection. | optional |
| `fast-sandbox` | fast-sandbox Firecracker runtime: all-in-one control plane (reconcilers + FastPath) and the firecracker runtime (UDS API + DART + node readiness self-labeling + janitor sidecar). | requires `base`; Firecracker-capable (KVM) nodes |
| `opensandbox` | Umbrella chart: one release installing everything, with per-component conditions. | — |

## Install

All-in-one (umbrella):

```bash
cd manifests/charts
helm dependency build opensandbox  # package sub-charts (charts/ is git-ignored)
helm install opensandbox opensandbox --namespace opensandbox-system --create-namespace
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
- The fast-sandbox CRDs (`charts/base/files/fast-sandbox-crds.yaml`) and the
  companion images are derived from the upstream source pinned in
  [`third-party/fast-sandbox.commit`](third-party/fast-sandbox.commit)
  (Git-LFS-pointer style: repo + commit). Bump the `commit` line, then run
  `manifests/release/build-fast-sandbox.sh --sync-crds` (or `make helm-gen-fast-sandbox-crds`
  from `manifests/` for the CRDs alone) so the derived files match the pin.
  Do not edit `files/fast-sandbox-crds.yaml` by hand.
- The umbrella chart's `Chart.lock` must stay in sync with its `Chart.yaml`
  dependencies; run `helm dependency update charts/opensandbox` after changing
  dependency versions.
