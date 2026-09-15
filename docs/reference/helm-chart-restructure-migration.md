---
title: Helm Chart Restructure Migration
description: Migration notes for the charts moving under manifests/charts, the base chart split, and the standalone ingress-gateway chart.
---

# Helm Chart Restructure Migration Guide

Feature: [#1857](https://github.com/opensandbox-group/OpenSandbox/pull/1857)

All Helm charts moved under `manifests/charts/` and were split by component:

| Chart | Directory | Notes |
|---|---|---|
| base (new) | `manifests/charts/base` | CRDs + user-facing CRD RBAC, cluster-scoped |
| controller | `manifests/charts/controller` | was `kubernetes/charts/opensandbox-controller` |
| server | `manifests/charts/server` | was `kubernetes/charts/opensandbox-server` |
| ingress-gateway (new) | `manifests/charts/ingress-gateway` | extracted from the server chart |
| node-agent | `manifests/charts/node-agent` | was `kubernetes/charts/opensandbox-node-agent` |
| opensandbox (umbrella) | `manifests/charts/opensandbox` | aggregates the above |

## ⚠️ Upgrade risks — read first

### 1. Standalone controller upgrades with `crds.keep=false` delete every custom resource

The CRDs were moved out of the controller chart. On `helm upgrade` of an
existing controller release, Helm's three-way merge sees the CRDs disappear
from the release manifest and deletes them — and Kubernetes cascade-deletes
**all** `BatchSandbox`, `Pool`, and `SandboxSnapshot` objects.

- Default installs are safe: the old chart's default `crds.keep=true` puts
  `helm.sh/resource-policy: keep` on the CRDs, and Helm refuses to delete
  resources carrying that annotation.
- **If you explicitly set `crds.keep=false`, do not `helm upgrade` in place.**
  Instead: install `base` first, then upgrade; or verify the CRDs carry the
  `helm.sh/resource-policy: keep` annotation before upgrading
  (`kubectl get crd batchsandboxes.sandbox.opensandbox.io -o jsonpath='{.metadata.annotations}'`).

### 2. Umbrella users: server gateway deployment values are silently ignored

The gateway deployment moved from the server chart to the new
`ingress-gateway` chart. Umbrella values under
`opensandbox-server.server.gateway.*` that only affected the gateway
workload (`image`, `replicaCount`, `port`, `dataplaneNamespace`,
`providerType`, `logLevel`, `env`, `resources`, scheduling/security keys)
are no longer consumed by any template. After upgrading, the gateway either
stays off (previous default) or runs with the `ingress-gateway` chart's
defaults — your overrides are **not** carried over.

Move those overrides to the `ingress-gateway.*` (umbrella) or
`gateway.*` (standalone chart) values before upgrading. Only
`enabled`, `host`, `gatewayRouteMode`, and `secureAccess` remain on
`opensandbox-server.server.gateway.*` (they render the server's
`[ingress]` config announcement).

## Install flow changes

- The controller chart no longer installs CRDs. Standalone installs need the
  base chart first:

  ```bash
  helm install base manifests/charts/base
  helm install opensandbox-controller manifests/charts/controller \
    --namespace opensandbox-system --create-namespace
  ```

- Umbrella installs are unchanged: `base` is a default-enabled dependency, so
  `helm install opensandbox ...` still provisions CRDs in one release.

## Release ordering

The docs reference `helm/base/<version>/base-<version>.tgz` release assets.
Publish the first `helm/base` tag right after this change merges; until then
those links 404 and standalone installs should use the chart sources.

## Unchanged

- Chart names, resource names, selector labels, and the server config-side
  `server.gateway.{enabled,host,gatewayRouteMode,secureAccess}` keys are
  preserved, so upgrades are rename-free.
- The umbrella chart still installs everything in a single release.
