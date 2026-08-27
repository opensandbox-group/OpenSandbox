#!/usr/bin/env bash

# Copyright 2026 Alibaba Group Holding Ltd.
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

log() {
  echo "[helm-release] $*"
}

warn() {
  echo "[helm-release][warn] $*" >&2
}

die() {
  echo "[helm-release][error] $*" >&2
  exit 1
}

require_cmd() {
  command -v "$1" >/dev/null 2>&1 || die "Missing required command: $1"
}

require_env() {
  local name="$1"
  [[ -n "${!name:-}" ]] || die "Required environment variable is empty: ${name}"
}

sha256_file() {
  local path="$1"

  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$path" | awk '{print tolower($1)}'
  else
    shasum -a 256 "$path" | awk '{print tolower($1)}'
  fi
}

file_size() {
  wc -c <"$1" | tr -d '[:space:]'
}

remote_tag_commit() {
  local refs
  local direct_ref="refs/tags/${RELEASE_TAG}"
  local peeled_ref="${direct_ref}^{}"

  if ! refs="$(git ls-remote --exit-code origin "$direct_ref" "$peeled_ref")"; then
    die "Release tag '${RELEASE_TAG}' does not exist on origin"
  fi

  awk -v direct="$direct_ref" -v peeled="$peeled_ref" '
    $2 == direct { direct_sha = $1 }
    $2 == peeled { peeled_sha = $1 }
    END {
      if (peeled_sha != "") {
        print tolower(peeled_sha)
      } else {
        print tolower(direct_sha)
      }
    }
  ' <<<"$refs"
}

verify_release_tag() {
  local actual_sha
  actual_sha="$(remote_tag_commit)"
  [[ -n "$actual_sha" ]] || die "Could not resolve release tag '${RELEASE_TAG}' on origin"
  [[ "$actual_sha" == "$source_sha" ]] || \
    die "Release tag '${RELEASE_TAG}' resolves to ${actual_sha}, expected ${source_sha}"
}

get_release_json() {
  local output_path="${work_dir}/release-response.json"
  local error_path="${work_dir}/release-response.err"
  local endpoint="repos/${repository}/releases/tags/${RELEASE_TAG}"

  if gh api --method GET \
    -H "Accept: application/vnd.github+json" \
    -H "X-GitHub-Api-Version: 2022-11-28" \
    "$endpoint" >"$output_path" 2>"$error_path"; then
    jq -e 'type == "object"' "$output_path" >/dev/null || \
      die "GitHub returned an invalid release response for '${RELEASE_TAG}'"
    cat "$output_path"
    return 0
  fi

  if grep -Fq "HTTP 404" "$error_path"; then
    return 4
  fi

  cat "$error_path" >&2
  return 1
}

validate_release_identity() {
  local json="$1"
  local actual_tag release_id draft prerelease

  actual_tag="$(jq -r '.tag_name // ""' <<<"$json")"
  release_id="$(jq -r '.id // ""' <<<"$json")"
  draft="$(jq -r '.draft' <<<"$json")"
  prerelease="$(jq -r '.prerelease' <<<"$json")"

  [[ "$actual_tag" == "$RELEASE_TAG" ]] || \
    die "GitHub returned release tag '${actual_tag}', expected '${RELEASE_TAG}'"
  [[ "$release_id" =~ ^[0-9]+$ ]] || die "Release '${RELEASE_TAG}' has an invalid ID"
  [[ "$draft" == "true" || "$draft" == "false" ]] || \
    die "Release '${RELEASE_TAG}' has an invalid draft state"
  [[ "$prerelease" == "true" || "$prerelease" == "false" ]] || \
    die "Release '${RELEASE_TAG}' has an invalid prerelease state"
  jq -e '.assets | type == "array"' <<<"$json" >/dev/null || \
    die "Release '${RELEASE_TAG}' has an invalid asset list"
}

refresh_release() {
  local status=0
  release_json="$(get_release_json)" || status=$?
  [[ "$status" -eq 0 ]] || die "Release '${RELEASE_TAG}' disappeared or could not be read"
  validate_release_identity "$release_json"
}

release_is_draft() {
  [[ "$(jq -r '.draft' <<<"$release_json")" == "true" ]]
}

assert_not_prerelease() {
  [[ "$(jq -r '.prerelease' <<<"$release_json")" == "false" ]] || \
    die "Release '${RELEASE_TAG}' is a prerelease; refusing stable Helm publication"
}

validate_release_semantics() {
  local actual_title body required_line
  local -a required_lines

  actual_title="$(jq -r '.name // ""' <<<"$release_json")"
  body="$(jq -r '.body // ""' <<<"$release_json")"
  [[ "$actual_title" == "$release_title" ]] || \
    die "Release '${RELEASE_TAG}' has title '${actual_title}', expected '${release_title}'"

  required_lines=(
    "**Release status:** \`${readiness_status}\`"
    "- Chart version: \`${CHART_VERSION}\`"
    "- App version: \`${APP_VERSION}\`"
    "- Source ref: \`${RELEASE_TAG}\`"
    "- Source commit: \`${source_sha}\`"
    "- Package: \`${package_name}\`"
    "- Package SHA-256: \`${expected_sha}\`"
    "- Validation profile: \`${RUNTIME_PROFILE}\`"
    "- Runtime verified: \`${RUNTIME_VERIFIED}\`"
  )
  for required_line in "${required_lines[@]}"; do
    [[ "$body" == *"$required_line"* ]] || \
      die "Release '${RELEASE_TAG}' notes are missing required identity: ${required_line}"
  done

  if [[ "$readiness_status" == "production-ready" ]]; then
    while IFS= read -r required_line; do
      [[ -n "$required_line" ]] || continue
      [[ "$body" == *"$required_line"* ]] || \
        die "Release '${RELEASE_TAG}' notes do not identify tested image: ${required_line}"
    done <<<"$tested_images"
  fi
}

asset_json_by_name() {
  local json="$1"
  local asset_name="$2"
  local matches count

  matches="$(jq -c --arg name "$asset_name" \
    '[.assets[] | select(.name == $name)]' <<<"$json")"
  count="$(jq -r 'length' <<<"$matches")"
  [[ "$count" -le 1 ]] || \
    die "Release '${RELEASE_TAG}' contains multiple assets named '${asset_name}'"

  if [[ "$count" -eq 1 ]]; then
    jq -c '.[0]' <<<"$matches"
  fi
}

verify_remote_asset() {
  local json="$1"
  local asset_name="$2"
  local expected_asset_sha="$3"
  local expected_size="$4"
  local asset_json asset_id asset_state asset_digest asset_size download_path downloaded_sha

  asset_json="$(asset_json_by_name "$json" "$asset_name")"
  [[ -n "$asset_json" ]] || \
    die "Release '${RELEASE_TAG}' is missing required asset '${asset_name}'"

  asset_id="$(jq -r '.id // ""' <<<"$asset_json")"
  asset_state="$(jq -r '.state // ""' <<<"$asset_json")"
  asset_digest="$(jq -r '.digest // ""' <<<"$asset_json")"
  asset_size="$(jq -r '.size // ""' <<<"$asset_json")"

  [[ "$asset_id" =~ ^[0-9]+$ ]] || die "Asset '${asset_name}' has an invalid ID"
  [[ "$asset_state" == "uploaded" ]] || \
    die "Asset '${asset_name}' is in state '${asset_state}', expected 'uploaded'"
  [[ "$asset_size" == "$expected_size" ]] || \
    die "Asset '${asset_name}' has size ${asset_size}, expected ${expected_size}"

  if [[ -n "$asset_digest" ]]; then
    asset_digest="${asset_digest,,}"
    [[ "$asset_digest" == "sha256:${expected_asset_sha}" ]] || \
      die "Asset '${asset_name}' has digest '${asset_digest}', expected 'sha256:${expected_asset_sha}'"
  else
    warn "GitHub did not return a digest for '${asset_name}'; enforcing the downloaded SHA-256"
  fi

  download_path="${work_dir}/download-${asset_id}"
  if ! gh api --method GET \
    -H "Accept: application/octet-stream" \
    -H "X-GitHub-Api-Version: 2022-11-28" \
    "repos/${repository}/releases/assets/${asset_id}" >"$download_path"; then
    die "Could not download release asset '${asset_name}' (ID ${asset_id})"
  fi

  downloaded_sha="$(sha256_file "$download_path")"
  [[ "$downloaded_sha" == "$expected_asset_sha" ]] || \
    die "Downloaded asset '${asset_name}' has SHA-256 ${downloaded_sha}, expected ${expected_asset_sha}"

  verified_asset_id="$asset_id"
  log "Verified '${asset_name}' (asset ID ${asset_id}, SHA-256 ${downloaded_sha})"
}

assert_asset_metadata() {
  local json="$1"
  local asset_name="$2"
  local expected_asset_id="$3"
  local expected_asset_sha="$4"
  local expected_size="$5"
  local asset_json asset_id asset_state asset_digest asset_size

  asset_json="$(asset_json_by_name "$json" "$asset_name")"
  [[ -n "$asset_json" ]] || \
    die "Final release is missing required asset '${asset_name}'"

  asset_id="$(jq -r '.id // ""' <<<"$asset_json")"
  asset_state="$(jq -r '.state // ""' <<<"$asset_json")"
  asset_digest="$(jq -r '.digest // ""' <<<"$asset_json")"
  asset_size="$(jq -r '.size // ""' <<<"$asset_json")"

  [[ "$asset_id" == "$expected_asset_id" ]] || \
    die "Asset '${asset_name}' changed IDs from ${expected_asset_id} to ${asset_id}"
  [[ "$asset_state" == "uploaded" ]] || \
    die "Asset '${asset_name}' changed to state '${asset_state}'"
  [[ "$asset_size" == "$expected_size" ]] || \
    die "Asset '${asset_name}' changed size from ${expected_size} to ${asset_size}"
  if [[ -n "$asset_digest" ]]; then
    [[ "${asset_digest,,}" == "sha256:${expected_asset_sha}" ]] || \
      die "Asset '${asset_name}' changed digest to '${asset_digest}'"
  fi
}

ensure_asset() {
  local local_path="$1"
  local asset_name="$2"
  local expected_asset_sha="$3"
  local expected_size="$4"
  local current_asset upload_status=0

  refresh_release
  assert_not_prerelease
  current_asset="$(asset_json_by_name "$release_json" "$asset_name")"

  if [[ -z "$current_asset" ]]; then
    release_is_draft || \
      die "Published release '${RELEASE_TAG}' is missing '${asset_name}'; refusing to mutate it"

    log "Uploading missing draft asset '${asset_name}' without clobber"
    gh release upload "$RELEASE_TAG" "$local_path" \
      --repo "$repository" || upload_status=$?

    refresh_release
    current_asset="$(asset_json_by_name "$release_json" "$asset_name")"
    if [[ -z "$current_asset" ]]; then
      die "Upload of '${asset_name}' failed with status ${upload_status}, and no asset was created"
    fi
    if [[ "$upload_status" -ne 0 ]]; then
      warn "Upload returned status ${upload_status}; validating the asset now present before continuing"
    fi
  fi

  verify_remote_asset "$release_json" "$asset_name" "$expected_asset_sha" "$expected_size"
}

if [[ $# -ne 0 ]]; then
  die "This script accepts release inputs through environment variables only"
fi

for name in \
  RELEASE_TAG \
  COMPONENT \
  CHART_VERSION \
  APP_VERSION \
  PACKAGE_PATH \
  CHECKSUM_PATH \
  EXPECTED_SHA256 \
  SOURCE_SHA \
  RUNTIME_VERIFIED \
  RUNTIME_PROFILE; do
  require_env "$name"
done

repository="${GITHUB_REPOSITORY:-}"
[[ -n "$repository" ]] || die "GITHUB_REPOSITORY is required by the GitHub Actions runtime"
[[ "$repository" =~ ^[^/]+/[^/]+$ ]] || die "Invalid GITHUB_REPOSITORY: ${repository}"

require_cmd awk
require_cmd cat
require_cmd cmp
require_cmd gh
require_cmd git
require_cmd grep
require_cmd jq
require_cmd mktemp
require_cmd tr
require_cmd wc
if ! command -v sha256sum >/dev/null 2>&1; then
  require_cmd shasum
fi

case "$RUNTIME_VERIFIED" in
  true|false) ;;
  *) die "RUNTIME_VERIFIED must be 'true' or 'false', got '${RUNTIME_VERIFIED}'" ;;
esac

[[ "$EXPECTED_SHA256" =~ ^[0-9A-Fa-f]{64}$ ]] || \
  die "EXPECTED_SHA256 must be a 64-character hexadecimal SHA-256"
[[ "$SOURCE_SHA" =~ ^[0-9A-Fa-f]{40}$ || "$SOURCE_SHA" =~ ^[0-9A-Fa-f]{64}$ ]] || \
  die "SOURCE_SHA must be a full hexadecimal commit ID"
[[ "$COMPONENT" =~ ^[0-9A-Za-z][0-9A-Za-z._-]*$ ]] || \
  die "COMPONENT contains unsupported characters: ${COMPONENT}"
[[ "$CHART_VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || \
  die "Stable Helm releases require an X.Y.Z CHART_VERSION, got '${CHART_VERSION}'"
[[ "$APP_VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || \
  die "Stable Helm releases require an X.Y.Z APP_VERSION, got '${APP_VERSION}'"

for value_name in RELEASE_TAG APP_VERSION RUNTIME_PROFILE; do
  value="${!value_name}"
  [[ "$value" != *$'\n'* && "$value" != *$'\r'* ]] || \
    die "${value_name} must be a single line"
done

tag_prefix="helm/${COMPONENT}/"
[[ "$RELEASE_TAG" == "${tag_prefix}"* ]] || \
  die "RELEASE_TAG '${RELEASE_TAG}' must start with '${tag_prefix}'"
tag_chart_version="${RELEASE_TAG#"$tag_prefix"}"
[[ -n "$tag_chart_version" && "$tag_chart_version" != */* ]] || \
  die "RELEASE_TAG '${RELEASE_TAG}' must have the form helm/<component>/<version>"
[[ "$tag_chart_version" == "$CHART_VERSION" ]] || \
  die "Release tag version '${tag_chart_version}' does not match chart version '${CHART_VERSION}'"

[[ -f "$PACKAGE_PATH" ]] || die "Package does not exist: ${PACKAGE_PATH}"
[[ -f "$CHECKSUM_PATH" ]] || die "Checksum file does not exist: ${CHECKSUM_PATH}"
[[ "$PACKAGE_PATH" != "$CHECKSUM_PATH" ]] || die "Package and checksum paths must be different"

package_name="${PACKAGE_PATH##*/}"
checksum_name="${CHECKSUM_PATH##*/}"
expected_package_name="${COMPONENT}-${CHART_VERSION}.tgz"
[[ "$package_name" == "$expected_package_name" ]] || \
  die "Expected package name '${expected_package_name}', got '${package_name}'"
[[ "$checksum_name" == "SHA256SUMS" ]] || \
  die "Checksum asset must be named 'SHA256SUMS', got '${checksum_name}'"

expected_sha="${EXPECTED_SHA256,,}"
source_sha="${SOURCE_SHA,,}"
local_package_sha="$(sha256_file "$PACKAGE_PATH")"
[[ "$local_package_sha" == "$expected_sha" ]] || \
  die "Local package SHA-256 ${local_package_sha} does not match expected ${expected_sha}"

work_dir="$(mktemp -d)"
cleanup() {
  rm -rf -- "$work_dir"
}
trap cleanup EXIT

expected_checksum_path="${work_dir}/expected-SHA256SUMS"
printf '%s  %s\n' "$expected_sha" "$package_name" >"$expected_checksum_path"
cmp -s "$expected_checksum_path" "$CHECKSUM_PATH" || \
  die "SHA256SUMS must contain exactly '${expected_sha}  ${package_name}'"

local_checksum_sha="$(sha256_file "$CHECKSUM_PATH")"
package_size="$(file_size "$PACKAGE_PATH")"
checksum_size="$(file_size "$CHECKSUM_PATH")"

if [[ "$COMPONENT" == "opensandbox" && "$RUNTIME_VERIFIED" == "true" ]]; then
  readiness_status="production-ready"
else
  readiness_status="package-verified"
fi

tested_images="${TESTED_IMAGES:-}"
if [[ "$readiness_status" == "production-ready" ]]; then
  [[ -n "$tested_images" ]] || die "TESTED_IMAGES is required for a production-ready release"
  [[ "$tested_images" != *$'\r'* && "$tested_images" != *'```'* ]] || \
    die "TESTED_IMAGES contains unsupported release-note content"
fi

release_title="Helm Chart ${COMPONENT} ${CHART_VERSION} (App ${APP_VERSION})"
notes_path="${work_dir}/release-notes.md"
{
  echo "## ${COMPONENT} Helm Chart"
  echo
  echo "**Release status:** \`${readiness_status}\`"
  echo
  echo "- Chart version: \`${CHART_VERSION}\`"
  echo "- App version: \`${APP_VERSION}\`"
  echo "- Source ref: \`${RELEASE_TAG}\`"
  echo "- Source commit: \`${source_sha}\`"
  echo "- Package: \`${package_name}\`"
  echo "- Package SHA-256: \`${expected_sha}\`"
  echo "- Validation profile: \`${RUNTIME_PROFILE}\`"
  echo "- Runtime verified: \`${RUNTIME_VERIFIED}\`"
  if [[ -n "${GITHUB_SERVER_URL:-}" && -n "${GITHUB_RUN_ID:-}" ]]; then
    echo "- Validation run: ${GITHUB_SERVER_URL}/${repository}/actions/runs/${GITHUB_RUN_ID}"
  fi
  echo
  echo "### Verification status"
  echo
  if [[ "$readiness_status" == "production-ready" ]]; then
    echo "The exact umbrella chart package passed the \`${RUNTIME_PROFILE}\` runtime gate before publication."
    echo
    echo "### Tested core image identities"
    echo
    echo '```text'
    printf '%s\n' "$tested_images"
    echo '```'
    echo
    echo "Each line records the requested image reference, registry RepoDigest set, and local image ID pulled for the linux/amd64 Kind gate. Tag-based images are preloaded into Kind; digest-qualified images are pulled directly by the cluster. The live Pods must also report resolved image IDs and pass the runtime checks. The status describes the publication-time validation profile; the chart still uses its documented version tags."
  else
    echo "The chart package and checksum are verified. This release does not claim integrated runtime readiness."
  fi
  echo
  echo "### Installation"
  echo
  if [[ "$COMPONENT" == "opensandbox" || "$COMPONENT" == "opensandbox-server" ]]; then
    echo "Create both the control-plane and workload namespaces, then read the API key without storing it in shell history:"
    echo
    echo '```bash'
    echo 'read -rsp "OpenSandbox API key: " OPENSANDBOX_API_KEY && echo'
    echo 'kubectl create namespace opensandbox-system --dry-run=client -o yaml | kubectl apply -f -'
    echo 'kubectl create namespace opensandbox --dry-run=client -o yaml | kubectl apply -f -'
    echo 'kubectl create secret generic opensandbox-api-key \'
    echo '  --namespace opensandbox-system \'
    echo '  --from-literal="api-key=${OPENSANDBOX_API_KEY}" \'
    echo "  --dry-run=client -o yaml | kubectl apply -f -"
    echo 'unset OPENSANDBOX_API_KEY'
    printf 'helm install %s \\\n' "$COMPONENT"
    printf '  https://github.com/%s/releases/download/%s/%s \\\n' \
      "$repository" "$RELEASE_TAG" "$package_name"
    echo '  --namespace opensandbox-system \'
    if [[ "$COMPONENT" == "opensandbox" ]]; then
      echo "  --set-json 'opensandbox-server.server.env=[{\"name\":\"OPENSANDBOX_SERVER_API_KEY\",\"valueFrom\":{\"secretKeyRef\":{\"name\":\"opensandbox-api-key\",\"key\":\"api-key\"}}}]'"
    else
      echo "  --set-json 'server.env=[{\"name\":\"OPENSANDBOX_SERVER_API_KEY\",\"valueFrom\":{\"secretKeyRef\":{\"name\":\"opensandbox-api-key\",\"key\":\"api-key\"}}}]'"
    fi
    echo '```'
  else
    echo '```bash'
    printf 'helm install %s \\\n' "$COMPONENT"
    printf '  https://github.com/%s/releases/download/%s/%s \\\n' \
      "$repository" "$RELEASE_TAG" "$package_name"
    printf '  --namespace opensandbox-system \\\n'
    echo "  --create-namespace"
    echo '```'
  fi
  echo
  echo "Download \`${package_name}\` and \`SHA256SUMS\`, then run \`sha256sum -c SHA256SUMS\` before installation."
} >"$notes_path"

verify_release_tag

release_json=""
lookup_status=0
release_json="$(get_release_json)" || lookup_status=$?
case "$lookup_status" in
  0)
    validate_release_identity "$release_json"
    log "Found existing release '${RELEASE_TAG}'"
    ;;
  4)
    log "Creating draft release '${RELEASE_TAG}' with verified existing tag"
    create_status=0
    gh release create "$RELEASE_TAG" \
      --repo "$repository" \
      --verify-tag \
      --draft \
      --latest=false \
      --title "$release_title" \
      --notes-file "$notes_path" || create_status=$?

    if [[ "$create_status" -ne 0 ]]; then
      warn "Draft creation returned status ${create_status}; checking for a concurrently created release"
    fi
    refresh_release
    ;;
  *)
    die "Could not query release '${RELEASE_TAG}'"
    ;;
esac

assert_not_prerelease

if release_is_draft; then
  log "Writing verified release notes to draft '${RELEASE_TAG}'"
  edit_status=0
  gh release edit "$RELEASE_TAG" \
    --repo "$repository" \
    --verify-tag \
    --title "$release_title" \
    --notes-file "$notes_path" || edit_status=$?

  refresh_release
  if [[ "$edit_status" -ne 0 ]] && release_is_draft; then
    die "Could not update draft release notes (status ${edit_status})"
  fi
  assert_not_prerelease
fi

validate_release_semantics

ensure_asset "$PACKAGE_PATH" "$package_name" "$expected_sha" "$package_size"
package_asset_id="$verified_asset_id"
ensure_asset "$CHECKSUM_PATH" "$checksum_name" "$local_checksum_sha" "$checksum_size"
checksum_asset_id="$verified_asset_id"

refresh_release
assert_not_prerelease
assert_asset_metadata "$release_json" "$package_name" "$package_asset_id" "$expected_sha" "$package_size"
assert_asset_metadata "$release_json" "$checksum_name" "$checksum_asset_id" "$local_checksum_sha" "$checksum_size"

if release_is_draft; then
  log "Publishing verified draft release '${RELEASE_TAG}'"
  publish_status=0
  gh release edit "$RELEASE_TAG" \
    --repo "$repository" \
    --verify-tag \
    --draft=false \
    --prerelease=false \
    --latest=false || publish_status=$?

  refresh_release
  if [[ "$publish_status" -ne 0 ]] && release_is_draft; then
    die "Could not publish draft release (status ${publish_status})"
  fi
fi

release_is_draft && die "Release '${RELEASE_TAG}' is still a draft after publication"
assert_not_prerelease

# Round-trip both immutable asset identities after publication, then perform one
# final GET so a retry succeeds only for the same published bytes.
verify_remote_asset "$release_json" "$package_name" "$expected_sha" "$package_size"
[[ "$verified_asset_id" == "$package_asset_id" ]] || \
  die "Published package asset ID changed during publication"
verify_remote_asset "$release_json" "$checksum_name" "$local_checksum_sha" "$checksum_size"
[[ "$verified_asset_id" == "$checksum_asset_id" ]] || \
  die "Published checksum asset ID changed during publication"

refresh_release
release_is_draft && die "Final release GET returned draft state"
assert_not_prerelease
assert_asset_metadata "$release_json" "$package_name" "$package_asset_id" "$expected_sha" "$package_size"
assert_asset_metadata "$release_json" "$checksum_name" "$checksum_asset_id" "$local_checksum_sha" "$checksum_size"
validate_release_semantics
verify_release_tag

release_url="$(jq -r '.html_url // ""' <<<"$release_json")"
[[ -n "$release_url" ]] || die "Final release response did not include html_url"

log "Published release verified: ${release_url}"
log "Release status: ${readiness_status}"

if [[ -n "${GITHUB_STEP_SUMMARY:-}" ]]; then
  {
    echo "## Helm release published"
    echo
    echo "- Release: [\`${RELEASE_TAG}\`](${release_url})"
    echo "- Status: \`${readiness_status}\`"
    echo "- Package asset ID: \`${package_asset_id}\`"
    echo "- Package SHA-256: \`${expected_sha}\`"
    echo "- Checksum asset ID: \`${checksum_asset_id}\`"
    echo "- Source commit: \`${source_sha}\`"
    echo "- Validation profile: \`${RUNTIME_PROFILE}\`"
    if [[ "$readiness_status" == "production-ready" ]]; then
      echo "- Tested image identities: recorded in the Release notes"
    fi
  } >>"$GITHUB_STEP_SUMMARY"
fi
