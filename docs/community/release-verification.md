---
title: Release Verification
description: Verify signed OpenSandbox releases for source archives, container images, packages, and Helm charts.
---

# Release Verification

OpenSandbox signs public release outputs without changing the normal install
commands. Verification is optional for day-to-day use, but supported for users
who need supply chain integrity checks.

This process applies to releases produced after the signing workflows were
introduced. Older releases may not have attestations or signatures.

## Signing Model

OpenSandbox uses these signing paths:

- Source code releases: the Generic Release workflow uploads an explicit
  `opensandbox-<tag>.tar.gz` source archive and `SHA256SUMS` file to the GitHub
  Release, then creates GitHub/Sigstore provenance attestations for both files.
- Container images: the component and server image workflows sign Docker Hub,
  GitHub Container Registry (GHCR), and Alibaba Cloud Container Registry (ACR)
  image digests with `cosign` keyless signing, and publish provenance
  attestations to all three registries.
- Python and CLI packages: wheels and source distributions are attested before
  `uv publish`.
- JavaScript packages: the workflow runs `pnpm pack`, attests the generated npm
  tarball, and publishes that same tarball.
- C# packages: NuGet `.nupkg` files are attested before publication.
- Go SDK modules: the `sdks/sandbox/go/v<version>` source release archive is
  attested by the Generic Release workflow.
- Helm charts created by the current release workflow: packaged chart `.tgz`
  files are attested and published with an attested `SHA256SUMS` file so
  consumers can verify the exact release bytes.
- Java/Kotlin packages: Maven Central publications are signed by the Gradle
  Maven publish signing configuration. Download the `.asc` signature next to
  the Maven artifact and verify it with OpenPGP tooling.

Release tags may also be signed with `scripts/release/create-release.sh
--sign-tag` when the release operator has a local git signing key configured.
Do not rely on signed tags alone for generated deliverables; verify the
artifact you are installing.

## Trust Roots and Keys

Most OpenSandbox release signatures are keyless Sigstore signatures created by
GitHub Actions OpenID Connect (OIDC). There is no long-lived OpenSandbox private
key for these signatures, so there is no project public key file to download.
The public certificates and signed bundles are retrieved by `gh` or `cosign`
from GitHub's attestation service, OCI registries, and Sigstore transparency
infrastructure.

Expected identity values:

- Repository: `opensandbox-group/OpenSandbox`
- OIDC issuer: `https://token.actions.githubusercontent.com`
- Source release workflow: `opensandbox-group/OpenSandbox/.github/workflows/release-generic.yml`
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

For releases produced before the GitHub organization migration, use the
historical identity instead:

```bash
REPOSITORY="alibaba/OpenSandbox"
WORKFLOW_REPOSITORY="${REPOSITORY}"
WORKFLOW_REPOSITORY_URL="https://github.com/${WORKFLOW_REPOSITORY}"
```

If you run the release workflows from a downstream fork, replace
`opensandbox-group/OpenSandbox` in the verification commands with that fork's
`owner/repository` identity.

Private signing material is not stored in GitHub Releases, Docker Hub, GHCR,
ACR, PyPI, npm, Maven Central, NuGet, or Helm chart downloads. Java/Kotlin
Maven Central signing keys are held only in GitHub Actions secrets.

## Verify Source Releases

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

The Generic Release workflow is started with `workflow_dispatch`, so its
provenance `source-ref` is the ref selected when the workflow was dispatched
(normally `refs/heads/main`), not the release tag created by the job.

## Verify Container Images

Install `cosign` and `gh`, then resolve the image digest. Always verify by
digest, not by mutable tag alone.

Release images are published with the same component name and digest in all
three official registries:

| Registry | Image name pattern |
| --- | --- |
| Docker Hub | `docker.io/opensandbox/<component>` |
| GitHub Container Registry | `ghcr.io/opensandbox-group/opensandbox/<component>` |
| Alibaba Cloud Container Registry | `sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/<component>` |

The component can be `execd`, `code-interpreter`, `ingress`, `egress`,
`controller`, `task-executor`, `image-committer`, or `nodeagent`. The server
image uses the component name `server`.

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

GHCR and ACR images use the same digest and identity checks with their
respective image names, for example:

```bash
IMAGE="ghcr.io/opensandbox-group/opensandbox/execd"
# or
IMAGE="sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/execd"
```

## Verify Packages

Download the package file from its normal package registry, then verify the
attestation against the OpenSandbox repository and the expected release tag.

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

The checksum binds the downloaded package to the digest recorded by the
release workflow. The attestation verifies that the package with that digest
was produced by the expected OpenSandbox workflow. Change `HELM_CHART` and
`HELM_VERSION` to verify a different Helm release.

Manual Helm releases must select the exact existing release tag as their
dispatch ref. Their provenance `source-ref`, like a tag-triggered Helm release,
therefore identifies that protected Helm tag.

### Helm Release Status

A stable `opensandbox` umbrella chart is marked `production-ready` only after
the release workflow installs the exact packaged `.tgz` identified by the
published digest into an ephemeral Kind cluster. The workflow does not rebuild
the chart between the runtime gate and publication. The gate must verify:

- The controller and server workloads become Ready.
- The required OpenSandbox CRDs are established and usable.
- Authenticated server access succeeds, while missing or invalid credentials
  are rejected.
- A BatchSandbox completes the create, command-execution, and delete lifecycle.

This status covers the default core profile named in the Release notes. It does
not certify optional ingress, egress policy, snapshot/pause-resume, node-agent,
upgrade, or multi-architecture lanes unless those profiles are listed
separately.

Standalone `opensandbox-controller`, `opensandbox-server`, and
`opensandbox-node-agent` chart releases are `package-verified` only. Their
validation covers the packaged chart and does not claim the integrated Kind
runtime coverage of a stable umbrella release.

Each Helm GitHub Release records the package SHA-256 digest, source ref and
commit, validation workflow run, validation profile, and—when runtime-tested—
the requested core image references, registry RepoDigests, and image IDs in its
release notes. The profile distinguishes the stable umbrella Kind runtime gate
from package-only verification. Because chart defaults remain documented image
version tags, `production-ready` records the publication-time gate result; it
does not claim that a registry tag can never be changed later.

Java/Kotlin Maven artifacts:

```bash
curl -O https://repo1.maven.org/maven2/com/alibaba/opensandbox/sandbox/1.0.10/sandbox-1.0.10.jar
curl -O https://repo1.maven.org/maven2/com/alibaba/opensandbox/sandbox/1.0.10/sandbox-1.0.10.jar.asc
KEY_ID="$(gpg --list-packets sandbox-1.0.10.jar.asc | awk '/keyid/ { print $NF; exit }')"
gpg --keyserver hkps://keys.openpgp.org --recv-keys "$KEY_ID"
gpg --verify sandbox-1.0.10.jar.asc sandbox-1.0.10.jar
```

If verification cannot find an attestation for a release that predates this
process, use a newer release as the signed release evidence.
