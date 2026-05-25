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

usage() {
  cat <<'EOF'
Usage:
  scripts/release/create-release.sh --target <target> --version <version> [options]

Required:
  --target <target>     Release target key, e.g.:
                        js/sandbox
                        js/code-interpreter
                        python/sandbox
                        python/code-interpreter
                        python/mcp/sandbox
                        java/sandbox
                        java/code-interpreter
                        csharp/sandbox
                        csharp/code-interpreter
                        sdks/sandbox/go
                        cli
                        server
                        docker/execd
                        docker/code-interpreter
                        docker/ingress
                        docker/egress
                        k8s/controller
                        k8s/task-executor
                        helm/opensandbox
                        helm
  --version <version>   Release version string. For v-prefixed targets, script normalizes
                        to tags like <target>/v<version> automatically.

Options:
  --from-tag <tag>      Override previous release tag boundary.
  --path <path>         Add extra path filter (repeatable).
  --no-path-filter      Disable default target path filters.
  --push                Push tag to origin (required to trigger tag-based workflows).
  --dry-run             Print computed results without creating tag/release.
  --initial-release     Allow release without previous tag (uses full history).
  --sign-tag            Create a cryptographically signed git tag using the local
                        git signing configuration. Defaults to an annotated tag.
  --help                Show this help.

Examples:
  scripts/release/create-release.sh --target js/sandbox --version 1.0.5 --dry-run
  scripts/release/create-release.sh --target server --version 0.2.0 --push
  scripts/release/create-release.sh --target docker/execd --version v0.3.0 --push
EOF
}

log() {
  echo "[release] $*"
}

warn() {
  echo "[release][warn] $*" >&2
}

die() {
  echo "[release][error] $*" >&2
  exit 1
}

require_cmd() {
  local cmd="$1"
  command -v "$cmd" >/dev/null 2>&1 || die "Missing required command: $cmd"
}

remote_tag_commit() {
  local tag="$1"
  local commit

  commit="$(git ls-remote origin "refs/tags/${tag}^{}" | awk 'NR == 1 { print $1 }')"
  if [[ -z "$commit" ]]; then
    commit="$(git ls-remote origin "refs/tags/${tag}" | awk 'NR == 1 { print $1 }')"
  fi

  printf '%s' "$commit"
}

is_semver_like() {
  local version="${1#v}"
  [[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$ ]]
}

is_numeric_id() {
  [[ "$1" =~ ^[0-9]+$ ]]
}

semver_compare() {
  local left="${1#v}"
  local right="${2#v}"

  # Build metadata must not affect precedence.
  left="${left%%+*}"
  right="${right%%+*}"

  local left_main="${left%%-*}"
  local right_main="${right%%-*}"
  local left_pre=""
  local right_pre=""
  if [[ "$left" == *-* ]]; then
    left_pre="${left#*-}"
  fi
  if [[ "$right" == *-* ]]; then
    right_pre="${right#*-}"
  fi

  local l1 l2 l3 r1 r2 r3
  IFS='.' read -r l1 l2 l3 <<<"$left_main"
  IFS='.' read -r r1 r2 r3 <<<"$right_main"

  if ((10#$l1 > 10#$r1)); then
    echo 1
    return
  elif ((10#$l1 < 10#$r1)); then
    echo -1
    return
  fi
  if ((10#$l2 > 10#$r2)); then
    echo 1
    return
  elif ((10#$l2 < 10#$r2)); then
    echo -1
    return
  fi
  if ((10#$l3 > 10#$r3)); then
    echo 1
    return
  elif ((10#$l3 < 10#$r3)); then
    echo -1
    return
  fi

  # A version without prerelease has higher precedence.
  if [[ -z "$left_pre" && -z "$right_pre" ]]; then
    echo 0
    return
  elif [[ -z "$left_pre" ]]; then
    echo 1
    return
  elif [[ -z "$right_pre" ]]; then
    echo -1
    return
  fi

  local -a left_ids right_ids
  local left_len right_len max_len i
  IFS='.' read -r -a left_ids <<<"$left_pre"
  IFS='.' read -r -a right_ids <<<"$right_pre"
  left_len="${#left_ids[@]}"
  right_len="${#right_ids[@]}"
  if ((left_len > right_len)); then
    max_len="$left_len"
  else
    max_len="$right_len"
  fi

  local li ri
  for ((i = 0; i < max_len; i++)); do
    li="${left_ids[$i]:-}"
    ri="${right_ids[$i]:-}"

    if [[ -z "$li" && -n "$ri" ]]; then
      echo -1
      return
    elif [[ -n "$li" && -z "$ri" ]]; then
      echo 1
      return
    fi

    if is_numeric_id "$li" && is_numeric_id "$ri"; then
      if ((10#$li > 10#$ri)); then
        echo 1
        return
      elif ((10#$li < 10#$ri)); then
        echo -1
        return
      fi
    elif is_numeric_id "$li" && ! is_numeric_id "$ri"; then
      echo -1
      return
    elif ! is_numeric_id "$li" && is_numeric_id "$ri"; then
      echo 1
      return
    else
      if [[ "$li" > "$ri" ]]; then
        echo 1
        return
      elif [[ "$li" < "$ri" ]]; then
        echo -1
        return
      fi
    fi
  done

  echo 0
}

semver_lt() {
  [[ "$(semver_compare "$1" "$2")" == "-1" ]]
}

semver_gt() {
  [[ "$(semver_compare "$1" "$2")" == "1" ]]
}

normalize_handle() {
  local author="$1"
  local email="$2"
  local candidate=""

  if [[ "$email" =~ ^([0-9]+\+)?([^@]+)@users\.noreply\.github\.com$ ]]; then
    candidate="${BASH_REMATCH[2]}"
  else
    candidate="${email%@*}"
  fi

  candidate="$(echo "$candidate" | tr -cd '[:alnum:]_.-')"
  if [[ -n "$candidate" ]]; then
    printf '@%s' "$candidate"
  else
    printf '%s' "$author"
  fi
}

TARGET=""
VERSION=""
FROM_TAG=""
DRY_RUN=false
PUSH_TAG=false
INITIAL_RELEASE=false
NO_PATH_FILTER=false
SIGN_TAG=false
CUSTOM_PATH_FILTERS=()

while [[ $# -gt 0 ]]; do
  case "$1" in
    --target)
      [[ $# -ge 2 ]] || die "--target requires a value"
      TARGET="$2"
      shift 2
      ;;
    --version)
      [[ $# -ge 2 ]] || die "--version requires a value"
      VERSION="$2"
      shift 2
      ;;
    --from-tag)
      [[ $# -ge 2 ]] || die "--from-tag requires a value"
      FROM_TAG="$2"
      shift 2
      ;;
    --path)
      [[ $# -ge 2 ]] || die "--path requires a value"
      CUSTOM_PATH_FILTERS+=("$2")
      shift 2
      ;;
    --no-path-filter)
      NO_PATH_FILTER=true
      shift
      ;;
    --dry-run)
      DRY_RUN=true
      shift
      ;;
    --push)
      PUSH_TAG=true
      shift
      ;;
    --sign-tag)
      SIGN_TAG=true
      shift
      ;;
    --initial-release)
      INITIAL_RELEASE=true
      shift
      ;;
    --help|-h)
      usage
      exit 0
      ;;
    *)
      die "Unknown argument: $1"
      ;;
  esac
done

[[ -n "$TARGET" ]] || die "--target is required"
[[ -n "$VERSION" ]] || die "--version is required"

require_cmd git
require_cmd rg
if [[ "$DRY_RUN" != true ]]; then
  require_cmd gh
fi

git rev-parse --is-inside-work-tree >/dev/null 2>&1 || die "Must run inside a git repository"

TAG_NEEDS_V=false
DISPLAY_NAME=""
WORKFLOW_HINT=""
TARGET_PATH_FILTERS=()

# Registry: maps all publishable targets to tag conventions.
case "$TARGET" in
  js/sandbox)
    TAG_NEEDS_V=true
    DISPLAY_NAME="JavaScript Sandbox SDK"
    WORKFLOW_HINT=".github/workflows/publish-js-sdks.yml"
    TARGET_PATH_FILTERS=("sdks/sandbox/javascript" "specs/sandbox-lifecycle.yml")
    ;;
  js/code-interpreter)
    TAG_NEEDS_V=true
    DISPLAY_NAME="JavaScript Code Interpreter SDK"
    WORKFLOW_HINT=".github/workflows/publish-js-sdks.yml"
    TARGET_PATH_FILTERS=("sdks/code-interpreter/javascript" "specs/execd-api.yaml")
    ;;
  python/sandbox)
    TAG_NEEDS_V=true
    DISPLAY_NAME="Python Sandbox SDK"
    WORKFLOW_HINT=".github/workflows/publish-python-sdks.yml"
    TARGET_PATH_FILTERS=("sdks/sandbox/python" "specs/sandbox-lifecycle.yml")
    ;;
  python/code-interpreter)
    TAG_NEEDS_V=true
    DISPLAY_NAME="Python Code Interpreter SDK"
    WORKFLOW_HINT=".github/workflows/publish-python-sdks.yml"
    TARGET_PATH_FILTERS=("sdks/code-interpreter/python" "specs/execd-api.yaml")
    ;;
  python/mcp/sandbox)
    TAG_NEEDS_V=true
    DISPLAY_NAME="Python MCP Sandbox SDK"
    WORKFLOW_HINT=".github/workflows/publish-python-sdks.yml"
    TARGET_PATH_FILTERS=("sdks/mcp/sandbox/python" "specs/sandbox-lifecycle.yml")
    ;;
  java/sandbox)
    TAG_NEEDS_V=true
    DISPLAY_NAME="Java Sandbox SDK"
    WORKFLOW_HINT=".github/workflows/publish-java-sdks.yml"
    TARGET_PATH_FILTERS=("sdks/sandbox/kotlin" "specs/sandbox-lifecycle.yml")
    ;;
  java/code-interpreter)
    TAG_NEEDS_V=true
    DISPLAY_NAME="Java Code Interpreter SDK"
    WORKFLOW_HINT=".github/workflows/publish-java-sdks.yml"
    TARGET_PATH_FILTERS=("sdks/code-interpreter/kotlin" "specs/execd-api.yaml")
    ;;
  csharp/sandbox)
    TAG_NEEDS_V=true
    DISPLAY_NAME="CSharp Sandbox SDK"
    WORKFLOW_HINT=".github/workflows/publish-csharp-sdks.yml"
    TARGET_PATH_FILTERS=("sdks/sandbox/csharp" "specs/sandbox-lifecycle.yml")
    ;;
  csharp/code-interpreter)
    TAG_NEEDS_V=true
    DISPLAY_NAME="CSharp Code Interpreter SDK"
    WORKFLOW_HINT=".github/workflows/publish-csharp-sdks.yml"
    TARGET_PATH_FILTERS=("sdks/code-interpreter/csharp" "specs/execd-api.yaml")
    ;;
  sdks/sandbox/go|go/sandbox)
    TARGET="sdks/sandbox/go"
    TAG_NEEDS_V=true
    DISPLAY_NAME="Go Sandbox SDK"
    WORKFLOW_HINT=".github/workflows/release-generic.yml"
    TARGET_PATH_FILTERS=("sdks/sandbox/go" "specs/sandbox-lifecycle.yml")
    ;;
  cli)
    TAG_NEEDS_V=true
    DISPLAY_NAME="OpenSandbox CLI"
    WORKFLOW_HINT=".github/workflows/publish-cli.yml"
    TARGET_PATH_FILTERS=("cli")
    ;;
  server)
    TAG_NEEDS_V=true
    DISPLAY_NAME="OpenSandbox Server"
    WORKFLOW_HINT=".github/workflows/publish-server.yml"
    TARGET_PATH_FILTERS=("server" "specs/sandbox-lifecycle.yml")
    ;;
  docker/execd)
    DISPLAY_NAME="Component Image execd"
    WORKFLOW_HINT=".github/workflows/publish-components.yml"
    TARGET_PATH_FILTERS=("components/execd")
    ;;
  docker/code-interpreter)
    DISPLAY_NAME="Component Image code-interpreter"
    WORKFLOW_HINT=".github/workflows/publish-components.yml"
    TARGET_PATH_FILTERS=("sandboxes/code-interpreter")
    ;;
  docker/ingress)
    DISPLAY_NAME="Component Image ingress"
    WORKFLOW_HINT=".github/workflows/publish-components.yml"
    TARGET_PATH_FILTERS=("components/ingress")
    ;;
  docker/egress)
    DISPLAY_NAME="Component Image egress"
    WORKFLOW_HINT=".github/workflows/publish-components.yml"
    TARGET_PATH_FILTERS=("components/egress")
    ;;
  k8s/controller)
    DISPLAY_NAME="K8s Component controller"
    WORKFLOW_HINT=".github/workflows/publish-components.yml"
    TARGET_PATH_FILTERS=("kubernetes")
    ;;
  k8s/task-executor)
    DISPLAY_NAME="K8s Component task-executor"
    WORKFLOW_HINT=".github/workflows/publish-components.yml"
    TARGET_PATH_FILTERS=("kubernetes")
    ;;
  helm|helm/opensandbox)
    TARGET="helm/opensandbox"
    DISPLAY_NAME="Helm opensandbox"
    WORKFLOW_HINT=".github/workflows/publish-helm-chart.yml"
    TARGET_PATH_FILTERS=("kubernetes/charts/opensandbox")
    ;;
  *)
    die "Unsupported target '$TARGET'. Run with --help for supported target list."
    ;;
esac

VERSION_NO_V="${VERSION#v}"
if [[ "$TAG_NEEDS_V" == true ]]; then
  NEW_TAG="${TARGET}/v${VERSION_NO_V}"
  TAG_PREFIX="${TARGET}/v"
  VERSION_LABEL="v${VERSION_NO_V}"
else
  NEW_TAG="${TARGET}/${VERSION}"
  TAG_PREFIX="${TARGET}/"
  VERSION_LABEL="$VERSION"
fi

if [[ -n "${GITHUB_OUTPUT:-}" ]]; then
  {
    echo "tag=${NEW_TAG}"
    echo "display_name=${DISPLAY_NAME}"
    echo "version_label=${VERSION_LABEL}"
  } >>"${GITHUB_OUTPUT}"
fi

if [[ -n "$FROM_TAG" ]] && ! git rev-parse -q --verify "refs/tags/${FROM_TAG}" >/dev/null; then
  die "--from-tag '${FROM_TAG}' does not exist"
fi

resolve_previous_tag() {
  local explicit="$1"
  if [[ -n "$explicit" ]]; then
    printf '%s' "$explicit"
    return 0
  fi

  local tags_output
  tags_output="$(git tag --list "${TAG_PREFIX}*" | rg -v "^${NEW_TAG}$" || true)"
  if [[ -z "$tags_output" ]]; then
    printf ''
    return 0
  fi

  if is_semver_like "$VERSION_NO_V"; then
    local semver_latest_version=""
    local semver_latest_tag=""
    local tag suffix
    while IFS= read -r tag; do
      [[ -n "$tag" ]] || continue
      suffix="${tag#${TAG_PREFIX}}"
      if is_semver_like "$suffix"; then
        if [[ -z "$semver_latest_version" ]] || semver_gt "$suffix" "$semver_latest_version"; then
          semver_latest_version="$suffix"
          semver_latest_tag="$tag"
        fi
      fi
    done <<<"$tags_output"

    if [[ -n "$semver_latest_tag" ]]; then
      printf '%s' "$semver_latest_tag"
      return 0
    fi
  fi

  git for-each-ref --sort=-creatordate --format='%(refname:strip=2)' "refs/tags/${TAG_PREFIX}*" \
    | rg -v "^${NEW_TAG}$" \
    | head -n1
}

PREVIOUS_TAG="$(resolve_previous_tag "$FROM_TAG")"

if [[ -z "$PREVIOUS_TAG" && "$INITIAL_RELEASE" != true ]]; then
  die "No previous tag found for target '${TARGET}'. Pass --from-tag or --initial-release."
fi

if [[ -n "$PREVIOUS_TAG" ]] && is_semver_like "$VERSION_NO_V"; then
  PREVIOUS_SUFFIX="${PREVIOUS_TAG#${TAG_PREFIX}}"
  if is_semver_like "$PREVIOUS_SUFFIX" && ! semver_gt "$VERSION_NO_V" "$PREVIOUS_SUFFIX"; then
    die "Version '${VERSION_LABEL}' is not greater than previous '${PREVIOUS_SUFFIX}'"
  fi
fi

log "Target: ${TARGET}"
log "Workflow: ${WORKFLOW_HINT}"
log "New tag: ${NEW_TAG}"
if [[ -n "$PREVIOUS_TAG" ]]; then
  log "Previous tag: ${PREVIOUS_TAG}"
else
  log "Previous tag: <none> (initial release mode)"
fi

if [[ -n "$PREVIOUS_TAG" ]]; then
  LOG_RANGE="${PREVIOUS_TAG}..HEAD"
else
  LOG_RANGE="HEAD"
fi

LOG_PATH_FILTERS=()
if [[ "$NO_PATH_FILTER" != true ]]; then
  LOG_PATH_FILTERS=("${TARGET_PATH_FILTERS[@]}")
fi
if [[ "${#CUSTOM_PATH_FILTERS[@]}" -gt 0 ]]; then
  LOG_PATH_FILTERS+=("${CUSTOM_PATH_FILTERS[@]}")
fi

if [[ "${#LOG_PATH_FILTERS[@]}" -gt 0 ]]; then
  log "Path filters: $(printf '%s ' "${LOG_PATH_FILTERS[@]}" | sed 's/[[:space:]]*$//')"
else
  log "Path filters: <none> (whole range)"
fi

FEATURES=()
BUG_FIXES=()
BREAKING_CHANGES=()
MISC_ITEMS=()
CONTRIBUTORS=()
CONTRIBUTORS_INDEX=$'\n'

add_contributor() {
  local author="$1"
  local email="$2"
  local handle
  handle="$(normalize_handle "$author" "$email")"
  if [[ "$CONTRIBUTORS_INDEX" != *$'\n'"$handle"$'\n'* ]]; then
    CONTRIBUTORS_INDEX+="${handle}"$'\n'
    CONTRIBUTORS+=("$handle")
  fi
}

format_entry() {
  local subject="$1"
  local body="$2"
  local text="$subject"

  if [[ ! "$subject" =~ \(\#[0-9]+\)$ ]]; then
    if [[ "$subject" =~ \#([0-9]+) ]]; then
      text="${subject} (#${BASH_REMATCH[1]})"
    elif [[ "$body" =~ \#([0-9]+) ]]; then
      text="${subject} (#${BASH_REMATCH[1]})"
    fi
  fi

  printf -- '- %s' "$text"
}

categorize_commit() {
  local subject="$1"
  local body="$2"
  local entry="$3"

  if [[ "$body" == *"BREAKING CHANGE"* ]] || printf '%s' "$subject" | rg -q '^[[:alpha:]]+(\([^)]+\))?!:'; then
    BREAKING_CHANGES+=("$entry")
  elif printf '%s' "$subject" | rg -q '^feat(\([^)]+\))?:\s'; then
    FEATURES+=("$entry")
  elif printf '%s' "$subject" | rg -q '^fix(\([^)]+\))?:\s'; then
    BUG_FIXES+=("$entry")
  else
    MISC_ITEMS+=("$entry")
  fi
}

while IFS= read -r -d $'\x1e' record; do
  [[ -n "$record" ]] || continue
  _hash="${record%%$'\x1f'*}"
  _rest="${record#*$'\x1f'}"
  _subject="${_rest%%$'\x1f'*}"
  _rest="${_rest#*$'\x1f'}"
  _body="${_rest%%$'\x1f'*}"
  _rest="${_rest#*$'\x1f'}"
  _author="${_rest%%$'\x1f'*}"
  _email="${_rest#*$'\x1f'}"

  [[ -n "${_subject:-}" ]] || continue
  entry="$(format_entry "$_subject" "${_body:-}")"
  categorize_commit "$_subject" "${_body:-}" "$entry"
  add_contributor "${_author:-unknown}" "${_email:-unknown@unknown}"
done < <(
  GIT_LOG_ARGS=(--no-merges --pretty=format:'%H%x1f%s%x1f%b%x1f%an%x1f%ae%x1e' "$LOG_RANGE")
  if [[ "${#LOG_PATH_FILTERS[@]}" -gt 0 ]]; then
    GIT_LOG_ARGS+=(--)
    GIT_LOG_ARGS+=("${LOG_PATH_FILTERS[@]}")
  fi
  git log "${GIT_LOG_ARGS[@]}"
)

render_section() {
  local title="$1"
  shift
  local -a items=("$@")
  local item
  local printed=0
  echo "### ${title}"
  for item in "${items[@]}"; do
    if [[ -n "$item" ]]; then
      echo "$item"
      printed=1
    fi
  done
  if [[ "$printed" -eq 0 ]]; then
    echo "- None"
  fi
  echo
}

NOTES_FILE="$(mktemp -t opensandbox-release-notes.XXXXXX.md)"
{
  echo "# ${DISPLAY_NAME} ${VERSION_LABEL}"
  echo
  echo "## What's New"
  echo
  if [[ -n "$PREVIOUS_TAG" ]]; then
    echo "Changes included since \`${PREVIOUS_TAG}\`."
  else
    echo "Initial release for this target."
  fi
  if [[ "${#LOG_PATH_FILTERS[@]}" -gt 0 ]]; then
    echo "Scoped paths: \`$(printf '%s ' "${LOG_PATH_FILTERS[@]}" | sed 's/[[:space:]]*$//')\`."
  fi
  echo
  render_section "✨ Features" "${FEATURES[@]-}"
  render_section "🐛 Bug Fixes" "${BUG_FIXES[@]-}"
  render_section "⚠️ Breaking Changes" "${BREAKING_CHANGES[@]-}"
  render_section "📦 Misc" "${MISC_ITEMS[@]-}"
  echo "## 👥 Contributors"
  echo
  echo "Thanks to these contributors ❤️"
  echo
  if [[ "$CONTRIBUTORS_INDEX" == $'\n' ]]; then
    echo "- None"
  else
    printf -- '- %s\n' "${CONTRIBUTORS[@]-}"
  fi
} >"$NOTES_FILE"

if [[ "$DRY_RUN" == true ]]; then
  log "Dry run enabled. No tag/release side effects will be performed."
  log "Computed range: ${LOG_RANGE}"
  echo
  log "Generated release notes preview:"
  echo "------------------------------------------------------------"
  cat "$NOTES_FILE"
  echo "------------------------------------------------------------"
  rm -f "$NOTES_FILE"
  exit 0
fi

if git rev-parse -q --verify "refs/tags/${NEW_TAG}" >/dev/null; then
  warn "Tag '${NEW_TAG}' already exists. Reusing existing tag."
else
  if [[ "$SIGN_TAG" == true ]]; then
    git tag -s "$NEW_TAG" -m "release: ${DISPLAY_NAME} ${VERSION_LABEL}"
    log "Created signed tag: ${NEW_TAG}"
  else
    git tag -a "$NEW_TAG" -m "release: ${DISPLAY_NAME} ${VERSION_LABEL}"
    log "Created annotated tag: ${NEW_TAG}"
  fi
fi

if [[ "$PUSH_TAG" == true ]]; then
  git push origin "$NEW_TAG"
  log "Pushed tag to origin: ${NEW_TAG}"
else
  warn "Tag not pushed. Use --push to trigger tag-based publish workflows."
fi

LOCAL_TAG_COMMIT="$(git rev-parse "${NEW_TAG}^{commit}")"
REMOTE_TAG_COMMIT="$(remote_tag_commit "$NEW_TAG")"
if [[ -z "$REMOTE_TAG_COMMIT" ]]; then
  die "Tag '${NEW_TAG}' does not exist on origin. Pass --push or push the tag before creating a GitHub Release."
fi
if [[ "$LOCAL_TAG_COMMIT" != "$REMOTE_TAG_COMMIT" ]]; then
  die "Local tag '${NEW_TAG}' resolves to ${LOCAL_TAG_COMMIT}, but origin resolves to ${REMOTE_TAG_COMMIT}. Refusing to create or update the GitHub Release."
fi

RELEASE_TITLE="${DISPLAY_NAME} ${VERSION_LABEL}"
if gh release view "$NEW_TAG" >/dev/null 2>&1; then
  gh release edit "$NEW_TAG" \
    --title "$RELEASE_TITLE" \
    --notes-file "$NOTES_FILE"
  log "Updated GitHub Release: ${NEW_TAG}"
else
  gh release create "$NEW_TAG" \
    --verify-tag \
    --title "$RELEASE_TITLE" \
    --notes-file "$NOTES_FILE"
  log "Created GitHub Release: ${NEW_TAG}"
fi

rm -f "$NOTES_FILE"
log "Release automation completed."
