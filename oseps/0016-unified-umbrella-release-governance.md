---
title: Unified Umbrella Release Governance
authors:
  - "@Pangjiping"
creation-date: 2026-07-21
last-updated: 2026-07-21
status: draft
---

# OSEP-0016: Unified Umbrella Release Governance

<!-- toc -->
- [Summary](#summary)
- [Motivation](#motivation)
- [Goals](#goals)
- [Non-Goals](#non-goals)
- [Proposal](#proposal)
  - [Unified Versioning](#unified-versioning)
  - [Naming Rules](#naming-rules)
  - [Starting Version: `1.0.0` as GA](#starting-version-100-as-ga)
  - [Cadence and Support](#cadence-and-support)
- [Design Details](#design-details)
  - [Bill of Materials (BOM)](#bill-of-materials-bom)
  - [Release Workflow](#release-workflow)
  - [Compatibility](#compatibility)
  - [User-facing Surface](#user-facing-surface)
- [Test Plan](#test-plan)
- [Drawbacks](#drawbacks)
- [Alternatives](#alternatives)
- [Migration](#migration)
<!-- /toc -->

## Summary

OpenSandbox today ships 19+ independently versioned targets (server,
four component images, two Kubernetes controllers, Helm chart, CLI,
five language SDKs). There is no single "OpenSandbox version" a user
can install, audit, or roll back to.

This OSEP unifies release governance: every artifact of a given
umbrella release carries the **same** `X.Y.Z`, cut from a single
release commit, driven by a single fan-out workflow, and pinned in
an immutable BOM. The first umbrella `opensandbox/1.0.0` is the GA
declaration of OpenSandbox. Cadence is one line every two weeks;
only the most recent line is supported.

## Motivation

Current latest tags: `server/v0.2.1`, `docker/execd/v1.0.21`,
`docker/ingress/v1.0.10`, `docker/egress/v1.1.4`, `java/sandbox/v1.0.16`,
`python/sandbox/v0.1.14`, `js/sandbox/v0.1.10`, `sdks/sandbox/go/v1.0.4`,
`csharp/sandbox/v0.1.4`. These numbers reflect each component's
private iteration count and encode no relationship between components.

Concrete pain points:

1. **No platform version.** Security advisories, docs, and installers
   must enumerate ~10 tags.
2. **Unverified combinations.** No assertion that any specific
   `{server, execd, ingress, …}` set has been e2e-tested together.
3. **Opaque client compatibility.** Users cannot answer "does
   `python/sandbox v0.1.14` work with `server v0.2.1`?" from release
   notes alone.
4. **Drift.** Docs, Helm defaults, and examples reference components
   individually and slip out of sync between releases.
5. **Release-note fragmentation.** 6–10 GitHub Releases per month
   with no single "what changed" entry point.

## Goals

- One version stamped onto every artifact of every umbrella release.
- One canonical git tag per release, backed by a signed BOM.
- Fan-out from a single trigger; partial releases are impossible.
- Public 2-week cadence, no LTS, latest-only support.
- Start the umbrella at `1.0.0` and treat that as GA.

## Non-Goals

- Restructuring the repository (no split repos, no submodule changes,
  no package-layout changes).
- Defining the API/CRD/spec stability contract that GA implies —
  deferred to a follow-up OSEP.
- Preserving per-component tags for future hotfixes. After the first
  umbrella, hotfixes ship only as new umbrella snapshots.
- Redesigning `scripts/release/create-release.sh`. Its per-target
  build logic is reused; only the driver changes.

## Proposal

### Unified Versioning

Every OpenSandbox release publishes every artifact at the same
semver core `X.Y.Z`, from the same commit. Concrete example at
umbrella `1.4.0`:

| Artifact | Identity |
|---|---|
| Server / component / K8s images | `opensandbox/{server,execd,ingress,egress,image-committer,controller,task-executor}:release-1.4.0` |
| Server PyPI distribution | `opensandbox-server==1.4.0` (for `uvx opensandbox-server` / `pip install opensandbox-server`) |
| Helm chart | `opensandbox-1.4.0.tgz`, `appVersion: 1.4.0` |
| CLI | `opensandbox-cli==1.4.0` |
| Python SDKs | `opensandbox==1.4.0`, `opensandbox-code-interpreter==1.4.0`, `opensandbox-mcp==1.4.0` |
| JS SDKs | `@alibaba-group/opensandbox@1.4.0`, `@alibaba-group/opensandbox-code-interpreter@1.4.0` |
| Kotlin/JVM (all Maven coordinates published by the release) | `com.alibaba.opensandbox:sandbox-bom:1.4.0` (dependency BOM), `com.alibaba.opensandbox:sandbox:1.4.0`, `com.alibaba.opensandbox:sandbox-api:1.4.0`, `com.alibaba.opensandbox:sandbox-pool-redis:1.4.0`, `com.alibaba.opensandbox:code-interpreter:1.4.0` |
| .NET | `Alibaba.OpenSandbox 1.4.0` |
| Go | `sdks/sandbox/go v1.4.0` |

Package identities are the ones the repository already publishes
today; this OSEP does **not** propose renaming any package. Only
the version string changes to the unified `X.Y.Z`.

**Scope of the umbrella.** The umbrella covers only the **platform
runtime** and its **client libraries**:

- Platform runtime images: server, execd, ingress, egress,
  image-committer, K8s controller, K8s task-executor.
- Client-facing artifacts: Helm chart, CLI, all published SDKs
  (Python, JavaScript, Kotlin/JVM, .NET, Go — each for the
  products that ship SDKs).

**Sandbox template images are explicitly excluded.** Images under
`sandboxes/` (currently only `sandboxes/code-interpreter`) provide
the runtime environment *inside* a user's sandbox and are chosen
by the user at sandbox-creation time. They version independently
of the umbrella: a user may run umbrella `1.4.0` with a
`code-interpreter` image tagged `v1.0.2` or `v1.1.0`, or vice
versa. Compatibility between platform and sandbox template images
is governed by the sandbox lifecycle API, not by umbrella
versioning. The `code-interpreter` **SDK library**
(`opensandbox-code-interpreter` etc.) is a client for that API
and *is* part of the umbrella; it is not the same thing as the
`sandboxes/code-interpreter` image.

**No component versions exist separately.** Any change that ships
triggers a new umbrella snapshot; the component participates by
being rebuilt at the new umbrella version, not by cutting an
independent version. Every umbrella release **rebuilds every image**
(fresh digest, fresh push), even for components without source
changes — this is the invariant that makes umbrella `X.Y.Z` a
byte-exact fingerprint of the whole platform.

### Naming Rules

Boolean rules that keep the eras mechanically distinguishable:

1. **Umbrella git tags** are v-less: `opensandbox/1.4.0`.
2. **Container image tags** carry a `release-` prefix:
   `opensandbox/execd:release-1.4.0`. Chosen because pre-umbrella
   images (`opensandbox/execd:v1.0.21`) sit in the same registry
   repositories; the prefix eliminates any collision or ambiguity
   at a glance.
3. **Package-registry artifacts** use bare `X.Y.Z`. PyPI, npm,
   Maven Central, NuGet, and Helm OCI all reject arbitrary
   prefixes and enforce semver ordering.
4. **Historical per-component tags** keep their existing `v` prefix
   in both git and image registries. Frozen at `1.0.0`, never
   deleted, never extended.
5. **Go SDK exception.** The Go SDK ships **two** modules that
   both live in repository subdirectories:
   - `sdks/sandbox/go` (`module github.com/alibaba/OpenSandbox/sdks/sandbox/go`)
   - `sdks/sandbox/go/poolredis` (`module github.com/alibaba/OpenSandbox/sdks/sandbox/go/poolredis`)

   The Go toolchain requires VCS tags for a subdirectory module
   to carry the subdirectory as prefix; without it,
   `go get github.com/alibaba/OpenSandbox/sdks/sandbox/go@v1.4.0`
   or `.../poolredis@v1.4.0` cannot resolve. Therefore, every
   umbrella release additionally mints two companion git tags:
   `sdks/sandbox/go/vX.Y.Z` and `sdks/sandbox/go/poolredis/vX.Y.Z`,
   both pointing at the same commit as `opensandbox/X.Y.Z` (i.e.
   at `C_bom`). This is a forced technical concession scoped to
   the Go module system, not a general exception to rule 4 —
   no non-Go component gets its own per-umbrella tag. If a new
   Go module is added in the future at another subdirectory, it
   must be listed here and receive the same companion-tag
   treatment.

Mapping at a glance:

```
Umbrella version                    → 1.4.0
Git tag                             → opensandbox/1.4.0
Go SDK companion tags (same commit) → sdks/sandbox/go/v1.4.0
                                      sdks/sandbox/go/poolredis/v1.4.0
Container image tag                 → release-1.4.0
Helm chart / SDK / CLI version      → 1.4.0
```

Users never write `release-` by hand: the Helm chart resolves
`image.tag` from `.Chart.AppVersion` internally.

### Starting Version: `1.0.0` as GA

The first umbrella release is `opensandbox/1.0.0`, treated as GA.

Rationale:

- The umbrella is the first version identity that spans the entire
  project; `1.0.0` therefore carries the semver meaning the project
  has never before been able to assert as a whole.
- No tag or image collision. `refs/tags/opensandbox/1.0.0` and
  `opensandbox/execd:release-1.0.0` are namespace-disjoint from
  every existing tag/image.
- `X` continues to mean "major/GA line"; `1 → 2` is reserved for
  breaking cross-component changes and needs its own OSEP.

Scope note: this OSEP binds only release-governance mechanics.
The concrete API/CRD/spec stability contract that "GA" implies is
**out of scope** and must land in a follow-up OSEP before or
alongside `1.0.0`.

### Cadence and Support

| Release type | Frequency | Support | Tag |
|---|---|---|---|
| Line birth (`X.Y.0`) | Every 2 weeks (even ISO-week Wednesdays) | Latest only; supersedes previous line on release | `opensandbox/X.Y.0` |
| In-line snapshot (`X.Y.Z`, `Z > 0`) | On demand within the current 2-week window | Same as current line | `opensandbox/X.Y.Z` |
| N-1 emergency CVE | CVSS ≥ 8.0, ≤72h from disclosure, only if no current-line fix already ships | One-off snapshot on the immediately previous line | `opensandbox/X.(Y-1).Z` |
| Pre-release | Ahead of a line birth | Not supported | `opensandbox/X.Y.0-rc.N` |

**No LTS.** At 2-week cadence, a back-port is a full umbrella
rebuild anyway; supporting a 6-month-old line costs more than
delivering the current line faster. Reintroducing LTS is deferred
to `2.0.0`.

When `X.(Y+1).0` ships, `X.Y.*` is immediately EOL for everything
except the single emergency-CVE window. Old tags and images remain
in registries forever; they simply stop receiving fixes.

## Design Details

### Bill of Materials (BOM)

Every umbrella release adds `releases/opensandbox/X.Y.Z.yaml` in
a workflow-authored commit on the release branch (see
[Release Workflow](#release-workflow) for the exact ordering).
Signed with existing Sigstore / `actions/attest` infrastructure.
Sketch:

```yaml
apiVersion: opensandbox.io/v1
kind: UmbrellaRelease
metadata:
  version: 1.4.0
  line: "1.4"
  channel: stable            # stable | rc
  releaseDate: 2026-10-15
  gitCommit: 6b1e…

compatibility:
  kubernetes: { minVersion: v1.24, maxVersion: v1.34 }
  crd:
    - { group: sandbox.opensandbox.io, versions: [v1alpha1] }

images:
  # One entry per image; every image is pinned by digest at tag
  # release-1.4.0. The same digest is pushed to docker.io, ghcr.io,
  # and the ACR mirror; mirror URIs listed separately (omitted here
  # for brevity).
  #
  # Required entries (platform runtime components only): server,
  # execd, ingress, egress, image-committer, controller,
  # task-executor.
  #
  # Sandbox template images (e.g. sandboxes/code-interpreter) are
  # NOT part of the umbrella. They are runtime-selectable by users
  # and version independently; see "Scope of the umbrella" below.
  execd:          { image: docker.io/opensandbox/execd,          tag: "release-1.4.0", digest: "sha256:…" }
  ingress:        { image: docker.io/opensandbox/ingress,        tag: "release-1.4.0", digest: "sha256:…" }
  imageCommitter: { image: docker.io/opensandbox/image-committer, tag: "release-1.4.0", digest: "sha256:…" }
  # …one entry per required image.

helm:   { chart: opensandbox, version: "1.4.0", appVersion: "1.4.0" }
server: { pypi: opensandbox-server==1.4.0 }
cli:    { pypi: opensandbox-cli==1.4.0 }
sdks:
  # Package identities are the ones the repository already publishes;
  # only the version string is unified. One entry per SDK per product.
  - { product: sandbox,          language: python,     package: "pypi:opensandbox==1.4.0" }
  - { product: code-interpreter, language: python,     package: "pypi:opensandbox-code-interpreter==1.4.0" }
  - { product: mcp/sandbox,      language: python,     package: "pypi:opensandbox-mcp==1.4.0" }
  - { product: sandbox,          language: javascript, package: "npm:@alibaba-group/opensandbox@1.4.0" }
  - { product: code-interpreter, language: javascript, package: "npm:@alibaba-group/opensandbox-code-interpreter@1.4.0" }
  # Kotlin/JVM Gradle multi-project publishes five Maven coordinates.
  # Every entry must appear in the BOM; missing any one means
  # osb devops doctor cannot verify it and rollback would not cover it.
  - { product: sandbox,          language: kotlin,     package: "maven:com.alibaba.opensandbox:sandbox:1.4.0" }
  - { product: sandbox-api,      language: kotlin,     package: "maven:com.alibaba.opensandbox:sandbox-api:1.4.0" }
  - { product: sandbox-bom,      language: kotlin,     package: "maven:com.alibaba.opensandbox:sandbox-bom:1.4.0" }
  - { product: sandbox-pool-redis, language: kotlin,   package: "maven:com.alibaba.opensandbox:sandbox-pool-redis:1.4.0" }
  - { product: code-interpreter, language: kotlin,     package: "maven:com.alibaba.opensandbox:code-interpreter:1.4.0" }
  - { product: sandbox,          language: csharp,     package: "nuget:Alibaba.OpenSandbox 1.4.0" }
  - { product: sandbox,          language: go,         package: "go:github.com/alibaba/OpenSandbox/sdks/sandbox/go v1.4.0" }
  - { product: sandbox-pool-redis, language: go,       package: "go:github.com/alibaba/OpenSandbox/sdks/sandbox/go/poolredis v1.4.0" }

specs:
  - { path: specs/sandbox-lifecycle.yml, sha256: … }
  - { path: specs/execd-api.yaml,        sha256: … }

attestation:
  bomSha256: …
  signatures:
    - { type: sigstore-bundle, ref: releases/opensandbox/1.4.0.yaml.sigstore.json }
```

Design points:

- The BOM is authoritative for **image digests**, not versions —
  the version is the file name.
- ~4 KB per file; ~600 KB over five years at 2-week cadence.
- Generated by workflow, so version-string drift is impossible.

### Release Workflow

New `.github/workflows/release-umbrella.yml`:

```
on: workflow_dispatch
inputs:
  version:          # e.g. 1.4.0 or 1.4.0-rc.1
  channel:          # stable | rc
  release_branch:   # main or release-1.4
  dry_run:          # default true
```

The workflow uses a **build-hold-publish** model so that no
user-pullable artifact carries the release identity until every
target has been built successfully. Partial releases are
structurally impossible; the "flag partial artifacts in a cleanup
issue" pattern is explicitly rejected.

The model treats **container images** and **language / chart
packages** differently, because their publish primitives support
different atomicity guarantees:

- **Container images: stage-then-promote in-registry.** OCI
  registries let a tag point at an existing digest without a
  rebuild. Images are pushed once at a staging tag
  `opensandbox/<comp>:staging-<commit>-<runid>` (namespaced,
  disjoint from the frozen legacy `v` prefix and the umbrella
  `release-` prefix). After all legs succeed, the workflow
  invokes `crane tag` (or equivalent) to add `release-X.Y.Z` as
  a second tag pointing at the same digest. The digest and its
  Sigstore attestation carry over unchanged; no rebuild.

- **Language packages and Helm chart: internal-hold-then-publish.**
  Package registries (PyPI, npm, Maven Central, NuGet, Helm OCI)
  embed the version string in package metadata and their
  filenames. There is no primitive that renames
  `1.4.0-staging.42` to `1.4.0`, and rebuilding a fresh `1.4.0`
  artifact cannot yield the same SHA-256 as the staged
  prerelease. The workflow therefore does **not** publish
  prerelease packages to public registries during staging.
  Instead:

  1. Every language / chart target builds its artifact at the
     final `X.Y.Z` version and uploads the artifact as a
     **workflow artifact** (private to the release run). Every
     build emits the artifact SHA-256 for the atomicity manifest.
  2. Nothing goes to PyPI / npm / Maven Central / NuGet /
     Helm OCI at this stage. External users cannot see any part
     of the release.
  3. After image staging and every language build succeed and
     the BOM commit `C_bom` is authored, the workflow publishes
     each held artifact to its public registry from the workflow
     artifact bytes, at `X.Y.Z` (first-and-only public publish).
     Each ecosystem's native publish primitive is used
     (`twine upload`, `npm publish`, `gradle publish`,
     `dotnet nuget push`, `helm push`).
  4. Publish order within this final step is: Helm chart first
     (largest blast radius if something fails), then language
     packages fanned out in parallel. A single failed leg after
     any successful public publish falls back to the ecosystem's
     rollback primitive as a best-effort remediation, and files
     a P0 release-manager incident. This residual risk is
     inherent to registries that do not support cross-package
     transactions and is documented as a known limitation.

**Staging identifiers cleanup.** Staging image tags are namespaced
and not linked from any user-facing surface; a scheduled GC deletes
staging tags whose parent workflow run did not complete within 14
days. Workflow artifacts are pruned per the repository's default
retention. No prerelease package identities ever exist on public
package registries under this model.

Steps:

1. **Preflight.** Reuse `release-preflight.yml` (approval +
   `origin/<branch>` reachability of the *build commit*, called
   `C_build` below).
2. **Version-consistency check.** Verify that every
   OpenSandbox-owned version field on `C_build` matches `${version}`.
   The scan is **scoped**, not a whole-tree regex: it checks a
   fixed set of source-of-truth files that own the platform's own
   version string. Everything else (OpenAPI `openapi:` fields,
   language-runtime pins in workflows, lockfile dependency
   versions, third-party constants, examples) is out of scope by
   design.

   The scoped file set falls into three groups.

   **Direct version fields:**

   - `kubernetes/charts/opensandbox/Chart.yaml` — both `version:`
     and `appVersion:` must equal `${version}`.
   - `kubernetes/charts/opensandbox/values.yaml` (and every
     sub-chart's `values.yaml`) — every reference to an
     OpenSandbox platform image must resolve to
     `release-${version}`. The scan covers both shapes the chart
     currently uses:
     - Split fields (`.image.repository` + `.image.tag`) — tag
       must equal `release-${version}`.
     - Full-image string fields such as
       `.controller.snapshot.imageCommitterImage` (currently
       `opensandbox/image-committer:v0.1.1`) — the trailing tag
       segment after the last `:` must equal `release-${version}`
       for every image whose repository namespace matches
       `**/opensandbox/*`.

     Non-OpenSandbox image references (e.g. sidecar images from
     third parties) are ignored. Enforcing this on the
     full-string form is necessary because image-committer and
     any similarly-injected image do not go through a `.image.tag`
     field today.
   - `sdks/**/package.json` (`.version`) for JS/TS SDKs.
   - `sdks/sandbox/kotlin/gradle.properties` (`project.version`).
   - Every `sdks/**/*.csproj` (`<Version>`) and
     `sdks/Directory.Build.props` (`<OpenSandboxPackageVersion>`).
   - `sdks/sandbox/go/version.go` (or equivalent) if a Go version
     constant exists.
   - Server config default that reports itself: the source of the
     `/version` endpoint payload.

   **Dynamic version sources (hatch-vcs).** The `opensandbox-cli`,
   `opensandbox-server`, and every Python SDK derive their version
   from `hatch-vcs` with per-package `tag_regex` fields that
   currently match legacy per-component tags (`cli/v*`,
   `server/v*`, `python/sandbox/v*`, `python/code-interpreter/v*`,
   `python/mcp/sandbox/v*`). Under this OSEP those legacy tag
   patterns will never match a new tag again, so the derived
   version would fall back to `0.0.0` or the last legacy tag.
   Implementation must change every affected `pyproject.toml`
   `tool.hatch.version.raw-options.tag_regex` (and matching
   `git_describe_command`) to the umbrella pattern
   `^opensandbox/(?P<version>\d+\.\d+\.\d+(?:[.\w+\-]*)?)$` **as
   part of the same PR that lands this OSEP's Phase 2 workflow**.
   The version-consistency scan verifies that every hatch-vcs
   `tag_regex` in the tree references the umbrella tag pattern
   and not a legacy per-component pattern.

   **Inter-package dependency ranges.** Some SDKs pin OpenSandbox
   version ranges internally that will constrain to legacy
   versions unless updated:

   - `sdks/code-interpreter/python/pyproject.toml` — dependency
     `opensandbox>=0.1.10,<0.2.0`.
   - `sdks/mcp/sandbox/python/pyproject.toml` — same range.
   - `sdks/Directory.Build.props` —
     `<OpenSandboxDependencyVersionRange>[$(OpenSandboxPackageVersion),0.2.0)</OpenSandboxDependencyVersionRange>`.

   The scan verifies that every inter-OpenSandbox dependency
   range (a) uses the current umbrella version as its lower bound
   and (b) uses a same-major upper bound (e.g. `[1.4.0,2.0.0)` at
   umbrella `1.4.0`). Anything else is release-blocking.

   Anything **outside** this three-group set (OpenAPI `openapi:`
   fields, third-party dependency versions in workflows or
   lockfiles, example manifests) is ignored. The full path list
   and range rules are maintained in
   `scripts/release/version-consistency-paths.txt` so they can be
   updated without OSEP amendment.
3. **Fan-out build.** Matrix job invokes every per-target routine
   in umbrella mode (see below):
   - Container images: build and push to
     `opensandbox/<comp>:staging-<commit>-<runid>`.
   - Language / chart packages: build the final `X.Y.Z` artifact,
     upload it as a workflow artifact, and record its SHA-256.
     Do **not** publish to public package registries here.
   Every leg emits its identifier (image digest or artifact SHA-256)
   into a workflow-artifact manifest.
4. **Atomicity guard.** If any leg fails, the workflow exits
   before any release-facing publish or tag is created. No
   `release-X.Y.Z` image tag, no public package version at
   `X.Y.Z`, no umbrella git tag ever appears. Staging image tags
   remain (namespaced, not linked from user-facing surfaces) and
   are garbage-collected after 14 days. Workflow-artifact packages
   are pruned per default retention.
5. **BOM assembly and commit (BOM commit `C_bom`).** The workflow
   assembles `releases/opensandbox/X.Y.Z.yaml` from the collected
   image digests and workflow-artifact SHA-256s, commits it on top
   of `C_build` to `<release_branch>` as `C_bom` with message
   `release(opensandbox): pin BOM for X.Y.Z`. The commit contains
   only the BOM YAML and its Sigstore bundle; no code changes.
6. **Publish.** Each held artifact reaches its user-facing identity
   for the first time:
   - Container images: add `release-X.Y.Z` as a second tag
     pointing at the staged digest via `crane tag` (no rebuild,
     no re-push of bytes; the digest and its Sigstore attestation
     are inherited).
   - Helm chart: `helm push` the held OCI chart to the public
     chart repository at `X.Y.Z` (first).
   - Language packages: for each ecosystem, upload the held
     workflow-artifact bytes at `X.Y.Z` using the ecosystem's
     native publish primitive (`twine upload`, `npm publish`,
     `gradle publish`, `dotnet nuget push`). Fanned out in
     parallel after the chart publish returns 2xx.
   A publish failure after any successful public publish in this
   step falls back to the ecosystem's rollback / yank primitive
   as a best-effort remediation and files a P0 incident. See
   "Residual atomicity risk" below.
7. **Umbrella tag.** `git tag -a opensandbox/X.Y.Z` on `C_bom`
   (not `C_build`), push, attach BOM + attestations to the GitHub
   Release via `gh release create`. Rationale for tagging `C_bom`:
   the tag's tree contains the BOM the release advertises; walking
   from the tag one commit back (`opensandbox/X.Y.Z^`) yields
   `C_build`, which is recorded in the BOM's `metadata.gitCommit`
   field. Both commits are permanent on the release branch.

   Additionally mint the Go SDK subdirectory-module companion
   tags on the same commit as the umbrella tag: currently
   `sdks/sandbox/go/vX.Y.Z` and `sdks/sandbox/go/poolredis/vX.Y.Z`.
   See [Naming Rules](#naming-rules), rule 5.
8. **Aggregated release notes** from per-PR labels
   (`.github/workflows/pr-label-check.yml`) since the previous
   umbrella tag.

`scripts/release/create-release.sh` gains a new target
`--target opensandbox` that computes `opensandbox/X.Y.Z` (no `v`),
takes no path filter, and requires `--version X.Y.Z`. It orchestrates
the build → hold → publish sequence above by invoking the per-target
routines under a new umbrella-mode flag.

**Existing per-target publish routines must be adapted, not reused
verbatim.** Today, `server --version 1.4.0` produces the git tag
`server/v1.4.0` and image tag `1.4.0`; `docker/execd --version 1.4.0`
produces `docker/execd/1.4.0` and image tag `1.4.0`; `helm/opensandbox`
only edits `appVersion` in the checked-in chart. None of these match
the umbrella identities defined by this OSEP (`opensandbox/1.4.0`
git tag, `release-1.4.0` image tags, `opensandbox-1.4.0.tgz` chart).
Implementation must:

1. Add an **umbrella mode** to `create-release.sh` and the underlying
   `build.sh` scripts that, when set, (a) suppresses the legacy
   `<target>/v<version>` git tag emission (the umbrella tag is the
   only tag), (b) forces image tags to `release-<version>` for every
   component, and (c) synchronizes Helm `chart.version` (not just
   `appVersion`) to `<version>` before packaging.
2. Update `publish-server.yml`, `publish-components.yml`,
   `publish-helm-chart.yml`, `publish-cli.yml`, and every
   `publish-*-sdks.yml` to accept the umbrella-mode inputs and
   respect the two-phase staging → promotion contract in
   [Release Workflow](#release-workflow).
3. Retire the per-target `workflow_dispatch` triggers on those
   publish workflows (or gate them behind a manual-override flag
   used only for emergency deletions), so that per-component
   releases cannot be cut in parallel with an umbrella release.

Legacy per-target tag namespaces (`server/vX.Y.Z`,
`docker/execd/vX.Y.Z`, …) remain in the protected ruleset for
history but are frozen at umbrella `1.0.0` — no new tags in those
namespaces are minted afterward.

**Two commits, one release.** The two-commit shape (`C_build` +
`C_bom`) is a deliberate choice that reviewers should understand:

- `C_build` is what every artifact is built from; its tree does
  not contain the BOM.
- `C_bom` is the release commit; its tree contains the BOM, which
  in turn pins the digests of the artifacts built from `C_build`.
- The umbrella tag points at `C_bom`. `git show opensandbox/X.Y.Z`
  reveals only the BOM change, which is exactly what a release
  reviewer needs to verify.
- `metadata.gitCommit` in the BOM points at `C_build`, so the
  audit chain from tag → BOM → source is single-hop and explicit.

**Failure-safe publish contract.** The final publish step (step 6)
is the only place where partial state is possible, and only after
every build has already succeeded. The workflow enforces a
verify-then-continue contract with an explicit rollback ledger:

1. **Ordered publish, verify-then-continue.** Each publish leg
   completes in a well-defined order (Helm chart → PyPI packages
   → npm packages → Maven staged repo → NuGet). After each leg
   returns 2xx, the workflow **verifies** the artifact is
   externally resolvable by pulling its metadata (registry HEAD /
   `pip index versions` / `npm view` / `helm show chart` /
   `nuget list`). Only after verification succeeds does the next
   leg start. A failed verification is treated identically to a
   failed publish and triggers rollback.
2. **Rollback ledger.** Every successful publish appends
   `{ecosystem, package, version, revocation_command}` to an
   in-workflow ledger. On any leg's failure, the workflow walks
   the ledger in reverse and issues each revocation command:
   - PyPI: `pip index yank` (permanent yank within retention
     window; deletion is not scriptable but yanking blocks new
     installs).
   - npm: `npm deprecate` + `npm dist-tag rm latest`. Full
     `unpublish` is only possible within 72 hours; the workflow
     attempts it first and falls back to deprecate.
   - Maven Central: the staged repo is `mvn nexus-staging:drop`
     rather than released. Because the workflow uses staged
     repos and only auto-releases after all other publishes
     verify, Maven Central rollback is fully reversible.
   - NuGet: `dotnet nuget delete` within 72h; falls back to
     `unlist` after that window.
   - Helm OCI: `oras rm` (OCI reference delete) or
     `crane delete` if hosted on a Docker-compat registry.
   - Container images: since images are already tagged
     `release-X.Y.Z` at this point, rollback deletes the tag
     via `crane delete` (leaves the digest reachable via its
     staging tag for post-mortem).
3. **Umbrella tag is a commit-fence.** The umbrella git tag is
   pushed **only after** the last verification succeeds. On
   rollback the tag is never pushed; the release simply does not
   exist to consumers. The BOM commit `C_bom` **is** pushed to
   the release branch during step 5 (before publish) and stays
   as a permanent "attempted release" record even on rollback —
   this is intentional, so aborted releases are auditable, but
   without a tag pointing at it users cannot resolve it.
4. **Idempotent retry.** Every publish leg is idempotent: a
   second run at the same version detects a matching digest /
   SHA-256 and short-circuits to "already published". This lets
   a transient failure retry without extra state.
5. **P0 incident automation.** Any invocation of the rollback
   ledger files a P0 incident with the release-manager oncall,
   including the ledger contents and the failing leg's log link.

**Reversibility limits.** The above sequence keeps rollback
correct for all ecosystems **within** their yank / drop / delete
windows. Two hard limits remain:

- **Maven Central after auto-release.** Once the staged repo is
  released, artifacts are immutable. The workflow orders Maven
  Central auto-release as the very last leg to minimize this
  window; if any later verification fails, Maven Central is
  already released and cannot be rolled back — only yanked
  (deprecated) by publishing a `-yanked` metadata pointer.
- **Global caches.** Even after rollback, mirror caches (Sonatype
  OSS Index proxies, corporate npm registries, etc.) may have
  cached the artifact. Rollback is authoritative on the
  upstream registry; downstream cache invalidation is not.

These residual risks cannot be structurally eliminated at the
package-registry layer because no cross-registry two-phase
commit exists. They are accepted as known limitations of
unified versioning on public registries.

**Release branches.** New line: cut `release-X.Y` from `main` at
the release commit; tag `opensandbox/X.Y.0` on it. Subsequent
`X.Y.Z` snapshots go on `release-X.Y`. `main` receives feature
work for the next line. `release-X.Y` closes when
`release-X.(Y+1)` opens, except for the emergency-CVE window.

**Backporting to N-1.** In-line snapshots (`Z > 0`) on the current
line use exactly the same workflow above, pointed at `release-X.Y`.
Backporting to the immediately previous line (allowed only under
the emergency-CVE rule in [Cadence and Support](#cadence-and-support))
uses the same workflow again, pointed at `release-X.(Y-1)`. There
is **no separate hotfix mechanism**; unified versioning means a
back-port is a full umbrella rebuild.

Concrete steps for producing `opensandbox/1.3.6` from a current
`1.4.x` line:

1. Confirm eligibility: CVSS ≥ 8.0, ≤72h from disclosure, and the
   fix is not already in the latest `1.4.x`. Log the decision in
   the release-manager ticket.
2. Land the fix on `main` first (standard PR review).
3. Cherry-pick onto `release-1.3`:
   ```
   git switch release-1.3
   git cherry-pick <sha-on-main>
   git push origin release-1.3
   ```
4. Trigger `release-umbrella.yml` with:
   - `version: 1.3.6`
   - `channel: stable`
   - `release_branch: release-1.3`
   - `dry_run: false`
5. The workflow rebuilds every artifact from `release-1.3`'s HEAD
   at version `1.3.6` — server, all component and K8s images
   (as `release-1.3.6`), Helm chart `1.3.6`, CLI `1.3.6`, every
   SDK `1.3.6`. Same fan-out, same atomicity guard, same BOM
   generation as a normal release.
6. Release notes on the `1.3.6` GitHub Release must link the CVE
   and state the equivalent `1.4.x` snapshot that also contains
   the fix, so users on either line know their upgrade target.

The N-1 emergency snapshot is one-shot per line: if a second
qualifying CVE surfaces before the next line birth, it merges
with the first into a single `1.3.7`; the policy does not permit
an unlimited series of `1.3.x` snapshots. Any bug that is not a
CVSS ≥ 8.0 CVE waits for the next line birth (at most two weeks).

### Compatibility

- **Server ↔ CLI/SDK.** Same line supported. `±1` minor emits WARN.
  Beyond that: refuse destructive commands.
- **Server ↔ Kubernetes.** Declared per release in `compatibility.kubernetes`;
  currently exercised by `.github/workflows/kubernetes-test.yml`.
- **Ingress ↔ server.** Ships as an umbrella pair. Cross-umbrella
  mixing is untested by construction.
- **CRD deprecation.** Standard policy: ≥1 line's notice; removal
  only on a major bump.

### User-facing Surface

**Installation** is exclusively via the Helm chart. `osb` is a
client for the sandbox lifecycle API and does not install the
platform — no new install command is proposed here.

Docs:

- `docs/community/releases.md` — umbrella-first listing; legacy
  component tags in an appendix.
- `docs/reference/compatibility-matrix.md` — generated from recent BOMs.
- `docs/getting-started/installation.md` —
  `helm install opensandbox opensandbox/opensandbox --version 1.4.0`.

CLI (extends existing surface, no new subsystem):

- `osb version` — additionally prints the server's umbrella version
  and skew status (via new lifecycle `/version` endpoint).
- `osb devops doctor` — new subcommand under existing `devops`
  group. Reads cluster umbrella version (via lifecycle `/version`
  or CRD `status.umbrellaVersion`) and flags any image whose digest
  differs from the BOM shipped with the CLI.

## Test Plan

- **BOM JSON Schema** at `specs/schemas/umbrella-release.schema.json`;
  validated in CI.
- **Version-consistency scan** — unit test asserts that every
  file in `scripts/release/version-consistency-paths.txt` has been
  updated to `${version}` on the build commit `C_build`, and
  covers no files outside that set.
- **Weekly umbrella dry-run** — `release-umbrella.yml` with
  `dry_run: true` on `main` catches fan-out regressions between
  real release windows.
- **Atomicity test (build phase).** Synthetic failure of one build
  leg must abort the whole workflow before the publish step runs,
  so no `release-X.Y.Z` image tag, no public `X.Y.Z` language
  package, no chart at `X.Y.Z`, and no umbrella git tag ever
  appears. Staged image tags must remain namespaced-only; no
  public package registry receives any prerelease upload.
- **Verify-then-continue test (publish phase).** For each ordered
  publish leg, inject a synthetic verification failure and assert:
  (a) subsequent legs never start, (b) the rollback ledger is
  walked in reverse, (c) each ledger entry's revocation command
  is executed, (d) the umbrella git tag is not pushed. Run this
  test once per ecosystem (Helm, PyPI, npm, Maven, NuGet) plus
  once for the container-image retag step.
- **Idempotency test.** Re-running the release workflow at the
  same `${version}` after a partial success must detect existing
  artifacts and short-circuit each leg to no-op, without publishing
  duplicate versions or emitting new digests.
- **Staging cleanup test.** The scheduled 14-day garbage-collector
  must delete staging image tags whose parent workflow run never
  reached the publish step.
- **hatch-vcs regex test.** Assert that no `pyproject.toml` in
  the tree contains a legacy `tag_regex` referencing per-component
  tag namespaces (`cli/v*`, `server/v*`, `python/**/v*`) after
  Phase 2 migration lands.
- **Dependency range test.** Assert every inter-OpenSandbox
  version range (see `sdks/**/pyproject.toml`,
  `sdks/Directory.Build.props`) uses the current umbrella version
  as lower bound and a same-major upper bound.
- **Skew tests** in `tests/*/` for client-vs-server `±1`/`±2` minor.
- **Helm rendering golden test** in `kubernetes/test/` compares
  chart `values.yaml` against BOM-derived block.
- **`osb devops doctor` e2e** on Kind — installs umbrella `X.Y.0`,
  swaps one image out-of-BOM, asserts drift is reported.

## Drawbacks

- **~450 artifact publishes/year** (7 images + Helm + CLI + 8 SDKs,
  every 2 weeks) regardless of code churn.
- **Registry noise on package registries** — 26 no-op version bumps
  per SDK per year. Accepted as the cost of unified versioning.
- **No per-component hotfix agility.** A 1-line ingress fix ships
  as a full umbrella rebuild. This is the central trade.
- **`1.0.0` = GA is a strong claim.** A follow-up API-stability OSEP
  must land in time to back it credibly.
- **Small permanent history** — one BOM YAML + attestation per
  release, well under 1 MB over five years.

## Alternatives

**A. Keep per-component versions; umbrella is only a manifest.**
Earlier draft of this OSEP. Preserves hotfix agility; loses "single
version users can name and install". Rejected — does not deliver
the primary goal.

**B. Unified versioning without a BOM.** Cheaper machinery, but no
authoritative digest reference and no basis for `osb devops
doctor`. Rejected — BOM cost is negligible.

**C. Two-tier umbrella (`platform/X.Y.Z` + `sdks/X.Y.Z`).**
Doubles the surface users must reason about; contradicts unified
versioning. Rejected.

**D. Longer cadence + LTS.** Under unified versioning a back-port
is a full rebuild anyway, so LTS is more expensive than in model
A. Deferred to `2.0.0`.

## Migration

**Phase 1 — Provisional.** Land BOM schema and workflow with
`dry_run: true` forced on. No user-visible tag change.

**Phase 2 — `opensandbox/1.0.0-rc.1`.** First full pre-release.
Historical component tag namespaces frozen at this point.

**Phase 3 — GA `opensandbox/1.0.0`.** Workflow enabled unconditionally.
Docs, Helm defaults, `osb version` all switch to the umbrella.
Doubles as the GA declaration.

**Phase 4 — Steady state.** 2-week line births; on-demand in-line
and emergency-CVE snapshots.

**Historical tags.** Never deleted, never extended. Registries and
git keep resolving them forever. Docs keep a "legacy component
tags" appendix for at least six lines after `1.0.0`, plus a
lookup script mapping any legacy tag to its superseding umbrella.
Existing user scripts that pin per-component tags keep working.

Skew warnings from CLI/SDK are non-fatal for one line after they
first appear before graduating to a refusal.
