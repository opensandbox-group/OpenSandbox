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

# One-shot platform version bump — the machine-deterministic part of an
# umbrella release prep (OSEP-0016). Rewrites every version carrier to the
# target umbrella version and commits the result:
#
#   1-2. chart version/appVersion + umbrella chart dependencies
#   3.   chart image references -> :release-<v>
#   4.   umbrella Chart.lock regeneration
#   5.   chart README regeneration (helm-docs)
#   6-10. SDK/CLI package versions, dependency ranges, identity constants
#        (stable channel only — rc ships no SDK artifacts, so the SDK tree
#        stays at the last published line)
#
# Called by create-umbrella-release.sh --bump-only; can also run standalone.
# The hand-authored release notes are NOT touched by this script.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

usage() {
  cat <<'EOF'
Usage:
  manifests/release/bump-versions.sh --version <X.Y.Z[-rc.N]> [--dry-run]

Required:
  --version <version>   Umbrella version, e.g. 1.1.0 or 1.1.0-rc.1.

Options:
  --dry-run             Print the plan and current values; rewrite nothing.
  --help                Show this help.
EOF
}

log() { echo "[bump] $*"; }
warn() { echo "[bump][warn] $*" >&2; }
die() { echo "[bump][error] $*" >&2; exit 1; }

VERSION=""
DRY_RUN=false

while [[ $# -gt 0 ]]; do
  case "$1" in
    --version) [[ $# -ge 2 ]] || die "--version requires a value"; VERSION="$2"; shift 2 ;;
    --dry-run) DRY_RUN=true; shift ;;
    --help|-h) usage; exit 0 ;;
    *) die "Unknown argument: $1" ;;
  esac
done

[[ -n "$VERSION" ]] || { usage; echo; die "--version is required"; }
VERSION="${VERSION#v}"
[[ "$VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$ ]] \
  || die "Version '${VERSION}' is not semver (X.Y.Z[-rc.N])"

MAJOR="${VERSION%%.*}"
NEXT_MAJOR=$((MAJOR + 1))

cd "$REPO_ROOT"
git rev-parse --is-inside-work-tree >/dev/null 2>&1 || die "Must run inside a git repository"

if [[ "$DRY_RUN" != true ]]; then
  git diff-index --quiet HEAD -- || die "Worktree not clean. Commit or stash before bumping."
fi

if [[ "$DRY_RUN" == true ]]; then
  log "Bumping platform version to ${VERSION} (dry run, nothing will be rewritten or committed)"
else
  log "Bumping platform version to ${VERSION}"
fi

# 1) Chart.yaml top-level versions + appVersion (where present)
for chart in base controller server ingress-gateway node-agent fast-sandbox opensandbox; do
  f="manifests/charts/${chart}/Chart.yaml"
  [[ -f "$f" ]] || { warn "missing ${f}; skipped"; continue; }
  if [[ "$DRY_RUN" == true ]]; then
    log "dry-run ${f}: $(sed -n 's/^version:[[:space:]]*//p' "$f" | head -1) -> ${VERSION}"
    continue
  fi
  awk -v v="$VERSION" '
    /^dependencies:/ { deps = 1 }
    !deps && /^appVersion:/ { print "appVersion: \"" v "\""; next }
    !deps && /^version:/ { print "version: " v; next }
    { print }' "$f" > "${f}.tmp" && mv "${f}.tmp" "$f"
done

if [[ "$DRY_RUN" == true ]]; then
  # 2) Umbrella appVersion + dependency versions
  f="manifests/charts/opensandbox/Chart.yaml"
  bad_deps="$(sed -n '/^dependencies:/,$p' "$f" | awk -v want="${VERSION}" '
    /^[[:space:]]+- name:/ { name=$NF }
    /^[[:space:]]+version:/ {
      v=$2; gsub(/"/, "", v); gsub(/'\''/, "", v)
      if (v != want) print name " -> " v
    }')"
  if [[ -n "$bad_deps" ]]; then
    while IFS= read -r line; do log "dry-run ${f}: dependency ${line} (want ${VERSION})"; done <<<"$bad_deps"
  fi

  # 3) Image references in chart values: pinned split tags and full-image strings
  while IFS= read -r -d '' f; do
    bad="$(grep -rohE '(opensandbox|fast-sandbox)/[A-Za-z0-9._/-]+:[^"'"'"'[:space:]]+' "$f" 2>/dev/null \
      | grep -vE ":release-${VERSION}$" | sort -u || true)"
    [[ -n "$bad" ]] && log "dry-run ${f}: image refs -> release-${VERSION}: ${bad//$'\n'/, }"
  done < <(find manifests/charts -name 'values*.yaml' -print0)
else
  # 2) Umbrella appVersion + dependency versions
  f="manifests/charts/opensandbox/Chart.yaml"
  awk -v v="$VERSION" '
    /^appVersion:/ { print "appVersion: \"" v "\""; next }
    /^dependencies:/ { deps = 1 }
    deps && /^([[:space:]])+version:/ {
      match($0, /^[[:space:]]+/)
      print substr($0, 1, RLENGTH) "version: \"" v "\""
      next
    }
    { print }' "$f" > "${f}.tmp" && mv "${f}.tmp" "$f"

  # 3) Image references in chart values: pinned split tags and full-image strings.
  #    Rewrites any previous channel tag (legacy vX.Y.Z, dev, latest, or a
  #    release-X.Y.Z[-rc.N] left by an earlier umbrella bump) to release-VERSION.
  while IFS= read -r -d '' f; do
    sed -E \
      -e 's,((opensandbox|fast-sandbox)/[A-Za-z0-9._/-]+):(v[0-9][^"[:space:]]*|release-[0-9][^"[:space:]]*|dev|latest),\1:release-'"${VERSION}"',g' \
      -e 's,^([[:space:]]*tag:[[:space:]]*")(v[0-9][^"]*|release-[0-9][^"]*|dev|latest)("),\1release-'"${VERSION}"'\3,' \
      -e 's,^([[:space:]]*tag:[[:space:]]*)(v[0-9][^"[:space:]]*|release-[0-9][^"[:space:]]*|dev|latest)$,\1release-'"${VERSION}"',' \
      "$f" > "${f}.tmp" && mv "${f}.tmp" "$f"
  done < <(find manifests/charts -name 'values*.yaml' -print0)
fi

# 4) Umbrella Chart.lock must follow the dependency bumps: packaged sub-charts
# are not committed, and CI runs `helm dependency build`, which rejects a lock
# that is out of sync with Chart.yaml.
LOCK_FILE="manifests/charts/opensandbox/Chart.lock"
stale_lock="$(sed -n '/^dependencies:/,$p' "$LOCK_FILE" | awk -v want="${VERSION}" '
  /^[[:space:]]+- name:/ { name=$NF }
  /^[[:space:]]+version:/ {
    v=$2; gsub(/"/, "", v)
    if (v != want) print name " -> " v
  }')"
if [[ "$DRY_RUN" == true ]]; then
  [[ -n "$stale_lock" ]] && log "dry-run ${LOCK_FILE}: regenerate (stale: ${stale_lock//$'\n'/, })"
elif command -v helm >/dev/null 2>&1; then
  lock_before="$(mktemp -t lock.XXXXXX)"
  grep -v '^generated:' "$LOCK_FILE" >"$lock_before"
  helm dependency update manifests/charts/opensandbox >/dev/null
  # keep the run idempotent: a re-bump only refreshes the generated timestamp
  if diff -q "$lock_before" <(grep -v '^generated:' "$LOCK_FILE") >/dev/null; then
    git checkout -- "$LOCK_FILE"
  fi
  rm -f "$lock_before"
  log "regenerated ${LOCK_FILE} (or confirmed in sync)"
else
  [[ -z "$stale_lock" ]] || die "helm not found and ${LOCK_FILE} is stale (${stale_lock//$'\n'/, }). Install helm or run 'helm dependency update manifests/charts/opensandbox' manually."
fi

# 5) Regenerate chart documentation so values tables and version badges
#    follow the bump (the CI helm-docs drift check fails otherwise).
if [[ "$DRY_RUN" == true ]]; then
  log "dry-run manifests/charts/*/README.md: regenerate via 'make helm-docs' in manifests/ when values drift"
elif command -v helm-docs >/dev/null 2>&1; then
  (cd manifests && helm-docs --chart-search-root charts/ >/dev/null)
  log "regenerated chart README files"
else
  warn "helm-docs not found; chart README files may be stale and the CI helm-docs check will fail. Run 'make helm-docs' in manifests/ manually."
fi

# 6-10) SDK/CLI package versions, dependency ranges, and identity constants.
# These only move at the stable bump: rc releases ship no SDK artifacts, so
# the SDK tree stays at the last published line (dependency ranges keep
# referencing published versions instead of an rc version that never ships).
if [[ "$VERSION" == *-* ]]; then
  log "channel=rc: SDK/CLI package versions, dependency ranges, and identity constants untouched (they bump at the stable release)"
else
# 6) JS SDK package versions (top-level field)
for f in sdks/sandbox/javascript/package.json sdks/code-interpreter/javascript/package.json; do
  [[ -f "$f" ]] || { warn "missing ${f}; skipped"; continue; }
  if [[ "$DRY_RUN" == true ]]; then
    log "dry-run ${f}: $(sed -n 's/.*"version":[[:space:]]*"\([^"]*\)".*/\1/p' "$f" | head -1) -> ${VERSION}"
    continue
  fi
  awk -v v="$VERSION" '
    !done && /"version":/ { sub(/"version":[[:space:]]*"[^"]*"/, "\"version\": \"" v "\""); done = 1 }
    { print }' "$f" > "${f}.tmp" && mv "${f}.tmp" "$f"
done

# 7) Kotlin/JVM project version
f="sdks/sandbox/kotlin/gradle.properties"
if [[ "$DRY_RUN" == true ]]; then
  log "dry-run ${f}: $(sed -n 's/^project\.version=//p' "$f" | head -1) -> ${VERSION}"
elif [[ -f "$f" ]]; then
  sed -i.bak "s/^project\.version=.*/project.version=${VERSION}/" "$f" && rm -f "${f}.bak"
fi

# 8) .NET package versions + dependency range
f="sdks/Directory.Build.props"
if [[ -f "$f" ]]; then
  if [[ "$DRY_RUN" == true ]]; then
    log "dry-run ${f}: $(sed -n 's:.*<OpenSandboxPackageVersion>\(.*\)</OpenSandboxPackageVersion>.*:\1:p' "$f" | head -1) -> ${VERSION}"
  else
    sed -i.bak \
      -e "s|<OpenSandboxPackageVersion>[^<]*</OpenSandboxPackageVersion>|<OpenSandboxPackageVersion>${VERSION}</OpenSandboxPackageVersion>|" \
      -e "s|<OpenSandboxCodeInterpreterPackageVersion>[^<]*</OpenSandboxCodeInterpreterPackageVersion>|<OpenSandboxCodeInterpreterPackageVersion>${VERSION}</OpenSandboxCodeInterpreterPackageVersion>|" \
      -e "s|<OpenSandboxDependencyVersionRange>[^<]*</OpenSandboxDependencyVersionRange>|<OpenSandboxDependencyVersionRange>[${VERSION},${NEXT_MAJOR}.0.0)</OpenSandboxDependencyVersionRange>|" \
      "$f" && rm -f "${f}.bak"
  fi
fi

# 9) Python inter-package dependency ranges (cli + sdks)
for f in cli/pyproject.toml sdks/code-interpreter/python/pyproject.toml sdks/mcp/sandbox/python/pyproject.toml; do
  [[ -f "$f" ]] || continue
  if [[ "$DRY_RUN" == true ]]; then
    grep -hoE "\"opensandbox>=[^\"]*\"" "$f" | sort -u | while IFS= read -r cur; do
      log "dry-run ${f}: ${cur} -> \"opensandbox>=${VERSION},<${NEXT_MAJOR}.0.0\""
    done
    continue
  fi
  sed -i.bak -E "s|\"opensandbox>=[^\"]*\"|\"opensandbox>=${VERSION},<${NEXT_MAJOR}.0.0\"|g" "$f" && rm -f "${f}.bak"
done

# 10) SDK identity versions: default User-Agent strings + Go Version constant,
#    each together with the regression test that pins the exact value.
# file|<product prefix> — any semver after the prefix is rewritten.
UA_SPECS=(
  "sdks/sandbox/python/src/opensandbox/config/connection.py|OpenSandbox-Python-SDK"
  "sdks/sandbox/python/src/opensandbox/config/connection_sync.py|OpenSandbox-Python-SDK"
  "sdks/sandbox/python/tests/test_connection_config.py|OpenSandbox-Python-SDK"
  "sdks/sandbox/javascript/src/core/constants.ts|OpenSandbox-JS-SDK"
  "sdks/sandbox/javascript/tests/connection.test.mjs|OpenSandbox-JS-SDK"
  "sdks/sandbox/kotlin/sandbox/src/main/kotlin/com/alibaba/opensandbox/sandbox/config/ConnectionConfig.kt|OpenSandbox-Kotlin-SDK"
  "sdks/sandbox/kotlin/sandbox/src/test/kotlin/com/alibaba/opensandbox/sandbox/config/ConnectionConfigUserAgentTest.kt|OpenSandbox-Kotlin-SDK"
  "sdks/sandbox/csharp/src/OpenSandbox/Core/Constants.cs|OpenSandbox-CSharp-SDK"
  "sdks/sandbox/csharp/tests/OpenSandbox.Tests/ConstantsTests.cs|OpenSandbox-CSharp-SDK"
  # file|<literal>"<semver>" pattern (Go Version constant + pinned test want)
  "sdks/sandbox/go/constants.go|Version = "
  "sdks/sandbox/go/constants_test.go|want = "
)
for spec in "${UA_SPECS[@]}"; do
  f="${spec%%|*}"
  pattern="${spec#*|}"
  [[ -f "$f" ]] || die "Missing target file: $f"
  if [[ "$DRY_RUN" == true ]]; then
    if [[ "$pattern" == *" = " ]]; then
      current="$(grep -hoE "${pattern}\"[0-9][0-9A-Za-z.+-]*\"" "$f" | sort -u | tr '\n' ' ')"
      log "dry-run ${f}: ${current}-> ${pattern}\"${VERSION}\""
    else
      current="$(grep -hoE "${pattern}/[0-9][0-9A-Za-z.+-]*" "$f" | sort -u | tr '\n' ' ')"
      log "dry-run ${f}: ${current}-> ${pattern}/${VERSION}"
    fi
    continue
  fi
  if [[ "$pattern" == *" = " ]]; then
    sed -i.bak -E "s|(${pattern}\")[0-9][0-9A-Za-z.+-]*(\")|\1${VERSION}\2|" "$f" && rm -f "${f}.bak"
    grep -qF "${pattern}\"${VERSION}\"" "$f" || die "Failed to bump ${pattern% = } constant in ${f}"
    log "bumped ${f} -> ${pattern}\"${VERSION}\""
  else
    sed -i.bak -E "s|(${pattern}/)[0-9][0-9A-Za-z.+-]*|\1${VERSION}|g" "$f" && rm -f "${f}.bak"
    grep -qF "${pattern}/${VERSION}" "$f" || die "Failed to bump ${pattern} identity in ${f}"
    log "bumped ${f} -> ${pattern}/${VERSION}"
  fi
done
fi

if [[ "$DRY_RUN" == true ]]; then
  log "Dry run complete; nothing was rewritten or committed."
  exit 0
fi

git add manifests/charts sdks cli
if git diff --cached --quiet; then
  warn "Nothing to bump; already at ${VERSION}."
else
  git commit -m "release(opensandbox): bump platform version to ${VERSION}"
  log "Bump commit created for ${VERSION}."
fi
log "Next: copy docs/releases/TEMPLATE.md to docs/releases/${VERSION}.md, fill it in, commit, then run the full release (drop --bump-only)."
