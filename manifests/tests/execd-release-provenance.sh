#!/usr/bin/env bash
# Copyright 2026 The OpenSandbox Authors
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
fixture="$(mktemp -d)"
trap 'rm -rf "$fixture"' EXIT

mkdir -p "$fixture/bin"
export GITHUB_REPOSITORY=opensandbox-group/OpenSandbox
export GITHUB_REF=refs/heads/release-1.99
export GITHUB_SHA=1111111111111111111111111111111111111111
export TEST_DIGEST=sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
export TEST_IMAGE=docker.io/opensandbox/execd
export TEST_TAG=release-1.99.0
export TEST_CALLS="$fixture/calls"
export PATH="$fixture/bin:$PATH"

cat >"$fixture/bin/verify-tool" <<'EOF'
#!/usr/bin/env bash
set -eu
tool=${0##*/}
echo "$tool $*" >>"$TEST_CALLS"
case "$tool" in
  crane)
    [[ "$*" == "digest $TEST_IMAGE:$TEST_TAG" ]]
    printf '%s\n' "${REGISTRY_DIGEST:-$TEST_DIGEST}"
    ;;
  cosign) [[ "${FAILED_PROOF:-}" != signature ]] ;;
  gh) [[ "${FAILED_PROOF:-}" != provenance ]] ;;
esac
EOF
chmod +x "$fixture/bin/verify-tool"
for tool in crane cosign gh; do
  ln -s verify-tool "$fixture/bin/$tool"
done

verify_release="$root/manifests/release/verify-execd-release.sh"
verify() {
  bash "$verify_release" "$TEST_IMAGE" "$TEST_TAG" "${1:-$TEST_DIGEST}"
}

verify
workflow="$GITHUB_REPOSITORY/.github/workflows/release-umbrella.yml"
grep -Fx "cosign verify $TEST_IMAGE@$TEST_DIGEST --certificate-identity https://github.com/$workflow@$GITHUB_REF --certificate-oidc-issuer https://token.actions.githubusercontent.com" "$TEST_CALLS"
grep -Fx "gh attestation verify oci://$TEST_IMAGE@$TEST_DIGEST --bundle-from-oci --repo $GITHUB_REPOSITORY --signer-workflow $workflow --source-ref $GITHUB_REF --source-digest $GITHUB_SHA" "$TEST_CALLS"

if verify sha256:bad || REGISTRY_DIGEST=sha256:different verify || FAILED_PROOF=signature verify || FAILED_PROOF=provenance verify; then
  echo "Invalid image evidence was accepted." >&2
  exit 1
fi

# The release branch can advance after Actions has started building.
source_repo="$fixture/source"
mkdir -p "$source_repo"/{manifests/release,docs/releases,specs}
cp "$root/manifests/release/"{create-umbrella-release.sh,umbrella-release.yaml.tmpl} "$source_repo/manifests/release/"
cd "$source_repo"
printf 'Fixture release notes\n' >docs/releases/1.99.0.md
for spec in specs/sandbox-lifecycle.yml specs/execd-api.yaml specs/egress-api.yaml specs/diagnostic-api.yml; do
  printf 'built version of %s\n' "$spec" >"$spec"
done
git init --quiet -b release-1.99
git config user.name 'Release test'
git config user.email 'test@example.invalid'
git config commit.gpgsign false
git add .
git commit --quiet -m 'Build source'
build_commit="$(git rev-parse HEAD)"
build_spec_sha="$(shasum -a 256 specs/execd-api.yaml | cut -d' ' -f1)"
printf 'new branch version\n' >specs/execd-api.yaml
git commit --quiet -am 'Advance release branch'
branch_commit="$(git rev-parse HEAD)"

render_bom() {
  bash manifests/release/create-umbrella-release.sh --version 1.99.0 --dry-run --skip-remote-check --skip-consistency "$@"
}

render_bom --build-commit "$build_commit" >"$fixture/pinned-bom"
grep -Fx "  gitCommit: $build_commit" "$fixture/pinned-bom"
grep -Fx "    sha256: $build_spec_sha" "$fixture/pinned-bom"
render_bom >"$fixture/default-bom"
grep -Fx "  gitCommit: $branch_commit" "$fixture/default-bom"
if grep -Fq "$build_spec_sha" "$fixture/default-bom"; then
  echo "BOM generated without --build-commit used an old spec hash." >&2
  exit 1
fi
echo "Release provenance checks passed."
