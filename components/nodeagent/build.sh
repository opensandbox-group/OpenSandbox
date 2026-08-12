#!/bin/bash
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
    build_args+=(--build-arg "${name}=${!name}")
  fi
}

TAG=${TAG:-latest}
GHCR_REPO=${GHCR_REPO:-}
VERSION=${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}
GIT_COMMIT=${GIT_COMMIT:-$(git rev-parse HEAD 2>/dev/null || echo unknown)}
BUILD_TIME=${BUILD_TIME:-$(default_build_time)}
BUILD_METADATA_FILE=${BUILD_METADATA_FILE:-build/nodeagent-image-metadata.json}
build_args=()
for name in GOFLAGS LDFLAGS; do
  build_arg_if_set "${name}"
done
REPO_ROOT=$(git rev-parse --show-toplevel 2>/dev/null || realpath "$(dirname "$0")/../..")
cd "$REPO_ROOT"
mkdir -p "$(dirname "$BUILD_METADATA_FILE")"

image_tags=(
  -t "opensandbox/nodeagent:${TAG}"
  -t "sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/nodeagent:${TAG}"
)
if [[ -n "$GHCR_REPO" ]]; then image_tags+=(-t "${GHCR_REPO}/nodeagent:${TAG}"); fi
if [[ "$TAG" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  image_tags+=(
    -t "opensandbox/nodeagent:latest"
    -t "sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/nodeagent:latest"
  )
  if [[ -n "$GHCR_REPO" ]]; then image_tags+=(-t "${GHCR_REPO}/nodeagent:latest"); fi
fi

builder_name="nodeagent-builder-$$-${RANDOM}"
builder_created=false
cleanup_builder() {
  if [[ "$builder_created" == true ]]; then
    docker buildx rm "$builder_name" >/dev/null 2>&1 || true
  fi
}
trap cleanup_builder EXIT

docker buildx create --name "$builder_name"
builder_created=true
docker buildx inspect "$builder_name" --bootstrap

docker buildx build \
  --builder "$builder_name" \
  "${image_tags[@]}" \
  -f components/nodeagent/Dockerfile \
  ${build_args[@]+"${build_args[@]}"} \
  --build-arg VERSION="$VERSION" \
  --build-arg GIT_COMMIT="$GIT_COMMIT" \
  --build-arg BUILD_TIME="$BUILD_TIME" \
  --platform linux/amd64,linux/arm64 \
  --metadata-file "$BUILD_METADATA_FILE" \
  --push \
  .
