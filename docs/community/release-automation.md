---
title: Release Automation
description: How OpenSandbox releases work — one umbrella release, one tag, every artifact.
---

# Release Automation

OpenSandbox ships as a **unified umbrella release** (OSEP-0016): every
artifact of a release — platform images, Helm charts (in-repo), CLI, and
all SDKs — carries the same `X.Y.Z`, driven by a single workflow.
Per-component and per-SDK release entry points have been removed; the
umbrella is the only release path. See
[Versioning](/community/versioning) for naming and cadence.

## Release Preparation (manual, on the release branch)

```bash
# 1. one-command bump: chart versions, image references, SDK versions,
#    and dependency ranges rewritten to the target version in one commit
manifests/release/create-umbrella-release.sh --version 1.1.0 --bump-only

# 2. hand-author the release notes (copy docs/releases/TEMPLATE.md),
#    then push
cp docs/releases/TEMPLATE.md docs/releases/1.1.0.md
$EDITOR docs/releases/1.1.0.md
git push
```

## Prereleases (rc)

An rc is always cut on the **line-birth version** (`X.Y.0-rc.N` — there
is no `X.Y.Z-rc` for `Z > 0`). It validates that the platform deploys
and the fan-out runs: images are published, **all packages are held
(not published)**, and only the umbrella tag is minted (marked
`--prerelease` on GitHub). rc runs are approval-free: no `release`
environment gate, and the BOM PR merges without waiting for review (if
the release-branch ruleset still mandates review, add a ruleset bypass
for the release bot — the workflow fails fast instead of waiting).

```bash
# 1. bump (channel=rc is derived from the suffix)
manifests/release/create-umbrella-release.sh --version 1.1.0-rc.1 --bump-only

# 2. notes, then push
cp docs/releases/TEMPLATE.md docs/releases/1.1.0-rc.1.md
$EDITOR docs/releases/1.1.0-rc.1.md
git push

# 3. dispatch
gh workflow run release-umbrella.yml \
  --ref release-1.1 \
  -f version=1.1.0-rc.1 -f dry_run=false
```

Repeat with `-rc.2`, `-rc.3`, … as fixes land — every rc is a full
rebuild and previous rc artifacts stay frozen in the registries. The
stable release of the line (`1.1.0`, no suffix) then publishes packages
for real. Prereleases are never resolved by default (PyPI / NuGet /
Maven / Go hide them; npm publishes under the `rc` dist-tag).

## Release Execution (workflow)

Dispatch `.github/workflows/release-umbrella.yml`
(`dry_run=false`, `version`, `channel`, `release_branch`). Stable
releases require a release approver other than the triggerer to approve
the `release` environment; rc releases and dry runs run approval-free.
The workflow then runs:

| Stage | What happens |
|---|---|
| preflight | commit reachability + notes presence (`docs/releases/<version>.md`) |
| scan | version-consistency scan (release-blocking) |
| build | 13 images built and pushed directly to `release-X.Y.Z` (run-scoped staging tags while the `UMBRELLA_PUBLISH_ENABLED` gate is closed); all packages built and held |
| BOM | digests pinned into `docs/releases/<version>.yaml`, pushed to a `release/<version>` branch; the workflow opens a PR to the release branch and merges it (`C_bom`) — stable waits for a code-owner approval (required by the branch ruleset), rc merges immediately |
| publish | once the BOM PR merges (all images green + code-owner approval), packages publish in verify-then-continue order (PyPI → npm → NuGet → Maven Central last); images are already public at `release-X.Y.Z` from the build stage |
| tag | `release-X.Y.Z` + `sdks/sandbox/go/vX.Y.Z` minted on `C_bom`, GitHub Release created with the notes and BOM |

A failed release never mints its git tag or GitHub Release, so the
release feed never shows a partial release; the weekly scheduled
dry-run (`dry_run=true`) keeps the fan-out healthy
between release windows.

### Dry runs

`dry_run=true` exercises everything except publishing: images are built
**locally** (single-arch, `--load` — no registry push, no credentials
needed), packages are built and held as workflow artifacts, the BOM
lands on the release branch through the BOM PR (one code-owner
approval, as on any change to a protected branch) with local image
IDs standing in for registry digests, and no git tags are minted. To
rehearse a release in a fork, prepare the release branch (bump + notes)
and dispatch the workflow with `dry_run=true` — the
`UMBRELLA_PUBLISH_ENABLED` variable does not need to exist there. The
fork must enable **Allow GitHub Actions to create and approve pull
requests** (Settings → Actions → General → Workflow permissions);
`pull-requests: write` in the workflow alone cannot override that
setting being off.

## Release Artifacts

- `docs/releases/<version>.yaml` — BOM, authoritative for image digests
- `docs/releases/<version>.md` — hand-authored release notes (mirrored
  to the GitHub Release)
- container images: `opensandbox/<component>:release-X.Y.Z` on Docker
  Hub, GHCR, and ACR
- packages: PyPI (server, CLI, 3 SDKs), npm (2), Maven Central (5
  Kotlin/JVM coordinates), NuGet (2) — all at the umbrella version
- Helm charts: **not published**; they ship in-repo at the release tag

## Release Runbook

### Publish order and rollback

Packages publish in verify-then-continue order — each leg is externally
verified before the next starts. Maven Central runs **last** because a
released Maven version is immutable: ordering it last means every
reversible ecosystem is already verified before the irreversible step.
On a failure, remediate already-published legs in reverse order:

| Ecosystem | Revocation |
|---|---|
| Maven Central | not released if its leg failed; after release, immutable — deprecate only |
| NuGet | `dotnet nuget delete <pkg> <version>` (72 h), else unlist |
| npm | `npm unpublish <pkg>@<version>` (72 h), else `npm deprecate` |
| PyPI | manual yank via project settings (no scriptable API) |
| images | `crane delete <image>:release-X.Y.Z` |

Always file a P0 release incident with the run link before remediating.
Global mirror caches may retain revoked artifacts; revocation is
authoritative upstream only.

### N-1 emergency backport (CVSS ≥ 8.0, ≤ 72 h, one-shot per line)

1. Land the fix on `main` (standard PR review).
2. `git checkout release-X.(Y-1) && git cherry-pick <sha> && git push`.
3. Dispatch `release-umbrella.yml` with `version=X.(Y-1).Z`,
   `release_branch=release-X.(Y-1)`, `dry_run=false`. The full fan-out
   reruns; the release notes must link the CVE and name the equivalent
   current-line snapshot.

Anything that is not a qualifying CVE waits for the next line birth.

### Staging cleanup

Run-scoped staging image tags (`staging-<sha>-<runid>`) exist only when
a run does not publish release tags (dry runs, or real runs while the
`UMBRELLA_PUBLISH_ENABLED` gate is still closed). They are namespaced
and never referenced by user-facing surfaces; a scheduled job deletes
staging tags whose run did not complete within 14 days. Held package
artifacts use the default 14-day workflow-artifact retention.

## Legacy Releases

Historical per-component tags (`server/v0.2.3`, `docker/execd/v1.1.0`,
…) are frozen and keep resolving forever. Verification instructions for
those older artifacts (and their workflow identities) are in
[Release Verification](/community/release-verification).
