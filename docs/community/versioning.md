---
title: Versioning and Releases
description: Unified umbrella versioning for OpenSandbox — one version shared by every image, chart, CLI, and SDK.
---

# Versioning and Releases

OpenSandbox ships as a **unified umbrella release**: every artifact of a release — server and component images, Kubernetes controller and task-executor images, Helm chart, CLI, and all SDKs — carries the **same `X.Y.Z`**, cut from a single release commit and pinned in a signed BOM. Full design: [OSEP-0016](https://github.com/opensandbox-group/OpenSandbox/blob/main/oseps/0016-unified-umbrella-release-governance.md).

::: warning Rollout status
Umbrella release tooling is being rolled out in phases (dry-run → rc → GA, see the OSEP's Migration section). Until GA, per-component tags remain the operative release mechanism ([Release Automation](/community/release-automation)). Historical per-component tags are frozen at that point — never deleted, never extended — and keep resolving forever.
:::

## First version: `1.1.0`

The first umbrella release is **`release-1.1.0`** and doubles as the GA declaration. `1.0.0` is deliberately skipped: Maven Central (`com.alibaba.opensandbox:sandbox` up to `1.0.19`) and the Go module proxy (`sdks/sandbox/go` up to `v1.0.5`) already consumed those versions, and both registries are immutable. The umbrella starts at the lowest new **line** (`X.Y.0`) above every already-consumed version — `1.0.20` would clear the registries but is not a valid line birth (`Z > 0` is reserved for in-line snapshots), so `1.1.0` it is.

## Naming rules

| Artifact | Format (example at `1.4.0`) |
|---|---|
| Git tag | `release-1.4.0` |
| Container images | `opensandbox/{server,execd,ingress,egress,image-committer,controller,task-executor}:release-1.4.0` |
| Go SDK (VCS tags) | `sdks/sandbox/go/v1.4.0`, `sdks/sandbox/go/poolredis/v1.4.0` — same commit as the umbrella tag |
| CLI / SDKs / server on PyPI | bare `1.4.0` |

Git and image tags share the same `release-` string: `git checkout release-1.4.0` and the image you pull are the same release. Package registries use the bare semver core because they reject prefixes.

## Scope

- **Covered**: platform runtime images (server, execd, ingress, egress, image-committer, controller, task-executor; the fast-sandbox family ships as `opensandbox/fsb-*`), CLI, and all published SDKs (Python, JavaScript, Kotlin/JVM, .NET, Go).
- **Helm charts are not published.** Charts live in-repo and are versioned at the release tag; render and deploy yourself:

  ```
  git checkout release-1.1.0
  helm template ./manifests/charts/opensandbox | kubectl apply -f -
  ```

  GitOps platforms can point directly at the repo path and tag.
- **Not covered**: sandbox template images such as `opensandbox/code-interpreter`. They are chosen by the user at sandbox-creation time and version independently in [opensandbox-group/sandbox-images](https://github.com/opensandbox-group/sandbox-images).

## Cadence and support

- A line is born every 2 weeks (`X.Y.0`); in-line snapshots (`X.Y.Z`, `Z > 0`) ship on demand. Pre-releases look like `X.Y.0-rc.N` and publish images only — packages are held until the line's stable release.
- **Latest line only, no LTS.** When `X.(Y+1).0` ships, `X.Y.*` is EOL except for a single emergency-CVE window (CVSS ≥ 8.0, ≤ 72h from disclosure, one-shot `X.(Y-1).Z` snapshot).
- There are no per-component hotfixes: a backport is a full umbrella rebuild at the new `X.Y.Z`.

To verify a release you installed, see [Release Verification](/community/release-verification).
