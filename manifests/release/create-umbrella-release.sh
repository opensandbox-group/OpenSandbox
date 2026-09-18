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

# All-in-one umbrella release driver (OSEP-0016).
#
# One version, one commit, one tag family for the whole platform:
#   git tag                     release-X.Y.Z        (annotated, on the BOM commit)
#   Go companion tag (same commit):
#                               sdks/sandbox/go/vX.Y.Z
#   (poolredis merges into the parent module pre-GA: issue #1900)
#   image tags (pushed by CI)   opensandbox/<comp>:release-X.Y.Z
#   packages / chart / CLI      X.Y.Z
#
# Local responsibilities of this script:
#   1. preflight (reachability of the release commit)
#   2. version-consistency scan (jump enablers, OSEP-0016 step 2)
#   3. verify the hand-authored release notes (docs/releases/X.Y.Z.md) exist
#   4. BOM skeleton + notes committed as the BOM commit (C_bom);
#      image digests are pinned by release-umbrella CI (sha256:PENDING until then)
#   5. mint the umbrella tag + the Go companion tag on C_bom
#   6. optional: push, create the GitHub Release
#
# The build-hold-publish fan-out lives in .github/workflows/release-umbrella.yml.

set -euo pipefail

RELEASE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${RELEASE_DIR}/../.." && pwd)"

usage() {
  cat <<'EOF'
Usage:
  manifests/release/create-umbrella-release.sh --version <X.Y.Z[-rc.N]> [options]

Required:
  --version <version>       Umbrella version, e.g. 1.1.0 or 1.1.0-rc.1.

Options:
  --bump-only               One-time prep commit: rewrite every chart version, image
                            reference, and SDK/dependency version to the target version,
                            then commit and exit. Run this on the release branch before
                            writing the release notes. Requires a clean worktree.
  --channel <stable|rc>     Defaults to rc when the version has a suffix, else stable.
  --release-branch <name>   Branch the BOM commit lands on. Default: current branch.
  --skip-remote-check       Skip reachability check against origin (offline use).
  --skip-consistency        Skip the version-consistency scan. Only allowed with --dry-run.
  --scan-only               Run the version-consistency scan and exit (CI job).
  --no-tags                 Do not mint/push tags; only the BOM commit lands (CI pins tags after publish gates).
  --digests-manifest <f>    JSON object {component: "sha256:..."} replacing PENDING digests in the BOM (requires jq).
  --allow-pending           Leave unresolved digests as sha256:PENDING instead of failing.
  --push                    Push the release branch and all three tags to origin.
  --release                 Create/update the GitHub Release (requires gh).
  --dry-run                 Print the full plan without any side effects.
  --help                    Show this help.

Examples:
  manifests/release/create-umbrella-release.sh --version 1.1.0-rc.1 --dry-run
  manifests/release/create-umbrella-release.sh --version 1.1.0 --push --release
EOF
}

log() { echo "[umbrella] $*"; }
warn() { echo "[umbrella][warn] $*" >&2; }
die() { echo "[umbrella][error] $*" >&2; exit 1; }
require_cmd() { command -v "$1" >/dev/null 2>&1 || die "Missing required command: $1"; }

sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | cut -d' ' -f1
  else
    shasum -a 256 "$1" | cut -d' ' -f1
  fi
}

is_semver() {
  [[ "${1#v}" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$ ]]
}

is_numeric_id() { [[ "$1" =~ ^[0-9]+$ ]]; }

semver_compare() {
  local left="${1#v}" right="${2#v}" rest
  left="${left%%+*}" right="${right%%+*}"
  local lmain="${left%%-*}" rmain="${right%%-*}" lpre="" rpre=""
  [[ "$left" == *-* ]] && lpre="${left#*-}"
  [[ "$right" == *-* ]] && rpre="${right#*-}"
  local l1 l2 l3 r1 r2 r3
  IFS='.' read -r l1 l2 l3 <<<"$lmain"
  IFS='.' read -r r1 r2 r3 <<<"$rmain"
  local pair a b
  for pair in "$l1:$r1" "$l2:$r2" "$l3:$r3"; do
    a="${pair%%:*}" b="${pair#*:}"
    if ((10#$a > 10#$b)); then echo 1; return
    elif ((10#$a < 10#$b)); then echo -1; return; fi
  done
  if [[ -z "$lpre" && -z "$rpre" ]]; then echo 0; return
  elif [[ -z "$lpre" ]]; then echo 1; return
  elif [[ -z "$rpre" ]]; then echo -1; return; fi
  local -a lids rids
  IFS='.' read -r -a lids <<<"$lpre"
  IFS='.' read -r -a rids <<<"$rpre"
  local max=${#lids[@]} i li ri
  (( ${#rids[@]} > max )) && max=${#rids[@]}
  for ((i = 0; i < max; i++)); do
    li="${lids[$i]:-}" ri="${rids[$i]:-}"
    if [[ -z "$li" ]]; then echo -1; return
    elif [[ -z "$ri" ]]; then echo 1; return; fi
    if is_numeric_id "$li" && is_numeric_id "$ri"; then
      if ((10#$li > 10#$ri)); then echo 1; return
      elif ((10#$li < 10#$ri)); then echo -1; return; fi
    elif is_numeric_id "$li"; then echo -1; return
    elif is_numeric_id "$ri"; then echo 1; return
    else
      if [[ "$li" > "$ri" ]]; then echo 1; return
      elif [[ "$li" < "$ri" ]]; then echo -1; return; fi
    fi
  done
  echo 0
}

semver_gt() { [[ "$(semver_compare "$1" "$2")" == "1" ]]; }

# ---------------------------------------------------------------------------
# Arguments
# ---------------------------------------------------------------------------

VERSION=""
CHANNEL=""
RELEASE_BRANCH=""
SKIP_REMOTE_CHECK=false
SKIP_CONSISTENCY=false
SCAN_ONLY=false
BUMP_ONLY=false
NO_TAGS=false
ALLOW_PENDING=false
DIGESTS_MANIFEST=""
PUSH=false
CREATE_RELEASE=false
DRY_RUN=false

while [[ $# -gt 0 ]]; do
  case "$1" in
    --version) [[ $# -ge 2 ]] || die "--version requires a value"; VERSION="$2"; shift 2 ;;
    --bump-only) BUMP_ONLY=true; shift ;;
    --channel) [[ $# -ge 2 ]] || die "--channel requires a value"; CHANNEL="$2"; shift 2 ;;
    --release-branch) [[ $# -ge 2 ]] || die "--release-branch requires a value"; RELEASE_BRANCH="$2"; shift 2 ;;
    --skip-remote-check) SKIP_REMOTE_CHECK=true; shift ;;
    --skip-consistency) SKIP_CONSISTENCY=true; shift ;;
    --scan-only) SCAN_ONLY=true; shift ;;
    --no-tags) NO_TAGS=true; shift ;;
    --digests-manifest) [[ $# -ge 2 ]] || die "--digests-manifest requires a value"; DIGESTS_MANIFEST="$2"; shift 2 ;;
    --allow-pending) ALLOW_PENDING=true; shift ;;
    --push) PUSH=true; shift ;;
    --release) CREATE_RELEASE=true; shift ;;
    --dry-run) DRY_RUN=true; shift ;;
    --help|-h) usage; exit 0 ;;
    *) die "Unknown argument: $1" ;;
  esac
done

[[ -n "$VERSION" ]] || { usage; echo; die "--version is required"; }
VERSION="${VERSION#v}"
is_semver "$VERSION" || die "Version '${VERSION}' is not semver (X.Y.Z[-rc.N])"

MAJOR="${VERSION%%.*}"
rest_minor="${VERSION#*.}"
MINOR="${rest_minor%%.*}"
LINE="${MAJOR}.${MINOR}"
NEXT_MAJOR=$((MAJOR + 1))
ESC_VERSION="$(printf '%s' "$VERSION" | sed 's/\./\\./g')"

if [[ -z "$CHANNEL" ]]; then
  if [[ "$VERSION" == *-* ]]; then CHANNEL="rc"; else CHANNEL="stable"; fi
fi
case "$CHANNEL" in
  stable) [[ "$VERSION" == *-* ]] && die "channel=stable requires a version without prerelease suffix" ;;
  rc)     [[ "$VERSION" == *-* ]] || die "channel=rc requires a version with prerelease suffix (e.g. -rc.1)" ;;
  *)      die "--channel must be stable or rc" ;;
esac

if [[ "$SKIP_CONSISTENCY" == true && "$DRY_RUN" != true ]]; then
  die "--skip-consistency is only allowed together with --dry-run"
fi

# Floor rule (OSEP-0016): 1.0.x was consumed by legacy java/sandbox and
# sdks/sandbox/go releases in immutable registries. Never reissue it.
if [[ "$MAJOR" == "1" && "$MINOR" == "0" ]]; then
  die "Version 1.0.* is frozen: consumed by legacy java/sandbox (1.0.19) and sdks/sandbox/go (v1.0.5) releases. Start at 1.1.0 or higher."
fi

UMBRELLA_TAG="release-${VERSION}"
GO_TAG_MAIN="sdks/sandbox/go/v${VERSION}"

require_cmd git
if [[ "$CREATE_RELEASE" == true ]]; then
  require_cmd gh
fi

cd "$REPO_ROOT"
git rev-parse --is-inside-work-tree >/dev/null 2>&1 || die "Must run inside a git repository"

CURRENT_BRANCH="$(git rev-parse --abbrev-ref HEAD)"
[[ -n "$RELEASE_BRANCH" ]] || RELEASE_BRANCH="$CURRENT_BRANCH"
[[ "$CURRENT_BRANCH" == "$RELEASE_BRANCH" ]] || die "Current branch '${CURRENT_BRANCH}' != --release-branch '${RELEASE_BRANCH}'. Check out the release branch first."

# ---------------------------------------------------------------------------
# --bump-only: delegate to the one-shot version bump script
# ---------------------------------------------------------------------------

if [[ "$BUMP_ONLY" == true ]]; then
  exec "${RELEASE_DIR}/bump-versions.sh" --version "$VERSION"
fi

C_BUILD="$(git rev-parse HEAD)"
BUILD_DATE="$(date +%F)"

NOTES_FILE="docs/releases/${VERSION}.md"
if [[ ! -f "$NOTES_FILE" ]]; then
  die "Release notes not found at ${NOTES_FILE}. Write them and commit on ${RELEASE_BRANCH} before triggering the release."
fi
if [[ ! -s "$NOTES_FILE" ]]; then
  die "Release notes at ${NOTES_FILE} are empty."
fi

# ---------------------------------------------------------------------------
# Preflight: release commit reachable from origin/<release-branch>
# ---------------------------------------------------------------------------

if [[ "$SKIP_REMOTE_CHECK" != true ]]; then
  log "Preflight: verifying HEAD ${C_BUILD:0:12} is reachable from origin/${RELEASE_BRANCH}"
  if git ls-remote --heads origin "$RELEASE_BRANCH" | grep -q .; then
    git fetch --force --no-tags origin "+refs/heads/${RELEASE_BRANCH}:refs/remotes/origin/${RELEASE_BRANCH}"
    git merge-base --is-ancestor "$C_BUILD" "origin/${RELEASE_BRANCH}" \
      || die "HEAD ${C_BUILD:0:12} is not reachable from origin/${RELEASE_BRANCH}."
  else
    warn "origin/${RELEASE_BRANCH} does not exist yet; skipping reachability check (branch is pushed by --push)."
  fi
fi

# ---------------------------------------------------------------------------
# Version-consistency scan (scoped, per OSEP-0016 step 2)
# ---------------------------------------------------------------------------

SCAN_ERRORS=0
scan_fail() { echo "  [scan][FAIL] $*" >&2; SCAN_ERRORS=$((SCAN_ERRORS + 1)); }
scan_ok()   { echo "  [scan][ok]   $*"; }

if [[ "$SKIP_CONSISTENCY" != true ]]; then
  log "Version-consistency scan for ${VERSION}:"

  for chart in base controller server ingress-gateway node-agent fast-sandbox opensandbox; do
    file="manifests/charts/${chart}/Chart.yaml"
    if [[ ! -f "$file" ]]; then
      scan_fail "missing ${file}"
      continue
    fi
    ver="$(sed -n 's/^version:[[:space:]]*//p' "$file" | head -1 | tr -d '"'"'"'')"
    if [[ "$ver" == "$VERSION" ]]; then
      scan_ok "${file} version=${ver}"
    else
      scan_fail "${file} version='${ver}' != ${VERSION}"
    fi
    app_ver="$(sed -n 's/^appVersion:[[:space:]]*//p' "$file" | head -1 | tr -d '"'"'"'')"
    if [[ -n "$app_ver" ]]; then
      if [[ "$app_ver" == "$VERSION" ]]; then
        scan_ok "${file} appVersion=${app_ver}"
      else
        scan_fail "${file} appVersion='${app_ver}' != ${VERSION}"
      fi
    fi
  done

  UMBRELLA_CHART="manifests/charts/opensandbox/Chart.yaml"

  bad_deps="$(sed -n '/^dependencies:/,$p' "$UMBRELLA_CHART" | awk -v want="${VERSION}" '
    /^[[:space:]]+- name:/ { name=$NF }
    /^[[:space:]]+version:/ {
      v=$2; gsub(/"/, "", v); gsub(/'\''/, "", v)
      if (v != want) print name " -> " v
    }')"
  if [[ -n "$bad_deps" ]]; then
    while IFS= read -r line; do
      scan_fail "umbrella chart dependency not aligned: ${line} (want ${VERSION})"
    done <<<"$bad_deps"
  else
    scan_ok "umbrella chart dependencies aligned to ${VERSION}"
  fi

  # Image references in chart values: every opensandbox/* image must carry
  # :release-${VERSION}. Empty split-field tags fall back to appVersion.
  bad_images="$(grep -rohE --include='values*.yaml' '(opensandbox|fast-sandbox)/[A-Za-z0-9._/-]+:[^"'"'"'[:space:]]+' manifests/charts 2>/dev/null \
    | grep -vE ":release-${ESC_VERSION}$" | sort -u || true)"
  if [[ -n "$bad_images" ]]; then
    while IFS= read -r ref; do
      scan_fail "chart image ref not on release tag: ${ref}"
    done <<<"$bad_images"
  else
    scan_ok "chart image refs resolve to release-${VERSION}"
  fi

  bad_split_tags="$(grep -rnE --include='values*.yaml' '^[[:space:]]+tag:[[:space:]]*"?(v?[0-9][^"[:space:]]*|dev|latest)' manifests/charts 2>/dev/null \
    | grep -vE "release-${ESC_VERSION}" || true)"
  if [[ -n "$bad_split_tags" ]]; then
    while IFS= read -r line; do
      scan_fail "chart split-field tag not on release tag: ${line}"
    done <<<"$bad_split_tags"
  else
    scan_ok "chart split-field image tags are empty or release-${VERSION}"
  fi

  # JS SDKs, Kotlin/JVM, .NET, Python ranges: SDK package carriers only move
  # at a stable release — rc ships no SDK artifacts, so their versions stay
  # at the last published line and are not checked here.
  if [[ "$CHANNEL" == "stable" ]]; then
  # JS SDKs
  for pkg in sdks/sandbox/javascript sdks/code-interpreter/javascript; do
    f="${pkg}/package.json"
    if [[ -f "$f" ]]; then
      ver="$(sed -n 's/.*"version":[[:space:]]*"\([^"]*\)".*/\1/p' "$f" | head -1)"
      if [[ "$ver" == "$VERSION" ]]; then scan_ok "${f} version=${ver}"
      else scan_fail "${f} version='${ver}' != ${VERSION}"; fi
    else scan_fail "missing ${f}"; fi
  done

  # Kotlin/JVM multi-project
  f="sdks/sandbox/kotlin/gradle.properties"
  ver="$(sed -n 's/^project\.version=//p' "$f" | head -1)"
  if [[ "$ver" == "$VERSION" ]]; then scan_ok "${f} project.version=${ver}"
  else scan_fail "${f} project.version='${ver}' != ${VERSION}"; fi

  # .NET
  f="sdks/Directory.Build.props"
  if [[ -f "$f" ]]; then
    ver="$(sed -n 's:.*<OpenSandboxPackageVersion>\(.*\)</OpenSandboxPackageVersion>.*:\1:p' "$f" | head -1)"
    if [[ "$ver" == "$VERSION" ]]; then scan_ok "${f} OpenSandboxPackageVersion=${ver}"
    else scan_fail "${f} OpenSandboxPackageVersion='${ver}' != ${VERSION}"; fi
    ver_ci="$(sed -n 's:.*<OpenSandboxCodeInterpreterPackageVersion>\(.*\)</OpenSandboxCodeInterpreterPackageVersion>.*:\1:p' "$f" | head -1)"
    if [[ "$ver_ci" == "$VERSION" ]]; then scan_ok "${f} OpenSandboxCodeInterpreterPackageVersion=${ver_ci}"
    else scan_fail "${f} OpenSandboxCodeInterpreterPackageVersion='${ver_ci}' != ${VERSION}"; fi
    want_range="[${VERSION},${NEXT_MAJOR}.0.0)"
    range="$(sed -n 's:.*<OpenSandboxDependencyVersionRange>\(.*\)</OpenSandboxDependencyVersionRange>.*:\1:p' "$f" | head -1)"
    if [[ "$range" == "$want_range" ]]; then scan_ok "${f} dependency range=${range}"
    else scan_fail "${f} dependency range='${range}' != ${want_range}"; fi
  else
    scan_fail "missing ${f}"
  fi

  # Python inter-package ranges (cli + sdks)
  for f in cli/pyproject.toml sdks/code-interpreter/python/pyproject.toml sdks/mcp/sandbox/python/pyproject.toml; do
    if [[ -f "$f" ]]; then
      if grep -Eq "\"opensandbox>=${ESC_VERSION},<${NEXT_MAJOR}\\.0\\.0\"" "$f"; then
        scan_ok "${f} opensandbox range >=${VERSION},<${NEXT_MAJOR}.0.0"
      else
        scan_fail "${f} opensandbox range must be 'opensandbox>=${VERSION},<${NEXT_MAJOR}.0.0'"
      fi
    else scan_fail "missing ${f}"; fi
  done
  else
    scan_ok "channel=rc: SDK package carriers skipped (they bump at the stable release)"
  fi

  # hatch-vcs tag patterns: umbrella-only; legacy per-component regexes are frozen
  # (both tag_regex and git_describe_command pin the namespace)
  bad_regex="$(grep -rl --include='pyproject.toml' 'tag_regex' server cli sdks 2>/dev/null \
    | while IFS= read -r pf; do
        grep -Eq '(tag_regex|git_describe_command).*(cli/v|server/v|python/|js/|java/|csharp/)' "$pf" 2>/dev/null && echo "$pf"
      done || true)"
  if [[ -n "$bad_regex" ]]; then
    while IFS= read -r pf; do
      scan_fail "${pf} still matches a legacy tag namespace (must use ^release-...)"
    done <<<"$bad_regex"
  else
    scan_ok "all hatch-vcs tag_regex values use the umbrella pattern"
  fi

  if (( SCAN_ERRORS > 0 )); then
    die "Version-consistency scan failed with ${SCAN_ERRORS} error(s). Fix the listed files on the release branch before releasing."
  fi
fi

if [[ "$SCAN_ONLY" == true ]]; then
  log "Scan-only mode: consistency scan passed; exiting before notes/BOM/tags."
  exit 0
fi

# ---------------------------------------------------------------------------
# Previous umbrella tag (monotonic version check)
# ---------------------------------------------------------------------------

PREVIOUS_TAG="$(git for-each-ref --sort=-version:refname --format='%(refname:strip=2)' 'refs/tags/release-*' | head -n1)"
if [[ -n "$PREVIOUS_TAG" ]]; then
  prev_ver="${PREVIOUS_TAG#release-}"
  if is_semver "$prev_ver" && ! semver_gt "$VERSION" "$prev_ver"; then
    die "Version '${VERSION}' is not greater than previous umbrella '${prev_ver}'"
  fi
fi

# ---------------------------------------------------------------------------
# BOM skeleton
# ---------------------------------------------------------------------------

RELEASES_DIR="docs/releases"
BOM_FILE="${RELEASES_DIR}/${VERSION}.yaml"
NOTES_OUT_FILE="${RELEASES_DIR}/${VERSION}.md"
spec_lifecycle_sha="$(sha256_of specs/sandbox-lifecycle.yml)"
spec_execd_sha="$(sha256_of specs/execd-api.yaml)"

BOM_FILE_TMP="$(mktemp -t opensandbox-bom.XXXXXX.yaml)"
cat >"$BOM_FILE_TMP" <<EOF
# Umbrella release BOM (OSEP-0016) — generated by create-umbrella-release.sh.
# The BOM is authoritative for image digests. Entries marked PENDING are
# pinned by the release-umbrella workflow during the build-hold-publish fan-out.
apiVersion: opensandbox.io/v1
kind: UmbrellaRelease
metadata:
  version: ${VERSION}
  line: "${LINE}"
  channel: ${CHANNEL}
  releaseDate: "${BUILD_DATE}"
  gitCommit: ${C_BUILD}

compatibility:
  kubernetes: { minVersion: v1.24, maxVersion: v1.34 }
  crd:
    - { group: sandbox.opensandbox.io, versions: [v1alpha1] }

images:
  server:         { image: docker.io/opensandbox/server,          tag: release-${VERSION}, digest: sha256:PENDING }
  execd:          { image: docker.io/opensandbox/execd,           tag: release-${VERSION}, digest: sha256:PENDING }
  ingress:        { image: docker.io/opensandbox/ingress,         tag: release-${VERSION}, digest: sha256:PENDING }
  egress:         { image: docker.io/opensandbox/egress,          tag: release-${VERSION}, digest: sha256:PENDING }
  imageCommitter: { image: docker.io/opensandbox/image-committer, tag: release-${VERSION}, digest: sha256:PENDING }
  controller:     { image: docker.io/opensandbox/controller,      tag: release-${VERSION}, digest: sha256:PENDING }
  taskExecutor:   { image: docker.io/opensandbox/task-executor,   tag: release-${VERSION}, digest: sha256:PENDING }
  nodeAgent:      { image: docker.io/opensandbox/nodeagent,       tag: release-${VERSION}, digest: sha256:PENDING }
  # fast-sandbox runtime family (fsb- prefix, linux/amd64; same mirror set)
  fsbController:            { image: docker.io/opensandbox/fsb-controller,            tag: release-${VERSION}, digest: sha256:PENDING }
  fsbFastlet:               { image: docker.io/opensandbox/fsb-fastlet,               tag: release-${VERSION}, digest: sha256:PENDING }
  fsbFastletProxy:          { image: docker.io/opensandbox/fsb-fastlet-proxy,         tag: release-${VERSION}, digest: sha256:PENDING }
  fsbJanitor:               { image: docker.io/opensandbox/fsb-janitor,               tag: release-${VERSION}, digest: sha256:PENDING }
  fsbFirecrackerRuntime:    { image: docker.io/opensandbox/fsb-firecracker-runtime,   tag: release-${VERSION}, digest: sha256:PENDING }
  fsbSandboxtemplateBuilder:{ image: docker.io/opensandbox/fsb-sandboxtemplate-builder, tag: release-${VERSION}, digest: sha256:PENDING }

helm:   { chart: opensandbox, version: "${VERSION}", appVersion: "${VERSION}" }  # in-repo at the tag; not published
server: { pypi: opensandbox-server==${VERSION} }
cli:    { pypi: opensandbox-cli==${VERSION} }
sdks:
  - { product: sandbox,            language: python,     package: "pypi:opensandbox==${VERSION}" }
  - { product: code-interpreter,   language: python,     package: "pypi:opensandbox-code-interpreter==${VERSION}" }
  - { product: mcp/sandbox,        language: python,     package: "pypi:opensandbox-mcp==${VERSION}" }
  - { product: sandbox,            language: javascript, package: "npm:@alibaba-group/opensandbox@${VERSION}" }
  - { product: code-interpreter,   language: javascript, package: "npm:@alibaba-group/opensandbox-code-interpreter@${VERSION}" }
  - { product: sandbox,            language: kotlin,     package: "maven:com.alibaba.opensandbox:sandbox:${VERSION}" }
  - { product: sandbox-api,        language: kotlin,     package: "maven:com.alibaba.opensandbox:sandbox-api:${VERSION}" }
  - { product: sandbox-bom,        language: kotlin,     package: "maven:com.alibaba.opensandbox:sandbox-bom:${VERSION}" }
  - { product: sandbox-pool-redis, language: kotlin,     package: "maven:com.alibaba.opensandbox:sandbox-pool-redis:${VERSION}" }
  - { product: code-interpreter,   language: kotlin,     package: "maven:com.alibaba.opensandbox:code-interpreter:${VERSION}" }
  - { product: sandbox,            language: csharp,     package: "nuget:Alibaba.OpenSandbox ${VERSION}" }
  - { product: code-interpreter,   language: csharp,     package: "nuget:Alibaba.OpenSandbox.CodeInterpreter ${VERSION}" }
  - { product: sandbox,            language: go,         package: "go:github.com/alibaba/OpenSandbox/sdks/sandbox/go v${VERSION}" }
  - { product: sandbox-pool-redis, language: go,         package: "go:github.com/alibaba/OpenSandbox/sdks/sandbox/go/poolredis v${VERSION}" }

specs:
  - { path: specs/sandbox-lifecycle.yml, sha256: ${spec_lifecycle_sha} }
  - { path: specs/execd-api.yaml,        sha256: ${spec_execd_sha} }
EOF

if [[ -n "$DIGESTS_MANIFEST" ]]; then
  require_cmd jq
  while IFS=$'\t' read -r comp digest; do
    [[ -n "$comp" ]] || continue
    esc_comp="$(printf '%s' "$comp" | sed 's/[.[\*^$/]/\\&/g')"
    esc_digest="$(printf '%s' "$digest" | sed 's/[.&/]/\\&/g')"
    if [[ "$comp" == */* ]]; then
      sed -i.bak "/docker.io\/${esc_comp},/s|digest: sha256:PENDING|digest: ${esc_digest}|" "$BOM_FILE_TMP"
    else
      sed -i.bak "/docker.io\/opensandbox\/${esc_comp},/s|digest: sha256:PENDING|digest: ${esc_digest}|" "$BOM_FILE_TMP"
    fi
    rm -f "${BOM_FILE_TMP}.bak"
  done < <(jq -r 'to_entries[] | "\(.key)\t\(.value)"' "$DIGESTS_MANIFEST")
  if grep -q 'sha256:PENDING' "$BOM_FILE_TMP"; then
    if [[ "$ALLOW_PENDING" != true ]]; then
      die "BOM still contains sha256:PENDING entries not covered by --digests-manifest."
    fi
    warn "BOM contains PENDING digests (allowed by --allow-pending)."
  else
    log "All image digests pinned from ${DIGESTS_MANIFEST}."
  fi
fi

# ---------------------------------------------------------------------------
# Plan output / dry-run
# ---------------------------------------------------------------------------

log "Umbrella version : ${VERSION} (channel=${CHANNEL})"
log "Release branch   : ${RELEASE_BRANCH}"
log "Build commit     : ${C_BUILD}"
log "Previous tag     : ${PREVIOUS_TAG:-<none> (first umbrella)}"
log "Release notes    : ${NOTES_FILE} (hand-authored, must be pre-committed)"
log "Tags to mint     : ${UMBRELLA_TAG}, ${GO_TAG_MAIN} (on the BOM commit)"
log "BOM + notes      : ${BOM_FILE}, ${NOTES_FILE}"

if [[ "$DRY_RUN" == true ]]; then
  log "Dry run enabled. No commit, tag, push, or release will be performed."
  echo
  log "Release notes preview (from ${NOTES_FILE}):"
  echo "------------------------------------------------------------"
  cat "$NOTES_FILE"
  echo "------------------------------------------------------------"
  echo
  log "BOM preview:"
  echo "------------------------------------------------------------"
  cat "$BOM_FILE_TMP"
  echo "------------------------------------------------------------"
  rm -f "$BOM_FILE_TMP"
  exit 0
fi

# ---------------------------------------------------------------------------
# BOM commit (C_bom) + tags
# ---------------------------------------------------------------------------

mkdir -p "$RELEASES_DIR"
cp "$BOM_FILE_TMP" "$BOM_FILE"
git add "$BOM_FILE" "$NOTES_FILE"
if git diff --cached --quiet; then
  warn "BOM commit is empty; reusing HEAD as C_bom."
  C_BOM="$C_BUILD"
else
  git commit -m "release(opensandbox): pin BOM for ${VERSION}"
  C_BOM="$(git rev-parse HEAD)"
fi
log "BOM commit (C_bom): ${C_BOM}"

mint_tag() {
  local tag="$1"
  if git rev-parse -q --verify "refs/tags/${tag}" >/dev/null; then
    local existing
    existing="$(git rev-parse "${tag}^{commit}")"
    if [[ "$existing" != "$C_BOM" ]]; then
      die "Tag '${tag}' already exists at ${existing}, but this release's BOM commit is ${C_BOM}."
    fi
    warn "Tag '${tag}' already exists on C_bom. Reusing."
  else
    git tag -a "$tag" -m "release: OpenSandbox ${VERSION}"
    log "Created annotated tag: ${tag}"
  fi
}

if [[ "$NO_TAGS" == true ]]; then
  warn "--no-tags: skipping tag minting (CI pins tags after publish gates pass)."
fi
if [[ "$NO_TAGS" != true ]]; then
  mint_tag "$UMBRELLA_TAG"
  mint_tag "$GO_TAG_MAIN"
fi

if [[ "$PUSH" == true ]]; then
  git push origin "$RELEASE_BRANCH"
  if [[ "$NO_TAGS" != true ]]; then
    git push origin "$UMBRELLA_TAG" "$GO_TAG_MAIN"
  fi
  log "Pushed ${RELEASE_BRANCH}${NO_TAGS:+ (branch only, tags skipped)}."
else
  warn "Not pushed. Use --push to push the branch${NO_TAGS:+, or drop --no-tags to also push tags}."
fi

# ---------------------------------------------------------------------------
# GitHub Release
# ---------------------------------------------------------------------------

if [[ "$CREATE_RELEASE" == true ]]; then
  [[ "$NO_TAGS" != true ]] || die "--release requires tags; drop --no-tags."
  TITLE="OpenSandbox ${VERSION}"
  if gh release view "$UMBRELLA_TAG" >/dev/null 2>&1; then
    gh release edit "$UMBRELLA_TAG" --title "$TITLE" --notes-file "$NOTES_FILE"
    gh release upload "$UMBRELLA_TAG" "$BOM_FILE_TMP" --clobber
    log "Updated GitHub Release: ${UMBRELLA_TAG}"
  else
    if [[ "$PUSH" != true ]]; then
      die "--release requires the tags on origin; pass --push first."
    fi
    gh release create "$UMBRELLA_TAG" \
      --verify-tag \
      --title "$TITLE" \
      --notes-file "$NOTES_FILE" \
      "$BOM_FILE_TMP"
    log "Created GitHub Release: ${UMBRELLA_TAG}"
  fi
fi

rm -f "$BOM_FILE_TMP"
log "Umbrella release ${VERSION} prepared. CI fan-out (release-umbrella.yml) pins image digests and publishes artifacts."
