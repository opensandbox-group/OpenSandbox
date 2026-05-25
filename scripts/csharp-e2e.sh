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

set -euxo pipefail

TAG=${TAG:-latest}
RUN_CODE_INTERPRETER_E2E=${RUN_CODE_INTERPRETER_E2E:-false}

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"

# build execd image locally (context must include internal/)
docker build -f components/execd/Dockerfile -t opensandbox/execd:local "${REPO_ROOT}"

# prepare required images from registry
docker pull opensandbox/code-interpreter:${TAG}
echo "-------- Eval test images --------"
docker images

# prepare hostpath volume for e2e test
mkdir -p /tmp/opensandbox-e2e/host-volume-test
mkdir -p /tmp/opensandbox-e2e/logs
echo "opensandbox-e2e-marker" > /tmp/opensandbox-e2e/host-volume-test/marker.txt
chmod -R 755 /tmp/opensandbox-e2e

# prepare Docker named volume for pvc e2e test
docker volume rm opensandbox-e2e-pvc-test 2>/dev/null || true
docker volume create opensandbox-e2e-pvc-test
docker run --rm -v opensandbox-e2e-pvc-test:/data alpine sh -c "\
  echo 'pvc-marker-data' > /data/marker.txt && \
  mkdir -p /data/datasets/train && \
  echo 'pvc-subpath-marker' > /data/datasets/train/marker.txt"
echo "-------- CSHARP E2E test logs for execd --------" > /tmp/opensandbox-e2e/logs/execd.log

# setup server
cd server
: > server.log
export OPENSANDBOX_INSECURE_SERVER=YES
(uv sync && uv run python -m opensandbox_server.main) > server.log 2>&1 &
cd ..

# wait for server
sleep 10

# test env for C# fixture
export OPENSANDBOX_TEST_DOMAIN="localhost:8080"
export OPENSANDBOX_TEST_PROTOCOL="http"
export OPENSANDBOX_TEST_API_KEY=""
export OPENSANDBOX_SANDBOX_DEFAULT_IMAGE="opensandbox/code-interpreter:${TAG}"

mkdir -p tests/csharp/build/test-results
dotnet restore "tests/csharp/OpenSandbox.E2ETests/OpenSandbox.E2ETests.csproj"
DOTNET_TEST_FILTER=()
if [ "${RUN_CODE_INTERPRETER_E2E}" != "true" ]; then
  DOTNET_TEST_FILTER=(--filter "FullyQualifiedName!~CodeInterpreterE2ETests")
fi
dotnet test "tests/csharp/OpenSandbox.E2ETests/OpenSandbox.E2ETests.csproj" \
  --configuration Release \
  --no-restore \
  --results-directory "tests/csharp/build/test-results" \
  --logger "trx;LogFileName=csharp-e2e.trx" \
  "${DOTNET_TEST_FILTER[@]}"
