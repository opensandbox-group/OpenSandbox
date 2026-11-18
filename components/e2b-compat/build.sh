#!/bin/bash
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

# Build a local host binary. Unlike components/ingress/build.sh, this script
# never builds or pushes container images: the compatibility component is a
# scaffold with no published image.

set -ex

default_build_time() {
  if [[ -n "${SOURCE_DATE_EPOCH:-}" ]]; then
    date -u -d "@${SOURCE_DATE_EPOCH}" +"%Y-%m-%dT%H:%M:%SZ" 2>/dev/null ||
      date -u -r "${SOURCE_DATE_EPOCH}" +"%Y-%m-%dT%H:%M:%SZ"
  else
    date -u +"%Y-%m-%dT%H:%M:%SZ"
  fi
}

VERSION=${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo "dev")}
GIT_COMMIT=${GIT_COMMIT:-$(git rev-parse HEAD 2>/dev/null || echo "unknown")}
BUILD_TIME=${BUILD_TIME:-$(default_build_time)}

cd "$(dirname "$0")"
mkdir -p bin

go build -trimpath -buildvcs=false \
  -ldflags "-buildid= -B none \
    -X 'github.com/alibaba/opensandbox/internal/version.Version=${VERSION}' \
    -X 'github.com/alibaba/opensandbox/internal/version.BuildTime=${BUILD_TIME}' \
    -X 'github.com/alibaba/opensandbox/internal/version.GitCommit=${GIT_COMMIT}'" \
  -o bin/e2b-compat main.go
