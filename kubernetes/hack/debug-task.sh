#!/bin/bash

# Copyright 2025 The OpenSandbox Authors
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

echo "Stopping any running debug containers..."
docker stop task-executor-debug > /dev/null || true
echo "Building debug docker image (dev environment)..."
docker build -t task-executor-debug -f Dockerfile.debug .

echo "Starting debug container with Auto-Sync and Hot-Reload..."
echo "---------------------------------------------------------"
echo "  App URL:      http://localhost:8080"
echo "  Debugger:     localhost:2345"
echo "  Source Code:  Mounted from $(pwd)"
echo "---------------------------------------------------------"
echo "Usage:"
echo "  1. Connect GoLand to localhost:2345"
echo "  2. Edit code locally -> Container auto-recompiles (watch the logs)"
echo "  3. Re-connect Debugger in GoLand"
echo "---------------------------------------------------------"

# Create docker volumes for cache if they don't exist
docker volume create sandbox-k8s-gomod > /dev/null
docker volume create sandbox-k8s-gocache > /dev/null

# Run the container
# --rm: remove container after exit
# -v $(pwd):/workspace: Mount local code
# -v ...: Mount caches for speed
# reflex command:
#   -r '\.go$': Watch all .go files recursively
#   -s: Service mode (kill old process before starting new one)
#   --: Delimiter
#   dlv debug: Compile and run ./cmd/task
#     --headless: No terminal UI
#     --listen=:2345: Debugger port
#     --api-version=2: API v2
#     --accept-multiclient: Allow multiple connections
#     --continue: Start running immediately (Optional, remove if you want to hit 'Resume' first)
#     --output /tmp/debug_bin: Put binary in tmp to avoid clutter/loops

docker run --rm -it \
  --privileged \
  -p 5758:5758 \
  -p 2345:2345 \
  --security-opt seccomp=unconfined \
  --cap-add=SYS_PTRACE \
  -v "$(pwd):/workspace" \
  -v sandbox-k8s-gomod:/go/pkg/mod \
  -v sandbox-k8s-gocache:/go/.cache/go-build \
  --name task-executor-debug \
  -e SANDBOX_MAIN_CONTAINER=task-executor \
  task-executor-debug \
  reflex -r '\.go$' -s -- \
    dlv debug ./cmd/task-executor \
    --headless \
    --listen=:2345 \
    --api-version=2 \
    --accept-multiclient \
    --output /tmp/debug_bin \
    -- \
    -enable-sidecar-mode=true -main-container-name=task-executor