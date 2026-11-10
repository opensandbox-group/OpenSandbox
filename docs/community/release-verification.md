---
title: Release Verification
description: Check release signing coverage and verify supported legacy OpenSandbox artifacts.
---

# Release Verification

Signing coverage depends on the release workflow, not just the release date or version. The legacy per-component workflows supported the verification commands below; the current umbrella release process does not provide the same signing coverage.

## Current Umbrella Release Status

::: warning Signing and provenance gap
The current [umbrella release workflow](https://github.com/opensandbox-group/OpenSandbox/blob/main/.github/workflows/release-umbrella.yml) and [package release workflow](https://github.com/opensandbox-group/OpenSandbox/blob/main/.github/workflows/release-packages.yml) do not contain Cosign signing or GitHub build-provenance attestation steps. Declaring `attestations: write` permission does not create an attestation. Do not assume that an umbrella release is signed because its version is newer than a signed per-component release.
:::

[Issue #2463](https://github.com/opensandbox-group/OpenSandbox/issues/2463) reports missing signatures and provenance for the examined `release-1.1.0`, `release-1.1.1`, and `release-1.2.0` artifacts, including the server 1.2.0 wheel. It tracks restoring coverage and clarifying registry availability. This guide does not promise that those artifacts will be re-signed.

For umbrella images, use the digest recorded in the versioned [release BOM](https://github.com/opensandbox-group/OpenSandbox/tree/main/docs/releases), for example [1.2.0.yaml](https://github.com/opensandbox-group/OpenSandbox/blob/main/docs/releases/1.2.0.yaml), when pinning an image. Digest matching checks artifact identity; it is not proof of who built the artifact. The BOM's `metadata.gitCommit` records the build commit, which can differ from the release tag's commit because the release workflow commits the finalized BOM before creating the tag.

If your deployment policy requires a verified signature or provenance, an absent attestation is an unmet requirement, not a successful verification. Do not replace the legacy workflow name in the commands below with `release-umbrella.yml` and treat that as evidence: changing an identity constraint cannot create missing signatures. Check the artifact's actual verification result and the resolution of #2463 before relying on umbrella release provenance.

## Legacy Signing Model {#signing-model}

Before the umbrella migration, the per-target workflows used these signing paths:

- Source code releases: the Generic Release workflow uploads an explicit `opensandbox-<tag>.tar.gz` source archive and `SHA256SUMS` file to the GitHub Release, then creates GitHub/Sigstore provenance attestations for both files.
- Container images: the component and server image workflows sign Docker Hub, GitHub Container Registry (GHCR), and Alibaba Cloud Container Registry (ACR) image digests with `cosign` keyless signing, and publish provenance attestations to all three registries.
- Python and CLI packages: wheels and source distributions are attested before `uv publish`.
- JavaScript packages: the workflow runs `pnpm pack`, attests the generated npm tarball, and publishes that same tarball.
- C# packages: NuGet `.nupkg` files are attested before publication.
- Go SDK modules: the `sdks/sandbox/go/v<version>` source release archive is attested by the Generic Release workflow.
- Helm charts created by the legacy Helm release workflow: packaged chart `.tgz` files are attested and published with an attested `SHA256SUMS` file so consumers can verify the exact release bytes.
- Java/Kotlin packages: Maven Central publications are signed by the Gradle Maven publish signing configuration. Download the `.asc` signature next to the Maven artifact and verify it with OpenPGP tooling.

Release tags may also be signed with `manifests/release/create-release.sh --sign-tag` when the release operator has a local git signing key configured. Do not rely on signed tags alone for generated deliverables; verify the artifact you are installing.

## Legacy Trust Roots and Keys {#trust-roots-and-keys}

The legacy GitHub Actions signatures described here are keyless Sigstore signatures created by GitHub Actions OpenID Connect (OIDC). There is no long-lived OpenSandbox private key for these signatures, so there is no project public key file to download. The public certificates and signed bundles are retrieved by `gh` or `cosign` from GitHub's attestation service, OCI registries, and Sigstore transparency infrastructure.

Expected identity values:

- Repository: `opensandbox-group/OpenSandbox`
- OIDC issuer: `https://token.actions.githubusercontent.com`

::: warning Legacy workflow identities (pre-umbrella releases only)
The per-target publish workflows below were removed when OpenSandbox moved to unified umbrella releases. They remain the verification identity for artifacts published before that point. New umbrella releases are produced by `release-umbrella.yml` and `release-packages.yml`.
:::

- Component image workflow: `opensandbox-group/OpenSandbox/.github/workflows/publish-components.yml`
- Server image workflow: `opensandbox-group/OpenSandbox/.github/workflows/publish-server.yml`
- CLI package workflow: `opensandbox-group/OpenSandbox/.github/workflows/publish-cli.yml`
- Python package workflow: `opensandbox-group/OpenSandbox/.github/workflows/publish-python-sdks.yml`
- JavaScript package workflow: `opensandbox-group/OpenSandbox/.github/workflows/publish-js-sdks.yml`
- C# package workflow: `opensandbox-group/OpenSandbox/.github/workflows/publish-csharp-sdks.yml`
- Helm chart workflow: `opensandbox-group/OpenSandbox/.github/workflows/publish-helm-chart.yml`

Set the repository identity for the release you are verifying:

```bash
REPOSITORY="opensandbox-group/OpenSandbox"
WORKFLOW_REPOSITORY="${REPOSITORY}"
WORKFLOW_REPOSITORY_URL="https://github.com/${WORKFLOW_REPOSITORY}"
```

For releases produced before the GitHub organization migration, use the historical identity instead:

```bash
REPOSITORY="alibaba/OpenSandbox"
WORKFLOW_REPOSITORY="${REPOSITORY}"
WORKFLOW_REPOSITORY_URL="https://github.com/${WORKFLOW_REPOSITORY}"
```

If you run the release workflows from a downstream fork, replace `opensandbox-group/OpenSandbox` in the verification commands with that fork's `owner/repository` identity.

Private signing material is not stored in GitHub Releases, Docker Hub, GHCR, ACR, PyPI, npm, Maven Central, NuGet, or Helm chart downloads. Java/Kotlin Maven Central signing keys are held only in GitHub Actions secrets.

## Verify Legacy Source Releases {#verify-source-releases}

> Source archives are produced only for releases before 2026-08 (when the `release-generic.yml` workflow was removed). Newer releases have no source archive; skip this section for them.

Set the release tag first:

```bash
TAG="server/v0.1.13"
SAFE_TAG="${TAG//\//-}"
```

Download the signed source archive and checksum file:

```bash
gh release download "$TAG" \
  --repo "$REPOSITORY" \
  --pattern "opensandbox-${SAFE_TAG}.tar.gz" \
  --pattern "SHA256SUMS"
```

Check the archive digest:

```bash
sha256sum -c SHA256SUMS
```

Verify the source archive attestation:

```bash
gh attestation verify "opensandbox-${SAFE_TAG}.tar.gz" \
  --repo "$REPOSITORY" \
  --signer-workflow "${WORKFLOW_REPOSITORY}/.github/workflows/release-generic.yml"
```

Verify the checksum file attestation:

```bash
gh attestation verify SHA256SUMS \
  --repo "$REPOSITORY" \
  --signer-workflow "${WORKFLOW_REPOSITORY}/.github/workflows/release-generic.yml"
```

The Generic Release workflow is started with `workflow_dispatch`, so its provenance `source-ref` is the ref selected when the workflow was dispatched (normally `refs/heads/main`), not the release tag created by the job.

## Verify Legacy Container Images {#verify-container-images}

Install `cosign` and `gh`, then resolve the image digest. Always verify by digest, not by mutable tag alone.

The legacy image workflows targeted the following registry locations with the same component name and digest. Availability and signing coverage for current umbrella releases must be checked separately; see the warning above and #2463.

| Registry | Image name pattern |
| --- | --- |
| Docker Hub | `docker.io/opensandbox/<component>` |
| GitHub Container Registry | `ghcr.io/opensandbox-group/opensandbox/<component>` |
| Alibaba Cloud Container Registry | `sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/<component>` |

The component can be `execd`, `ingress`, `egress`, `controller`, `task-executor`, `image-committer`, or `nodeagent`. The server image uses the component name `server`.

::: tip Code Interpreter Image Verification
The standalone `code-interpreter` environment image is published and maintained from [opensandbox-group/sandbox-images](https://github.com/opensandbox-group/sandbox-images).
- **Existing image releases** (such as `v1.1.0` and earlier) continue using the OpenSandbox workflow identity (`.github/workflows/publish-components.yml`).
- **New releases** from `opensandbox-group/sandbox-images` use its `release.yml` workflow identity. Users and operators verifying signatures or provenance for new releases must follow the verification documentation in [opensandbox-group/sandbox-images](https://github.com/opensandbox-group/sandbox-images).
:::

```bash
IMAGE="docker.io/opensandbox/execd"
TAG="v1.0.15"
DIGEST="$(docker buildx imagetools inspect "${IMAGE}:${TAG}" --format '{{.Manifest.Digest}}')"
IMAGE_REF="${IMAGE}@${DIGEST}"
```

Verify the cosign keyless signature:

```bash
cosign verify "$IMAGE_REF" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp "^${WORKFLOW_REPOSITORY_URL}/.github/workflows/publish-components.yml@refs/tags/(docker|k8s)/[^/]+/v?[0-9].*$"
```

Verify the registry provenance attestation:

```bash
gh attestation verify "oci://${IMAGE_REF}" \
  --repo "$REPOSITORY" \
  --bundle-from-oci \
  --signer-workflow "${WORKFLOW_REPOSITORY}/.github/workflows/publish-components.yml"
```

For the server image, use `docker.io/opensandbox/server` and the server workflow:

```bash
IMAGE="docker.io/opensandbox/server"
TAG="v0.1.13"
DIGEST="$(docker buildx imagetools inspect "${IMAGE}:${TAG}" --format '{{.Manifest.Digest}}')"
IMAGE_REF="${IMAGE}@${DIGEST}"

cosign verify "$IMAGE_REF" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp "^${WORKFLOW_REPOSITORY_URL}/.github/workflows/publish-server.yml@refs/tags/server/v[0-9].*$"
```

GHCR and ACR images use the same digest and identity checks with their respective image names, for example:

```bash
IMAGE="ghcr.io/opensandbox-group/opensandbox/execd"
# or
IMAGE="sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/execd"
```

## Verify Legacy Packages {#verify-packages}

Download the package file from its normal package registry, then verify the attestation against the OpenSandbox repository and the expected release tag.

Python and CLI packages:

```bash
python -m pip download opensandbox-server==0.1.13 --no-deps
gh attestation verify opensandbox_server-0.1.13*.whl \
  --repo "$REPOSITORY" \
  --signer-workflow "${WORKFLOW_REPOSITORY}/.github/workflows/publish-server.yml" \
  --source-ref refs/tags/server/v0.1.13
```

JavaScript packages:

```bash
npm pack @alibaba-group/opensandbox@0.1.7
gh attestation verify alibaba-group-opensandbox-0.1.7.tgz \
  --repo "$REPOSITORY" \
  --signer-workflow "${WORKFLOW_REPOSITORY}/.github/workflows/publish-js-sdks.yml" \
  --source-ref refs/tags/js/sandbox/v0.1.7
```

C# packages:

```bash
gh attestation verify Alibaba.OpenSandbox.1.0.0.nupkg \
  --repo "$REPOSITORY" \
  --signer-workflow "${WORKFLOW_REPOSITORY}/.github/workflows/publish-csharp-sdks.yml" \
  --source-ref refs/tags/csharp/sandbox/v1.0.0
```

Helm charts:

```bash
HELM_CHART="opensandbox"
HELM_VERSION="<chart-version>"
HELM_TAG="helm/${HELM_CHART}/${HELM_VERSION}"
HELM_PACKAGE="${HELM_CHART}-${HELM_VERSION}.tgz"

gh release download "$HELM_TAG" \
  --repo "$REPOSITORY" \
  --pattern "$HELM_PACKAGE" \
  --pattern SHA256SUMS
sha256sum -c SHA256SUMS
gh attestation verify "$HELM_PACKAGE" \
  --repo "$REPOSITORY" \
  --signer-workflow "${WORKFLOW_REPOSITORY}/.github/workflows/publish-helm-chart.yml" \
  --source-ref "refs/tags/${HELM_TAG}"
gh attestation verify SHA256SUMS \
  --repo "$REPOSITORY" \
  --signer-workflow "${WORKFLOW_REPOSITORY}/.github/workflows/publish-helm-chart.yml" \
  --source-ref "refs/tags/${HELM_TAG}"
```

The checksum binds the downloaded package to the digest recorded by the release workflow. The attestation verifies that the package with that digest was produced by the expected OpenSandbox workflow. Change `HELM_CHART` and `HELM_VERSION` to verify a different Helm release.

Manual Helm releases must select the exact existing release tag as their dispatch ref. Their provenance `source-ref`, like a tag-triggered Helm release, therefore identifies that protected Helm tag.

### Helm Release Status

A stable `opensandbox` umbrella chart is marked `production-ready` only after the release workflow installs the exact packaged `.tgz` identified by the published digest into an ephemeral Kind cluster. The workflow does not rebuild the chart between the runtime gate and publication. The gate must verify:

- The controller and server workloads become Ready.
- The required OpenSandbox CRDs are established and usable.
- Authenticated server access succeeds, while missing or invalid credentials are rejected.
- A BatchSandbox completes the create, command-execution, and delete lifecycle.

This status covers the default core profile named in the Release notes. It does not certify optional ingress, egress policy, snapshot/pause-resume, node-agent, upgrade, or multi-architecture lanes unless those profiles are listed separately.

Standalone `opensandbox-controller`, `opensandbox-server`, and `opensandbox-node-agent` chart releases are `package-verified` only. Their validation covers the packaged chart and does not claim the integrated Kind runtime coverage of a stable umbrella release.

Each Helm GitHub Release records the package SHA-256 digest, source ref and commit, validation workflow run, validation profile, and—when runtime-tested— the requested core image references, registry RepoDigests, and image IDs in its release notes. The profile distinguishes the stable umbrella Kind runtime gate from package-only verification. Because chart defaults remain documented image version tags, `production-ready` records the publication-time gate result; it does not claim that a registry tag can never be changed later.

Java/Kotlin Maven artifacts:

```bash
curl -O https://repo1.maven.org/maven2/com/alibaba/opensandbox/sandbox/1.0.10/sandbox-1.0.10.jar
curl -O https://repo1.maven.org/maven2/com/alibaba/opensandbox/sandbox/1.0.10/sandbox-1.0.10.jar.asc
KEY_ID="$(gpg --list-packets sandbox-1.0.10.jar.asc | awk '/keyid/ { print $NF; exit }')"
gpg --keyserver hkps://keys.openpgp.org --recv-keys "$KEY_ID"
gpg --verify sandbox-1.0.10.jar.asc sandbox-1.0.10.jar
```

If verification cannot find a signature or attestation, do not infer that a newer version is signed. Check the release workflow and signing coverage for the exact artifact; missing verification evidence does not satisfy a signature or provenance requirement.
