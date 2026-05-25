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

TAG=${TAG:-latest}
BUILD_METADATA_FILE=${BUILD_METADATA_FILE:-build/code-interpreter-image-metadata.json}
mkdir -p "$(dirname "${BUILD_METADATA_FILE}")"

docker buildx rm code-interpreter-builder || true

docker buildx create --use --name code-interpreter-builder

docker buildx inspect --bootstrap

docker buildx ls

#docker buildx build -t opensandbox/code-interpreter-base:${TAG} \
#  --platform linux/amd64,linux/arm64 \
#  -f Dockerfile_base \
#  --push \
#  .

docker buildx build \
  -t opensandbox/code-interpreter:${TAG} \
  -t sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/code-interpreter:${TAG} \
  --platform linux/amd64,linux/arm64 \
  --metadata-file "${BUILD_METADATA_FILE}" \
  --push \
  .
