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

set -e

build_arg_if_set() {
    local name="$1"
    if [[ -n "${!name+x}" ]]; then
        BUILD_ARGS+=(--build-arg "${name}=${!name}")
    fi
}

# Default values
TAG=${TAG:-latest}
COMPONENT=${COMPONENT:-controller}
PUSH=${PUSH:-true}
GHCR_REPO=${GHCR_REPO:-}
BUILD_METADATA_FILE=${BUILD_METADATA_FILE:-build/${COMPONENT}-image-metadata.json}
BUILD_ARGS=()
for name in GOFLAGS LDFLAGS CGO_ENABLED CC CXX CFLAGS CXXFLAGS CGO_CFLAGS CGO_CXXFLAGS CGO_LDFLAGS; do
    build_arg_if_set "${name}"
done
BUILD_ARGS+=(--build-arg "COMMIT_ID=$(git rev-parse --short HEAD)")
BUILD_ARGS+=(--build-arg "BUILD_DATE=$(date -u +%Y-%m-%dT%H:%M:%SZ)")
mkdir -p "$(dirname "${BUILD_METADATA_FILE}")"

DOCKERHUB_REPO="opensandbox"
ACR_REPO="sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox"

# Component specific settings
DOCKERFILE="Dockerfile"
if [ "$COMPONENT" == "controller" ]; then
    IMAGE_NAME="controller"
    BUILD_ARG="--build-arg PACKAGE=./cmd/controller"
elif [ "$COMPONENT" == "task-executor" ]; then
    IMAGE_NAME="task-executor"
    BUILD_ARG="--build-arg PACKAGE=cmd/task-executor/main.go --build-arg USERID=0"
elif [ "$COMPONENT" == "image-committer" ]; then
    IMAGE_NAME="image-committer"
    BUILD_ARG=""
    DOCKERFILE="Dockerfile.image-committer"
else
    echo "Error: Unknown component: $COMPONENT"
    echo "Available components: controller, task-executor, image-committer"
    exit 1
fi

echo "========================================="
echo "Building $COMPONENT"
echo "Image: $IMAGE_NAME"
echo "Tag: $TAG"
echo "Push: $PUSH"
echo "========================================="

# Build for multiple platforms
PLATFORMS="linux/amd64,linux/arm64"

if [ "$PUSH" == "true" ]; then
    IMAGE_TAGS=(-t "${DOCKERHUB_REPO}/${IMAGE_NAME}:${TAG}" -t "${ACR_REPO}/${IMAGE_NAME}:${TAG}")
    if [[ -n "${GHCR_REPO}" ]]; then
        IMAGE_TAGS+=(-t "${GHCR_REPO}/${IMAGE_NAME}:${TAG}")
    fi

    # Build and push to registry
    docker buildx build \
        --platform $PLATFORMS \
        $BUILD_ARG \
        "${BUILD_ARGS[@]}" \
        "${IMAGE_TAGS[@]}" \
        --metadata-file "${BUILD_METADATA_FILE}" \
        --push \
        -f "$DOCKERFILE" \
        .
    
    echo "========================================="
    echo "Successfully built and pushed:"
    echo "  ${DOCKERHUB_REPO}/${IMAGE_NAME}:${TAG}"
    echo "  ${ACR_REPO}/${IMAGE_NAME}:${TAG}"
    if [[ -n "${GHCR_REPO}" ]]; then
        echo "  ${GHCR_REPO}/${IMAGE_NAME}:${TAG}"
    fi
    echo "========================================="
else
    # Build only (for local testing)
    docker buildx build \
        --platform linux/amd64 \
        $BUILD_ARG \
        "${BUILD_ARGS[@]}" \
        -t ${IMAGE_NAME}:${TAG} \
        -f "$DOCKERFILE" \
        --load \
        .
    
    echo "========================================="
    echo "Successfully built (local only):"
    echo "  ${IMAGE_NAME}:${TAG}"
    echo "========================================="
fi
