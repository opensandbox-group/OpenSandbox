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

# Bump hand-maintained SDK identity versions to the umbrella version.
#
# Every SDK hardcodes its default User-Agent string (and the Go SDK its
# Version constant) in source, and a per-language regression test pins the
# exact value. Both sides must move together with the umbrella version or
# the identity reported to the server drifts from the published package.
# This script rewrites each source/test pair in one shot:
#
#   python  OpenSandbox-Python-SDK/<v>   config/connection{,_sync}.py + test
#   js      OpenSandbox-JS-SDK/<v>       core/constants.ts + test
#   kotlin  OpenSandbox-Kotlin-SDK/<v>   ConnectionConfig.kt + test
#   csharp  OpenSandbox-CSharp-SDK/<v>   Constants.cs + test
#   go      Version = "<v>"              constants.go + constants_test.go
#
# code-interpreter (python/js) and the CLI reuse these constants, so no
# separate entries are needed.
#
# Called automatically by create-umbrella-release.sh --bump-only; can also
# run standalone. It only rewrites files — committing stays with the caller.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

usage() {
  cat <<'EOF'
Usage:
  manifests/release/bump-sdk-identity.sh --version <X.Y.Z[-rc.N]> [--dry-run]

Required:
  --version <version>   Umbrella version, e.g. 1.1.0 or 1.1.0-rc.1.

Options:
  --dry-run             Print the current values and exit without rewriting.
  --help                Show this help.
EOF
}

log() { echo "[sdk-identity] $*"; }
die() { echo "[sdk-identity][error] $*" >&2; exit 1; }

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

cd "$SCRIPT_DIR/../.."

# file|default-User-Agent product prefix; the sed rule rewrites any semver
# (including .dev0 / -rc.N suffixes) after the prefix to the target version.
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
)

# file|<literal>"<semver>" pattern (Go Version constant + pinned test want)
GO_SPECS=(
  "sdks/sandbox/go/constants.go|Version = "
  "sdks/sandbox/go/constants_test.go|want = "
)

for spec in "${UA_SPECS[@]}"; do
  f="${spec%%|*}"
  product="${spec#*|}"
  [[ -f "$f" ]] || die "Missing target file: $f"
  if [[ "$DRY_RUN" == true ]]; then
    current="$(grep -hoE "${product}/[0-9][0-9A-Za-z.+-]*" "$f" | sort -u | tr '\n' ' ')"
    log "dry-run ${f}: ${current}-> ${product}/${VERSION}"
    continue
  fi
  sed -i.bak -E "s|(${product}/)[0-9][0-9A-Za-z.+-]*|\1${VERSION}|g" "$f" && rm -f "${f}.bak"
  grep -q "${product}/${VERSION}" "$f" || die "Failed to bump ${product} identity in ${f}"
  log "bumped ${f} -> ${product}/${VERSION}"
done

for spec in "${GO_SPECS[@]}"; do
  f="${spec%%|*}"
  pattern="${spec#*|}"
  [[ -f "$f" ]] || die "Missing target file: $f"
  if [[ "$DRY_RUN" == true ]]; then
    current="$(grep -hoE "${pattern}\"[0-9][0-9A-Za-z.+-]*\"" "$f" | sort -u | tr '\n' ' ')"
    log "dry-run ${f}: ${current}-> ${pattern}\"${VERSION}\""
    continue
  fi
  sed -i.bak -E "s|(${pattern}\")[0-9][0-9A-Za-z.+-]*(\")|\1${VERSION}\2|" "$f" && rm -f "${f}.bak"
  grep -q "${pattern}\"${VERSION}\"" "$f" || die "Failed to bump ${pattern% = *} constant in ${f}"
  log "bumped ${f} -> ${pattern}\"${VERSION}\""
done

log "SDK identity versions aligned to ${VERSION}."
