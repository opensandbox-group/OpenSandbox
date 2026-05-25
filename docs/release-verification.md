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
- Container images: the component and server image workflows sign Docker Hub
  and ACR image digests with `cosign` keyless signing, and publish provenance
  attestations to the registries.
- Python and CLI packages: wheels and source distributions are attested before
  `uv publish`.
- JavaScript packages: the workflow runs `pnpm pack`, attests the generated npm
  tarball, and publishes that same tarball.
- C# packages: NuGet `.nupkg` files are attested before publication.
- Go SDK modules: the `sdks/sandbox/go/v<version>` source release archive is
  attested by the Generic Release workflow.
- Helm charts: packaged chart `.tgz` files are attested before upload to the
  GitHub Release.
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

- Repository: `alibaba/OpenSandbox`
- OIDC issuer: `https://token.actions.githubusercontent.com`
- Source release workflow: `alibaba/OpenSandbox/.github/workflows/release-generic.yml`
- Component image workflow: `alibaba/OpenSandbox/.github/workflows/publish-components.yml`
- Server image workflow: `alibaba/OpenSandbox/.github/workflows/publish-server.yml`
- CLI package workflow: `alibaba/OpenSandbox/.github/workflows/publish-cli.yml`
- Python package workflow: `alibaba/OpenSandbox/.github/workflows/publish-python-sdks.yml`
- JavaScript package workflow: `alibaba/OpenSandbox/.github/workflows/publish-js-sdks.yml`
- C# package workflow: `alibaba/OpenSandbox/.github/workflows/publish-csharp-sdks.yml`
- Helm chart workflow: `alibaba/OpenSandbox/.github/workflows/publish-helm-chart.yml`

If you run the release workflows from a downstream fork, replace
`alibaba/OpenSandbox` in the verification commands with that fork's
`owner/repository` identity.

Private signing material is not stored in GitHub Releases, Docker Hub, ACR,
PyPI, npm, Maven Central, NuGet, or Helm chart downloads. Java/Kotlin Maven
Central signing keys are held only in GitHub Actions secrets.

## Verify Source Releases

Set the release tag first:

```bash
TAG="server/v0.1.13"
SAFE_TAG="${TAG//\//-}"
```

Download the signed source archive and checksum file:

```bash
gh release download "$TAG" \
  --repo alibaba/OpenSandbox \
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
  --repo alibaba/OpenSandbox \
  --signer-workflow alibaba/OpenSandbox/.github/workflows/release-generic.yml
```

Verify the checksum file attestation:

```bash
gh attestation verify SHA256SUMS \
  --repo alibaba/OpenSandbox \
  --signer-workflow alibaba/OpenSandbox/.github/workflows/release-generic.yml
```

The Generic Release workflow is started with `workflow_dispatch`, so its
provenance `source-ref` is the ref selected when the workflow was dispatched
(normally `refs/heads/main`), not the release tag created by the job.

## Verify Container Images

Install `cosign` and `gh`, then resolve the image digest. Always verify by
digest, not by mutable tag alone.

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
  --certificate-identity-regexp '^https://github.com/alibaba/OpenSandbox/.github/workflows/publish-components.yml@refs/tags/(docker|k8s)/[^/]+/v?[0-9].*$'
```

Verify the registry provenance attestation:

```bash
gh attestation verify "oci://${IMAGE_REF}" \
  --repo alibaba/OpenSandbox \
  --bundle-from-oci \
  --signer-workflow alibaba/OpenSandbox/.github/workflows/publish-components.yml
```

For the server image, use `docker.io/opensandbox/server` and the server workflow:

```bash
IMAGE="docker.io/opensandbox/server"
TAG="v0.1.13"
DIGEST="$(docker buildx imagetools inspect "${IMAGE}:${TAG}" --format '{{.Manifest.Digest}}')"
IMAGE_REF="${IMAGE}@${DIGEST}"

cosign verify "$IMAGE_REF" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp '^https://github.com/alibaba/OpenSandbox/.github/workflows/publish-server.yml@refs/tags/server/v[0-9].*$'
```

ACR images use the same digest and identity checks with the ACR image name, for
example:

```bash
IMAGE="sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/execd"
```

## Verify Packages

Download the package file from its normal package registry, then verify the
attestation against the OpenSandbox repository and the expected release tag.

Python and CLI packages:

```bash
python -m pip download opensandbox-server==0.1.13 --no-deps
gh attestation verify opensandbox_server-0.1.13*.whl \
  --repo alibaba/OpenSandbox \
  --signer-workflow alibaba/OpenSandbox/.github/workflows/publish-server.yml \
  --source-ref refs/tags/server/v0.1.13
```

JavaScript packages:

```bash
npm pack @alibaba-group/opensandbox@0.1.7
gh attestation verify alibaba-group-opensandbox-0.1.7.tgz \
  --repo alibaba/OpenSandbox \
  --signer-workflow alibaba/OpenSandbox/.github/workflows/publish-js-sdks.yml \
  --source-ref refs/tags/js/sandbox/v0.1.7
```

C# packages:

```bash
gh attestation verify Alibaba.OpenSandbox.1.0.0.nupkg \
  --repo alibaba/OpenSandbox \
  --signer-workflow alibaba/OpenSandbox/.github/workflows/publish-csharp-sdks.yml \
  --source-ref refs/tags/csharp/sandbox/v1.0.0
```

Helm charts:

```bash
gh release download helm/opensandbox/0.1.0 \
  --repo alibaba/OpenSandbox \
  --pattern 'opensandbox-*.tgz'
gh attestation verify opensandbox-*.tgz \
  --repo alibaba/OpenSandbox \
  --signer-workflow alibaba/OpenSandbox/.github/workflows/publish-helm-chart.yml
```

When Helm charts are released through `workflow_dispatch`, their provenance
`source-ref` is the selected dispatch ref. Tag-triggered Helm releases have a
tag `source-ref`.

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
