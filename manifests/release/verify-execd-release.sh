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

image="$1"
tag="$2"
digest="$3"
if [[ ! "$digest" =~ ^sha256:[0-9a-f]{64}$ ]]; then
  echo "::error::Expected a SHA-256 manifest digest for ${image}:${tag}." >&2
  exit 1
fi

tag_digest="$(crane digest "${image}:${tag}")"
if [[ "$tag_digest" != "$digest" ]]; then
  echo "::error::${image}:${tag} resolves to ${tag_digest}, expected ${digest}." >&2
  exit 1
fi

image_ref="${image}@${digest}"
workflow="${GITHUB_REPOSITORY}/.github/workflows/release-umbrella.yml"
cosign verify "$image_ref" \
  --certificate-identity "https://github.com/${workflow}@${GITHUB_REF}" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com >/dev/null
gh attestation verify "oci://${image_ref}" --bundle-from-oci \
  --repo "$GITHUB_REPOSITORY" --signer-workflow "$workflow" \
  --source-ref "$GITHUB_REF" --source-digest "$GITHUB_SHA"
