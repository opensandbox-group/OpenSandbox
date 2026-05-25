#!/bin/bash
# Copyright 2025 Alibaba Group Holding Ltd.
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

set -ex

default_build_time() {
  if [[ -n "${SOURCE_DATE_EPOCH:-}" ]]; then
    date -u -d "@${SOURCE_DATE_EPOCH}" +"%Y-%m-%dT%H:%M:%SZ" 2>/dev/null ||
      date -u -r "${SOURCE_DATE_EPOCH}" +"%Y-%m-%dT%H:%M:%SZ"
  else
    date -u +"%Y-%m-%dT%H:%M:%SZ"
  fi
}

build_arg_if_set() {
  local name="$1"
  if [[ -n "${!name+x}" ]]; then
    BUILD_ARGS+=(--build-arg "${name}=${!name}")
  fi
}

TAG=${TAG:-latest}
VERSION=${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo "dev")}
GIT_COMMIT=${GIT_COMMIT:-$(git rev-parse HEAD 2>/dev/null || echo "unknown")}
BUILD_TIME=${BUILD_TIME:-$(default_build_time)}
BUILD_METADATA_FILE=${BUILD_METADATA_FILE:-build/execd-image-metadata.json}
BUILD_ARGS=()
for name in GOFLAGS LDFLAGS CGO_ENABLED CC CXX CFLAGS CXXFLAGS CGO_CFLAGS CGO_CXXFLAGS CGO_LDFLAGS; do
  build_arg_if_set "${name}"
done

REPO_ROOT=$(git rev-parse --show-toplevel 2>/dev/null || realpath "$(dirname "$0")/../..")
cd "${REPO_ROOT}"
mkdir -p "$(dirname "${BUILD_METADATA_FILE}")"

docker buildx rm execd-builder || true

docker buildx create --use --name execd-builder

docker buildx inspect --bootstrap

docker buildx ls

LATEST_TAGS=()
if [[ "${TAG}" == v* ]]; then
  LATEST_TAGS+=(-t opensandbox/execd:latest -t sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/execd:latest)
fi

docker buildx build \
  -t opensandbox/execd:${TAG} \
  -t sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/execd:${TAG} \
  "${LATEST_TAGS[@]}" \
  -f components/execd/Dockerfile \
  "${BUILD_ARGS[@]}" \
  --build-arg VERSION="${VERSION}" \
  --build-arg GIT_COMMIT="${GIT_COMMIT}" \
  --build-arg BUILD_TIME="${BUILD_TIME}" \
  --platform linux/amd64,linux/arm64 \
  --metadata-file "${BUILD_METADATA_FILE}" \
  --push \
  .
