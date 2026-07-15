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

set -ex

TAG=${TAG:-latest}
GHCR_REPO=${GHCR_REPO:-}
BUILD_METADATA_FILE=${BUILD_METADATA_FILE:-build/server-image-metadata.json}
mkdir -p "$(dirname "${BUILD_METADATA_FILE}")"

docker buildx rm server-builder || true

docker buildx create --use --name server-builder

docker buildx inspect --bootstrap

docker buildx ls

IMAGE_TAGS=(-t opensandbox/server:${TAG} -t sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/server:${TAG})
LATEST_TAGS=()
if [[ -n "${GHCR_REPO}" ]]; then
  IMAGE_TAGS+=(-t "${GHCR_REPO}/server:${TAG}")
fi
if [[ "${TAG}" == v* ]]; then
  LATEST_TAGS+=(-t opensandbox/server:latest -t sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/server:latest)
  if [[ -n "${GHCR_REPO}" ]]; then
    LATEST_TAGS+=(-t "${GHCR_REPO}/server:latest")
  fi
fi

docker buildx build \
  "${IMAGE_TAGS[@]}" \
  "${LATEST_TAGS[@]}" \
  --platform linux/amd64,linux/arm64 \
  --metadata-file "${BUILD_METADATA_FILE}" \
  --push \
  .
